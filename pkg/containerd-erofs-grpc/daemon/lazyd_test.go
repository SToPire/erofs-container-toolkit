package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestLazyDaemonBindLayerRegistersInstance(t *testing.T) {
	ctx := context.Background()
	requests := make(chan recordedLazydRequest, 1)
	socket := serveLazyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		var body lazydInstanceConfig
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode lazyd request: %v", err)
		}
		requests <- recordedLazydRequest{method: r.Method, path: r.URL.Path, body: body}
		w.WriteHeader(http.StatusNoContent)
	})

	daemon, err := NewLazyDaemon(LazyDaemonConfig{Binary: "/bin/lazyd", Socket: socket})
	if err != nil {
		t.Fatalf("create lazy daemon: %v", err)
	}
	targetPath := filepath.Join(t.TempDir(), "snapshots/1/layer.erofs")
	layer := ocispec.Descriptor{
		MediaType: "application/vnd.erofs.layer.v1",
		Digest:    digest.FromString("remote-erofs-layer"),
		Size:      4096,
	}

	if err := daemon.BindLayer(ctx, "sha256:chain", RemoteLayerConfig{
		ImageRef: "registry.example.com/ns/image:latest",
		Layer:    layer,
		Username: "user",
		Secret:   "secret",
	}, targetPath); err != nil {
		t.Fatalf("bind lazy layer: %v", err)
	}

	st, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("stat sparse target: %v", err)
	}
	if st.Size() != layer.Size {
		t.Fatalf("unexpected sparse target size %d", st.Size())
	}

	req := <-requests
	if req.method != http.MethodPut || req.path != "/api/v1/instances/sha256:chain" {
		t.Fatalf("unexpected request %s %s", req.method, req.path)
	}
	if req.body.TargetPath != targetPath {
		t.Fatalf("unexpected target path %q", req.body.TargetPath)
	}
	if req.body.Blob.Digest != layer.Digest.String() || req.body.Blob.Size != uint64(layer.Size) || req.body.Blob.MediaType != layer.MediaType {
		t.Fatalf("unexpected blob config %#v", req.body.Blob)
	}
	if req.body.Source.Type != "oci-registry" || req.body.Source.ImageRef != "registry.example.com/ns/image:latest" {
		t.Fatalf("unexpected source config %#v", req.body.Source)
	}
	if req.body.Auth == nil || req.body.Auth.Username != "user" || req.body.Auth.Secret != "secret" {
		t.Fatalf("unexpected auth config %#v", req.body.Auth)
	}
}

func TestLazyDaemonUnbindLayerDeletesInstance(t *testing.T) {
	ctx := context.Background()
	requests := make(chan recordedLazydRequest, 1)
	socket := serveLazyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- recordedLazydRequest{method: r.Method, path: r.URL.Path}
		w.WriteHeader(http.StatusNoContent)
	})

	daemon, err := NewLazyDaemon(LazyDaemonConfig{Binary: "/bin/lazyd", Socket: socket})
	if err != nil {
		t.Fatalf("create lazy daemon: %v", err)
	}
	if err := daemon.UnbindLayer(ctx, "sha256:chain"); err != nil {
		t.Fatalf("unbind lazy layer: %v", err)
	}

	req := <-requests
	if req.method != http.MethodDelete || req.path != "/api/v1/instances/sha256:chain" {
		t.Fatalf("unexpected request %s %s", req.method, req.path)
	}
}

func serveLazyHTTP(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "lazyd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	server := &http.Server{Handler: handler}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()
	return socket
}

type recordedLazydRequest struct {
	method string
	path   string
	body   lazydInstanceConfig
}
