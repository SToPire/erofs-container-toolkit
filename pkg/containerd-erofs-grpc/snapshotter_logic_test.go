package containerderofsgrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/erofs/erofs-container-toolkit/pkg/converter"
)

func TestResolveProxyTargetUsesManifestAnnotation(t *testing.T) {
	manifestDigest := digest.FromString("manifest")
	sn := &Snapshotter{}

	gotDigest, gotIndex, handled, err := sn.resolveProxyTarget(context.Background(), "", map[string]string{
		converter.EROFSManifestAnnotation: manifestDigest.String(),
	})
	if err != nil {
		t.Fatalf("resolve proxy target: %v", err)
	}
	if !handled {
		t.Fatalf("expected proxy target to be handled")
	}
	if gotDigest != manifestDigest {
		t.Fatalf("unexpected manifest digest %q", gotDigest)
	}
	if gotIndex != 0 {
		t.Fatalf("unexpected layer index %d", gotIndex)
	}
}

func TestResolveProxyTargetFallsBackWithoutParentMetadata(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		base   snapshots.Snapshotter
	}{
		{
			name:   "empty parent",
			parent: "",
		},
		{
			name:   "parent not found",
			parent: "missing-parent",
			base:   &statOnlySnapshotter{},
		},
		{
			name:   "parent without proxy labels",
			parent: "plain-parent",
			base: &statOnlySnapshotter{infos: map[string]snapshots.Info{
				"plain-parent": {Labels: map[string]string{}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sn := &Snapshotter{base: tt.base}
			gotDigest, gotIndex, handled, err := sn.resolveProxyTarget(context.Background(), tt.parent, map[string]string{})
			if err != nil {
				t.Fatalf("resolve proxy target: %v", err)
			}
			if handled {
				t.Fatalf("expected request to fall back, got digest=%q index=%d", gotDigest, gotIndex)
			}
		})
	}
}

func TestResolveProxyTargetUsesParentMetadata(t *testing.T) {
	manifestDigest := digest.FromString("manifest")
	sn := &Snapshotter{
		base: &statOnlySnapshotter{infos: map[string]snapshots.Info{
			"parent": {
				Labels: map[string]string{
					LabelProxyManifestDigest: manifestDigest.String(),
					LabelProxyLayerIndex:     "2",
				},
			},
		}},
	}

	gotDigest, gotIndex, handled, err := sn.resolveProxyTarget(context.Background(), "parent", map[string]string{})
	if err != nil {
		t.Fatalf("resolve proxy target: %v", err)
	}
	if !handled {
		t.Fatalf("expected request to be handled")
	}
	if gotDigest != manifestDigest {
		t.Fatalf("unexpected manifest digest %q", gotDigest)
	}
	if gotIndex != 3 {
		t.Fatalf("unexpected next layer index %d", gotIndex)
	}
}

func TestResolveProxyTargetRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "invalid annotation digest",
			labels: map[string]string{
				converter.EROFSManifestAnnotation: "not-a-digest",
			},
			want: "parse " + converter.EROFSManifestAnnotation,
		},
		{
			name: "invalid parent manifest digest",
			labels: map[string]string{
				LabelProxyManifestDigest: "not-a-digest",
				LabelProxyLayerIndex:     "1",
			},
			want: "parse parent proxy manifest digest",
		},
		{
			name: "invalid parent layer index",
			labels: map[string]string{
				LabelProxyManifestDigest: digest.FromString("manifest").String(),
				LabelProxyLayerIndex:     "NaN",
			},
			want: "parse parent proxy layer index",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sn := &Snapshotter{
				base: &statOnlySnapshotter{infos: map[string]snapshots.Info{
					"parent": {Labels: tt.labels},
				}},
			}

			parent := "parent"
			if tt.name == "invalid annotation digest" {
				parent = ""
			}

			_, _, _, err := sn.resolveProxyTarget(context.Background(), parent, tt.labels)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestValidateAndSelectLayer(t *testing.T) {
	layer0 := ocispec.Descriptor{MediaType: converter.EROFSLayerMediaType, Digest: digest.FromString("layer-0")}
	layer1 := ocispec.Descriptor{MediaType: converter.EROFSLayerMediaType, Digest: digest.FromString("layer-1")}
	manifest := &ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layer0, layer1},
	}

	layer, err := validateAndSelectLayer(manifest, 1)
	if err != nil {
		t.Fatalf("validate and select layer: %v", err)
	}
	if layer.Digest != layer1.Digest {
		t.Fatalf("unexpected layer digest %q", layer.Digest)
	}
}

func TestValidateAndSelectLayerRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		manifest *ocispec.Manifest
		index    int
		want     string
	}{
		{
			name: "wrong manifest media type",
			manifest: &ocispec.Manifest{
				MediaType: ocispec.MediaTypeImageIndex,
			},
			index: 0,
			want:  "must be " + ocispec.MediaTypeImageManifest,
		},
		{
			name: "layer index out of range",
			manifest: &ocispec.Manifest{
				MediaType: ocispec.MediaTypeImageManifest,
				Layers: []ocispec.Descriptor{
					{MediaType: converter.EROFSLayerMediaType},
				},
			},
			index: 1,
			want:  "out of range",
		},
		{
			name: "unsupported layer media type",
			manifest: &ocispec.Manifest{
				MediaType: ocispec.MediaTypeImageManifest,
				Layers: []ocispec.Descriptor{
					{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"},
				},
			},
			index: 0,
			want:  "unsupported media type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateAndSelectLayer(tt.manifest, tt.index)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestSnapshotIDFromMounts(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name   string
		mounts []mount.Mount
		want   string
	}{
		{
			name: "source path",
			mounts: []mount.Mount{
				{Source: root + "/snapshots/101/fs"},
			},
			want: "101",
		},
		{
			name: "upperdir option",
			mounts: []mount.Mount{
				{Options: []string{
					"lowerdir=/tmp/lower",
					"upperdir=" + root + "/snapshots/202/fs",
				}},
			},
			want: "202",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapshotIDFromMounts(root, tt.mounts)
			if err != nil {
				t.Fatalf("snapshotIDFromMounts: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected snapshot id %q", got)
			}
		})
	}
}

func TestSnapshotIDHelpersRejectInvalidPaths(t *testing.T) {
	root := t.TempDir()
	snapshotRoot := root + "/snapshots/"

	_, err := snapshotIDFromMounts(root, []mount.Mount{{Source: "/tmp/not-a-snapshot"}})
	if err == nil || !strings.Contains(err.Error(), "could not determine snapshot id") {
		t.Fatalf("expected mount parse error, got %v", err)
	}

	_, err = snapshotIDFromPath(snapshotRoot, snapshotRoot)
	if err == nil || !strings.Contains(err.Error(), "invalid snapshot path") {
		t.Fatalf("expected invalid path error, got %v", err)
	}
}

type statOnlySnapshotter struct {
	infos map[string]snapshots.Info
}

func (s *statOnlySnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	if s != nil && s.infos != nil {
		if info, ok := s.infos[key]; ok {
			return info, nil
		}
	}
	return snapshots.Info{}, errdefs.ErrNotFound
}

func (s *statOnlySnapshotter) Update(context.Context, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Usage(context.Context, string) (snapshots.Usage, error) {
	return snapshots.Usage{}, errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Mounts(context.Context, string) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) View(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Commit(context.Context, string, string, ...snapshots.Opt) error {
	return errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Remove(context.Context, string) error {
	return errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Walk(context.Context, snapshots.WalkFunc, ...string) error {
	return errdefs.ErrNotImplemented
}

func (s *statOnlySnapshotter) Close() error {
	return nil
}
