package containerderofsgrpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/erofs/erofs-container-toolkit/pkg/converter"
)

func TestBlobProviderMaterializeUsesExistingTargetLayer(t *testing.T) {
	ctx := context.Background()
	layerData := []byte("local-erofs-layer")
	targetPath := filepath.Join(t.TempDir(), "snapshots", "1", "layer.erofs")
	if err := os.MkdirAll(filepath.Dir(targetPath), defaultSnapshotDirPerm); err != nil {
		t.Fatalf("mkdir target path: %v", err)
	}
	if err := os.WriteFile(targetPath, layerData, defaultLayerBlobPerm); err != nil {
		t.Fatalf("write local layer: %v", err)
	}

	provider := NewBlobProvider(BlobProviderConfig{
		Credentials: credentialBackendFunc(func(context.Context, string) (string, string, error) {
			return "user", "secret", nil
		}),
		HostsDir: "/etc/containerd/certs.d",
	})

	manifestDigest := digest.FromString("manifest")
	result, err := provider.Materialize(ctx, BlobConfig{
		InstanceID:     "instance-1",
		ImageRef:       "registry.example.com/ns/image:latest",
		ManifestDigest: manifestDigest,
		Layer:          ocispecDescriptorForRemoteLayer(),
		TargetPath:     targetPath,
	})
	if err != nil {
		t.Fatalf("materialize local blob: %v", err)
	}
	if result.Path != targetPath {
		t.Fatalf("unexpected target path %q", result.Path)
	}
	if result.Usage.Size != int64(len(layerData)) || result.Usage.Inodes != 1 {
		t.Fatalf("unexpected usage %#v", result.Usage)
	}
	if result.Remote != nil {
		t.Fatalf("expected no remote config for local blob")
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(data) != string(layerData) {
		t.Fatalf("unexpected materialized data %q", string(data))
	}
}

func TestBlobProviderMaterializeReturnsRemoteLayerConfigWhenBlobMissing(t *testing.T) {
	ctx := context.Background()
	provider := NewBlobProvider(BlobProviderConfig{
		Credentials: credentialBackendFunc(func(context.Context, string) (string, string, error) {
			return "user", "secret", nil
		}),
		HostsDir: "/etc/containerd/certs.d",
	})

	layer := ocispecDescriptorForRemoteLayer()
	targetPath := filepath.Join(t.TempDir(), "snapshots", "2", "layer.erofs")
	manifestDigest := digest.FromString("manifest")
	result, err := provider.Materialize(ctx, BlobConfig{
		InstanceID:     "instance-2",
		ImageRef:       "registry.example.com/ns/image:latest",
		ManifestDigest: manifestDigest,
		Layer:          layer,
		TargetPath:     targetPath,
	})
	if err != nil {
		t.Fatalf("materialize remote layer: %v", err)
	}
	if result.Path != targetPath {
		t.Fatalf("unexpected target path %q", result.Path)
	}
	if result.Usage.Size != 0 || result.Usage.Inodes != 0 {
		t.Fatalf("unexpected usage %#v", result.Usage)
	}
	if result.Remote == nil {
		t.Fatalf("expected remote layer config")
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("expected blob provider to leave target path untouched, got %v", err)
	}
	if got := result.Remote.Host; got != "registry.example.com" {
		t.Fatalf("unexpected remote host %q", got)
	}
	if got := result.Remote.Username; got != "user" {
		t.Fatalf("unexpected remote username %q", got)
	}
	if got := result.Remote.Secret; got != "secret" {
		t.Fatalf("unexpected remote secret %q", got)
	}
	if got := result.Remote.HostsDir; got != "/etc/containerd/certs.d" {
		t.Fatalf("unexpected remote hosts dir %q", got)
	}
	if got := result.Remote.ManifestDigest; got != manifestDigest {
		t.Fatalf("unexpected remote manifest digest %q", got)
	}
	if got := result.Remote.Layer.Digest; got != layer.Digest {
		t.Fatalf("unexpected remote layer digest %q", got)
	}
}

func TestBlobProviderMaterializeRequiresImageRef(t *testing.T) {
	ctx := context.Background()
	provider := NewBlobProvider(BlobProviderConfig{})

	_, err := provider.Materialize(ctx, BlobConfig{
		TargetPath: filepath.Join(t.TempDir(), "layer.erofs"),
	})
	if err == nil || !strings.Contains(err.Error(), LabelCRIImageRef) {
		t.Fatalf("expected missing image-ref error, got %v", err)
	}
}

func TestBlobProviderMaterializePropagatesCredentialError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("credentials unavailable")
	provider := NewBlobProvider(BlobProviderConfig{
		Credentials: credentialBackendFunc(func(context.Context, string) (string, string, error) {
			return "", "", wantErr
		}),
	})

	_, err := provider.Materialize(ctx, BlobConfig{
		ImageRef:   "registry.example.com/ns/image:latest",
		Layer:      ocispecDescriptorForRemoteLayer(),
		TargetPath: filepath.Join(t.TempDir(), "layer.erofs"),
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected credential error %v, got %v", wantErr, err)
	}
}

func ocispecDescriptorForRemoteLayer() ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("remote-erofs-layer"),
		Size:      128,
	}
}
