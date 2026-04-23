package containerderofsgrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/erofs/erofs-container-toolkit/pkg/converter"
)

func TestManifestProviderRequiresImageRefWhenRemoteFetchNeeded(t *testing.T) {
	ctx := context.Background()
	provider := NewManifestProvider(ManifestProviderConfig{})

	_, err := provider.Get(ctx, digest.FromString("missing-manifest"), map[string]string{})
	if err == nil || !strings.Contains(err.Error(), LabelCRIImageRef) {
		t.Fatalf("expected missing image-ref error, got %v", err)
	}
}

func TestManifestProviderFetchesManifestFromRemote(t *testing.T) {
	ctx := context.Background()
	layer := ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("layer-data"),
		Size:      10,
	}
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layer},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}

	provider := NewManifestProvider(ManifestProviderConfig{
		ResolverFactory: newStaticResolverFactory(map[digest.Digest]remoteObject{
			desc.Digest: {desc: desc, data: data},
		}),
	})

	got, err := provider.Get(ctx, desc.Digest, map[string]string{
		LabelCRIImageRef: "registry.example.com/ns/image:latest",
	})
	if err != nil {
		t.Fatalf("get remote manifest: %v", err)
	}
	if got.MediaType != ocispec.MediaTypeImageManifest {
		t.Fatalf("unexpected manifest media type %q", got.MediaType)
	}
	if len(got.Layers) != 1 || got.Layers[0].Digest != layer.Digest {
		t.Fatalf("unexpected manifest layers %#v", got.Layers)
	}

	// Verify the manifest stays in the provider's in-memory cache.
	cached, err := provider.Get(ctx, desc.Digest, map[string]string{})
	if err != nil {
		t.Fatalf("get cached manifest: %v", err)
	}
	if cached.Layers[0].Digest != layer.Digest {
		t.Fatalf("unexpected cached manifest layers %#v", cached.Layers)
	}
}

func TestManifestProviderReadsManifestFromContentStore(t *testing.T) {
	ctx := context.Background()
	store := newUnitTestContentStore(t)
	layer := ocispec.Descriptor{
		MediaType: converter.EROFSLayerMediaType,
		Digest:    digest.FromString("layer-data"),
		Size:      10,
	}
	desc := writeUnitManifest(t, ctx, store, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{layer},
	})

	provider := NewManifestProvider(ManifestProviderConfig{
		ContentStore: store,
	})

	got, err := provider.Get(ctx, desc.Digest, map[string]string{})
	if err != nil {
		t.Fatalf("get local manifest: %v", err)
	}
	if got.MediaType != ocispec.MediaTypeImageManifest {
		t.Fatalf("unexpected manifest media type %q", got.MediaType)
	}
	if len(got.Layers) != 1 || got.Layers[0].Digest != layer.Digest {
		t.Fatalf("unexpected manifest layers %#v", got.Layers)
	}
}

func TestManifestProviderRejectsInvalidRemoteManifestPayload(t *testing.T) {
	ctx := context.Background()
	data := []byte("{not-json")
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}

	provider := NewManifestProvider(ManifestProviderConfig{
		ResolverFactory: newStaticResolverFactory(map[digest.Digest]remoteObject{
			desc.Digest: {desc: desc, data: data},
		}),
	})

	_, err := provider.Get(ctx, desc.Digest, map[string]string{
		LabelCRIImageRef: "registry.example.com/ns/image:latest",
	})
	if err == nil || !strings.Contains(err.Error(), "decode manifest") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestManifestProviderRejectsNonManifestContent(t *testing.T) {
	ctx := context.Background()
	data, err := json.Marshal(map[string]any{"schemaVersion": 2})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}

	provider := NewManifestProvider(ManifestProviderConfig{
		ResolverFactory: newStaticResolverFactory(map[digest.Digest]remoteObject{
			desc.Digest: {desc: desc, data: data},
		}),
	})

	_, err = provider.Get(ctx, desc.Digest, map[string]string{
		LabelCRIImageRef: "registry.example.com/ns/image:latest",
	})
	if err == nil || !strings.Contains(err.Error(), "OCI image manifest") {
		t.Fatalf("expected non-manifest error, got %v", err)
	}
}

func TestManifestProviderRejectsOversizedManifest(t *testing.T) {
	data := []byte(strings.Repeat("x", maxManifestBytes+1))
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	provider := NewManifestProvider(ManifestProviderConfig{})

	_, err := provider.decodeManifest(desc, strings.NewReader(string(data)))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected manifest size error, got %v", err)
	}
}

func TestManifestProviderEvictsOldestCachedManifest(t *testing.T) {
	provider := NewManifestProvider(ManifestProviderConfig{})

	for i := range maxManifestCacheEntries + 1 {
		dgst := digest.FromString(fmt.Sprintf("manifest-%d", i))
		provider.storeCached(dgst, &ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
		})
	}

	if _, ok := provider.getCached(digest.FromString("manifest-0")); ok {
		t.Fatalf("expected oldest manifest to be evicted")
	}
	if _, ok := provider.getCached(digest.FromString(fmt.Sprintf("manifest-%d", maxManifestCacheEntries))); !ok {
		t.Fatalf("expected newest manifest to remain cached")
	}
}

func TestManifestProviderReadsOnlySizeLimitPlusOne(t *testing.T) {
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromString("oversized-manifest"),
	}
	reader := &countingReader{remaining: maxManifestBytes + 1024}
	provider := NewManifestProvider(ManifestProviderConfig{})

	_, err := provider.decodeManifest(desc, reader)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected manifest size error, got %v", err)
	}
	if reader.read > maxManifestBytes+1 {
		t.Fatalf("read beyond size limit: read %d, limit %d", reader.read, maxManifestBytes+1)
	}
}

type countingReader struct {
	remaining int
	read      int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}
