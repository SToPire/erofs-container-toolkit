package containerderofsgrpc

import (
	"context"
	"encoding/json"
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
