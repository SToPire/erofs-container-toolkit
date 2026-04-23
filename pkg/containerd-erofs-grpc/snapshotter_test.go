package containerderofsgrpc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	erofssnapshot "github.com/containerd/containerd/v2/plugins/snapshots/erofs"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/erofs/erofs-container-toolkit/pkg/converter"
)

func TestPrepareFirstLayerCreatesProxyCommittedSnapshot(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)
	layerDesc := ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("layer-0-data"),
		Size:      12,
	}
	manifest, manifestDesc, manifestData := remoteManifestDescriptor(t, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layerDesc},
	})

	daemon := &fakeDaemonClient{}
	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, newStaticResolverFactory(map[digest.Digest]remoteObject{
		manifestDesc.Digest: {desc: manifestDesc, data: manifestData},
	}), daemon)

	_, err := sn.Prepare(ctx, "extract-1", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef:                  "sha256:legacy-chain-0",
		converter.EROFSManifestAnnotation: manifestDesc.Digest.String(),
		LabelCRIImageRef:                  "registry.example.com/ns/image:latest",
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}

	info, err := sn.Stat(ctx, "sha256:legacy-chain-0")
	if err != nil {
		t.Fatalf("stat committed snapshot: %v", err)
	}
	if info.Kind != snapshots.KindCommitted {
		t.Fatalf("expected committed snapshot, got %v", info.Kind)
	}
	if got := info.Labels[LabelProxyManifestDigest]; got != manifestDesc.Digest.String() {
		t.Fatalf("unexpected manifest digest label: %q", got)
	}
	if got := info.Labels[LabelSnapshotRef]; got != "sha256:legacy-chain-0" {
		t.Fatalf("unexpected snapshot.ref label: %q", got)
	}
	if got := info.Labels[LabelProxyLayerIndex]; got != "0" {
		t.Fatalf("unexpected layer index label: %q", got)
	}
	if got := info.Labels[LabelProxyLayerDigest]; got != layerDesc.Digest.String() {
		t.Fatalf("unexpected layer digest label: %q", got)
	}

	if len(daemon.started) != 1 {
		t.Fatalf("expected daemon start once, got %d", len(daemon.started))
	}
	if len(daemon.bound) != 1 {
		t.Fatalf("expected daemon bind once, got %#v", daemon.bound)
	}

	layerPath := committedLayerPath(t, sn, "sha256:legacy-chain-0")
	data, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("read materialized layer: %v", err)
	}
	if string(data) != "daemon-layer" {
		t.Fatalf("unexpected layer contents %q", string(data))
	}
	_ = manifest
}

func TestPrepareMissingLayerBindsDaemon(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)

	layerDesc := ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("remote-layer"),
		Size:      4096,
	}
	_, manifestDesc, manifestData := remoteManifestDescriptor(t, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layerDesc},
	})

	daemon := &fakeDaemonClient{}
	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, newStaticResolverFactory(map[digest.Digest]remoteObject{
		manifestDesc.Digest: {desc: manifestDesc, data: manifestData},
	}), daemon)

	_, err := sn.Prepare(ctx, "extract-remote", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef:                  "sha256:legacy-chain-remote",
		converter.EROFSManifestAnnotation: manifestDesc.Digest.String(),
		LabelCRIImageRef:                  "registry.example.com/ns/image:latest",
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}

	if len(daemon.bound) != 1 {
		t.Fatalf("expected one daemon-bound layer, got %d", len(daemon.bound))
	}
	if daemon.bound[0].InstanceID != "sha256:legacy-chain-remote" {
		t.Fatalf("unexpected bound instance id %q", daemon.bound[0].InstanceID)
	}
	if daemon.bound[0].Config.Layer.Digest != layerDesc.Digest {
		t.Fatalf("unexpected bound layer digest %q", daemon.bound[0].Config.Layer.Digest)
	}
	if _, err := os.Stat(daemon.bound[0].TargetPath); err != nil {
		t.Fatalf("expected daemon to prepare target path: %v", err)
	}
}

func TestPrepareLaterLayerUsesParentMetadata(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)

	layer0 := ocispec.Descriptor{MediaType: converter.EROFSLayerMediaType, Digest: digest.FromString("layer-0"), Size: 7}
	layer1 := ocispec.Descriptor{MediaType: converter.EROFSLayerMediaType, Digest: digest.FromString("layer-1"), Size: 7}
	_, manifestDesc, manifestData := remoteManifestDescriptor(t, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layer0, layer1},
	})

	daemon := &fakeDaemonClient{}
	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, newStaticResolverFactory(map[digest.Digest]remoteObject{
		manifestDesc.Digest: {desc: manifestDesc, data: manifestData},
	}), daemon)

	_, err := sn.Prepare(ctx, "extract-1", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef:                  "sha256:legacy-chain-0",
		converter.EROFSManifestAnnotation: manifestDesc.Digest.String(),
		LabelCRIImageRef:                  "registry.example.com/ns/image:latest",
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("prepare first layer: %v", err)
	}

	_, err = sn.Prepare(ctx, "extract-2", "sha256:legacy-chain-0", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: "sha256:legacy-chain-1",
		LabelCRIImageRef: "registry.example.com/ns/image:latest",
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("prepare second layer: %v", err)
	}

	info, err := sn.Stat(ctx, "sha256:legacy-chain-1")
	if err != nil {
		t.Fatalf("stat second committed snapshot: %v", err)
	}
	if got := info.Labels[LabelProxyLayerIndex]; got != "1" {
		t.Fatalf("unexpected second-layer index %q", got)
	}
	if got := info.Labels[LabelSnapshotRef]; got != "sha256:legacy-chain-1" {
		t.Fatalf("unexpected second-layer snapshot.ref label %q", got)
	}
	if got := info.Labels[LabelProxyLayerDigest]; got != layer1.Digest.String() {
		t.Fatalf("unexpected second-layer digest %q", got)
	}
	if len(daemon.bound) != 2 {
		t.Fatalf("expected daemon bind for both layers, got %#v", daemon.bound)
	}
	if got := daemon.bound[1].Config.Layer.Digest; got != layer1.Digest {
		t.Fatalf("unexpected second-layer daemon bind digest %q", got)
	}
}

func TestPrepareFallbackWithoutOptInMetadataDelegates(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)
	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, nil, &fakeDaemonClient{})

	mounts, err := sn.Prepare(ctx, "extract-plain", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: "sha256:legacy-chain-plain",
	}))
	if err != nil {
		t.Fatalf("prepare fallback path: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Type != "bind" {
		t.Fatalf("unexpected fallback mounts: %#v", mounts)
	}
	info, err := sn.Stat(ctx, "extract-plain")
	if err != nil {
		t.Fatalf("stat delegated active snapshot: %v", err)
	}
	if info.Kind != snapshots.KindActive {
		t.Fatalf("expected delegated active snapshot, got %v", info.Kind)
	}
	if _, err := sn.Stat(ctx, "sha256:legacy-chain-plain"); !errdefs.IsNotFound(err) {
		t.Fatalf("expected no committed proxy snapshot, got %v", err)
	}
}

func TestPrepareFailsOnInvalidLayerMediaType(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)

	layer := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    digest.FromString("legacy-layer"),
		Size:      12,
	}
	_, manifestDesc, manifestData := remoteManifestDescriptor(t, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layer},
	})

	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, newStaticResolverFactory(map[digest.Digest]remoteObject{
		manifestDesc.Digest: {desc: manifestDesc, data: manifestData},
	}), &fakeDaemonClient{})
	_, err := sn.Prepare(ctx, "extract-invalid", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef:                  "sha256:legacy-invalid",
		converter.EROFSManifestAnnotation: manifestDesc.Digest.String(),
		LabelCRIImageRef:                  "registry.example.com/ns/image:latest",
	}))
	if err == nil || !strings.Contains(err.Error(), "unsupported media type") {
		t.Fatalf("expected media type validation error, got %v", err)
	}
	if _, err := sn.Stat(ctx, "sha256:legacy-invalid"); !errdefs.IsNotFound(err) {
		t.Fatalf("expected no committed snapshot, got %v", err)
	}
}

func TestRemoveRevokesDaemonStateWithoutCheckpointPersistence(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)

	layerDesc := ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("remote-layer-remove"),
		Size:      1024,
	}
	_, manifestDesc, manifestData := remoteManifestDescriptor(t, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layerDesc},
	})

	daemon := &fakeDaemonClient{}
	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, newStaticResolverFactory(map[digest.Digest]remoteObject{
		manifestDesc.Digest: {desc: manifestDesc, data: manifestData},
	}), daemon)

	_, err := sn.Prepare(ctx, "extract-1", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef:                  "sha256:legacy-chain-0",
		converter.EROFSManifestAnnotation: manifestDesc.Digest.String(),
		LabelCRIImageRef:                  "registry.example.com/ns/image:latest",
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("prepare first layer: %v", err)
	}

	if _, err := os.Stat(filepath.Join(base.root, "instances")); !os.IsNotExist(err) {
		t.Fatalf("expected no checkpoint directory, got %v", err)
	}

	if err := sn.Remove(ctx, "sha256:legacy-chain-0"); err != nil {
		t.Fatalf("remove proxy snapshot: %v", err)
	}
	if len(daemon.unbound) != 1 || daemon.unbound[0] != "sha256:legacy-chain-0" {
		t.Fatalf("unexpected unbound daemon layers: %#v", daemon.unbound)
	}
}

func TestViewAndMountsDelegateRuntimeSemantics(t *testing.T) {
	ctx := context.Background()
	base := newErofsBaseFixture(t)

	layerDesc := ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("layer-0-data"),
		Size:      12,
	}
	_, manifestDesc, manifestData := remoteManifestDescriptor(t, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layerDesc},
	})

	sn := newContainerdErofsGRPCSnapshotter(t, base.root, base.base, newStaticResolverFactory(map[digest.Digest]remoteObject{
		manifestDesc.Digest: {desc: manifestDesc, data: manifestData},
	}), &fakeDaemonClient{})
	_, err := sn.Prepare(ctx, "extract-1", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef:                  "sha256:legacy-chain-0",
		converter.EROFSManifestAnnotation: manifestDesc.Digest.String(),
		LabelCRIImageRef:                  "registry.example.com/ns/image:latest",
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("prepare first layer: %v", err)
	}

	if _, err := sn.View(ctx, "view-1", "sha256:legacy-chain-0"); err != nil {
		t.Fatalf("view proxy snapshot: %v", err)
	}

	mounts, err := sn.Mounts(ctx, "view-1")
	if err != nil {
		t.Fatalf("mounts for view: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Type != "erofs" {
		t.Fatalf("unexpected runtime mounts: %#v", mounts)
	}
	if !strings.HasSuffix(mounts[0].Source, "layer.erofs") {
		t.Fatalf("unexpected runtime mount source: %q", mounts[0].Source)
	}
}

type erofsBaseFixture struct {
	root string
	base snapshots.Snapshotter
}

func newContainerdErofsGRPCSnapshotter(t *testing.T, root string, base snapshots.Snapshotter, resolver ResolverFactory, daemon *fakeDaemonClient) *Snapshotter {
	t.Helper()
	sn, err := New(Config{
		Root: root,
		Base: base,
		ManifestProvider: NewManifestProvider(ManifestProviderConfig{
			ResolverFactory: resolver,
		}),
		BlobProvider: NewBlobProvider(BlobProviderConfig{
			Credentials: staticCredentialBackend{},
		}),
		Daemon:       daemon,
		DaemonConfig: DaemonConfig{Root: root},
	})
	if err != nil {
		t.Fatalf("create containerd-erofs-grpc snapshotter: %v", err)
	}
	t.Cleanup(func() {
		_ = sn.Close()
	})
	return sn
}

type staticCredentialBackend struct{}

func (staticCredentialBackend) Lookup(context.Context, string) (string, string, error) {
	return "user", "pass", nil
}

type fakeDaemonClient struct {
	started []DaemonConfig
	stopped int
	bound   []recordedBoundLayer
	unbound []string
}

func (f *fakeDaemonClient) Start(_ context.Context, cfg DaemonConfig) error {
	f.started = append(f.started, cfg)
	return nil
}

func (f *fakeDaemonClient) Stop(context.Context) error {
	f.stopped++
	return nil
}

func (f *fakeDaemonClient) BindLayer(_ context.Context, instanceID string, cfg RemoteLayerConfig, targetPath string) error {
	f.bound = append(f.bound, recordedBoundLayer{
		InstanceID: instanceID,
		Config:     cfg,
		TargetPath: targetPath,
	})
	if err := os.MkdirAll(filepath.Dir(targetPath), defaultSnapshotDirPerm); err != nil {
		return err
	}
	return os.WriteFile(targetPath, []byte("daemon-layer"), defaultLayerBlobPerm)
}

func (f *fakeDaemonClient) UnbindLayer(_ context.Context, instanceID string) error {
	f.unbound = append(f.unbound, instanceID)
	return nil
}

func newErofsBaseFixture(t *testing.T) *erofsBaseFixture {
	t.Helper()

	if !kernelSupportsErofs() {
		t.Skip("EROFS is not registered in /proc/filesystems")
	}

	root := t.TempDir()
	base, err := erofssnapshot.NewSnapshotter(root)
	if err != nil {
		t.Fatalf("create erofs snapshotter: %v", err)
	}

	return &erofsBaseFixture{root: root, base: base}
}

func committedLayerPath(t *testing.T, sn *Snapshotter, key string) string {
	t.Helper()

	viewKey := "inspect-" + digest.FromString(key).Encoded()[:12]
	mounts, err := sn.View(context.Background(), viewKey, key)
	if err != nil {
		t.Fatalf("create view for %q: %v", key, err)
	}
	t.Cleanup(func() {
		_ = sn.Remove(context.Background(), viewKey)
	})
	if len(mounts) != 1 || mounts[0].Type != "erofs" {
		t.Fatalf("unexpected view mounts for %q: %#v", key, mounts)
	}
	return mounts[0].Source
}

func kernelSupportsErofs() bool {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "\terofs\n")
}

func remoteManifestDescriptor(t *testing.T, manifest ocispec.Manifest) (ocispec.Manifest, ocispec.Descriptor, []byte) {
	t.Helper()

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	return manifest, desc, data
}

func TestResolveProxyTargetUsesManifestAnnotation(t *testing.T) {
	manifestDigest := digest.FromString("manifest")
	sn := &Snapshotter{}

	gotDigest, gotIndex, err := sn.resolveProxyTarget(context.Background(), "", map[string]string{
		converter.EROFSManifestAnnotation: manifestDigest.String(),
	})
	if err != nil {
		t.Fatalf("resolve proxy target: %v", err)
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
			gotDigest, gotIndex, err := sn.resolveProxyTarget(context.Background(), tt.parent, map[string]string{})
			if err != nil {
				t.Fatalf("resolve proxy target: %v", err)
			}
			if gotDigest != "" {
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

	gotDigest, gotIndex, err := sn.resolveProxyTarget(context.Background(), "parent", map[string]string{})
	if err != nil {
		t.Fatalf("resolve proxy target: %v", err)
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

			_, _, err := sn.resolveProxyTarget(context.Background(), parent, tt.labels)
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
