//go:build integration
// +build integration

package containerderofsgrpc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
