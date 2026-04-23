package daemon

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestEagerDaemonBindLayerDownloadsBlob(t *testing.T) {
	ctx := context.Background()
	layerData := []byte("erofs-layer-data")
	layer := ocispec.Descriptor{
		Digest: digest.FromBytes(layerData),
		Size:   int64(len(layerData)),
	}

	daemon := &EagerDaemon{
		resolverFactory: func(context.Context, func(string) (string, string, error)) remotes.Resolver {
			return &staticResolver{
				objects: map[digest.Digest]remoteObject{
					layer.Digest: {desc: layer, data: layerData},
				},
			}
		},
	}

	targetPath := filepathForTest(t, "snapshots/1/layer.erofs")
	if err := daemon.BindLayer(ctx, "instance-1", RemoteLayerConfig{
		ImageRef: "registry.example.com/ns/image:latest",
		Layer:    layer,
	}, targetPath); err != nil {
		t.Fatalf("bind eager layer: %v", err)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read eager layer: %v", err)
	}
	if !bytes.Equal(data, layerData) {
		t.Fatalf("unexpected eager layer contents %q", string(data))
	}
}

func filepathForTest(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), rel)
}

type remoteObject struct {
	desc ocispec.Descriptor
	data []byte
}

type staticResolver struct {
	objects map[digest.Digest]remoteObject
}

func (r *staticResolver) Resolve(_ context.Context, ref string) (string, ocispec.Descriptor, error) {
	return "", ocispec.Descriptor{}, errdefs.ErrNotImplemented
}

func (r *staticResolver) Fetcher(_ context.Context, _ string) (remotes.Fetcher, error) {
	return staticFetcher{objects: r.objects}, nil
}

func (r *staticResolver) Pusher(context.Context, string) (remotes.Pusher, error) {
	return nil, errdefs.ErrNotImplemented
}

type staticFetcher struct {
	objects map[digest.Digest]remoteObject
}

func (f staticFetcher) Fetch(_ context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	obj, ok := f.objects[desc.Digest]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(obj.data)), nil
}
