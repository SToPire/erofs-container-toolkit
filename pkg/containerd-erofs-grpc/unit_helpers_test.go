package containerderofsgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func newUnitTestContentStore(t *testing.T) content.Store {
	t.Helper()

	store, err := localcontent.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("create content store: %v", err)
	}
	return store
}

func writeUnitContent(t *testing.T, ctx context.Context, store content.Store, desc ocispec.Descriptor, data []byte) {
	t.Helper()

	if err := content.WriteBlob(ctx, store, desc.Digest.String(), bytes.NewReader(data), desc); err != nil {
		t.Fatalf("write content %s: %v", desc.Digest, err)
	}
}

func writeUnitBlob(t *testing.T, ctx context.Context, store content.Store, data []byte, mediaType string) ocispec.Descriptor {
	t.Helper()

	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	writeUnitContent(t, ctx, store, desc, data)
	return desc
}

func writeUnitManifest(t *testing.T, ctx context.Context, store content.Store, manifest ocispec.Manifest) ocispec.Descriptor {
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
	writeUnitContent(t, ctx, store, desc, data)
	return desc
}

type credentialBackendFunc func(ctx context.Context, host string) (string, string, error)

func (f credentialBackendFunc) Lookup(ctx context.Context, host string) (string, string, error) {
	return f(ctx, host)
}

type recordedBoundLayer struct {
	InstanceID string
	Config     RemoteLayerConfig
	TargetPath string
}

type recordingDaemonClient struct {
	started   []DaemonConfig
	stopped   int
	bound     []recordedBoundLayer
	unbound   []string
	startErr  error
	stopErr   error
	bindErr   error
	unbindErr error
}

func (r *recordingDaemonClient) Start(_ context.Context, cfg DaemonConfig) error {
	if r.startErr != nil {
		return r.startErr
	}
	r.started = append(r.started, cfg)
	return nil
}

func (r *recordingDaemonClient) Stop(_ context.Context) error {
	if r.stopErr != nil {
		return r.stopErr
	}
	r.stopped++
	return nil
}

func (r *recordingDaemonClient) BindLayer(_ context.Context, instanceID string, cfg RemoteLayerConfig, targetPath string) error {
	if r.bindErr != nil {
		return r.bindErr
	}
	r.bound = append(r.bound, recordedBoundLayer{
		InstanceID: instanceID,
		Config:     cfg,
		TargetPath: targetPath,
	})
	if err := os.MkdirAll(filepath.Dir(targetPath), defaultSnapshotDirPerm); err != nil {
		return err
	}
	return os.WriteFile(targetPath, []byte("daemon-layer"), defaultLayerBlobPerm)
}

func (r *recordingDaemonClient) UnbindLayer(_ context.Context, instanceID string) error {
	if r.unbindErr != nil {
		return r.unbindErr
	}
	r.unbound = append(r.unbound, instanceID)
	return nil
}

type remoteObject struct {
	desc ocispec.Descriptor
	data []byte
}

func newStaticResolverFactory(objects map[digest.Digest]remoteObject) ResolverFactory {
	return func(context.Context, func(string) (string, string, error)) remotes.Resolver {
		return &staticResolver{objects: objects}
	}
}

type staticResolver struct {
	objects map[digest.Digest]remoteObject
}

func (r *staticResolver) Resolve(_ context.Context, ref string) (string, ocispec.Descriptor, error) {
	dgst, err := digestFromReference(ref)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	obj, ok := r.objects[dgst]
	if !ok {
		return "", ocispec.Descriptor{}, fmt.Errorf("resolve %s: %w", ref, errdefs.ErrNotFound)
	}
	return ref, obj.desc, nil
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

func digestFromReference(ref string) (digest.Digest, error) {
	i := bytes.LastIndexByte([]byte(ref), '@')
	if i < 0 || i+1 >= len(ref) {
		return "", fmt.Errorf("reference %q is missing digest", ref)
	}
	return digest.Parse(ref[i+1:])
}
