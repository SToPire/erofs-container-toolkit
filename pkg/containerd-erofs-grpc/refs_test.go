package containerderofsgrpc

import (
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

func TestRegistryHostFromImageRef(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		wantHost string
	}{
		{
			name:     "tagged reference",
			imageRef: "registry.example.com/ns/image:latest",
			wantHost: "registry.example.com",
		},
		{
			name:     "digest reference",
			imageRef: "docker.io/library/busybox@" + digest.FromString("busybox").String(),
			wantHost: "docker.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, err := registryHostFromImageRef(tt.imageRef)
			if err != nil {
				t.Fatalf("registryHostFromImageRef(%q): %v", tt.imageRef, err)
			}
			if host != tt.wantHost {
				t.Fatalf("unexpected host %q", host)
			}
		})
	}
}

func TestRegistryHostFromImageRefRejectsInvalidReference(t *testing.T) {
	_, err := registryHostFromImageRef("not a valid reference")
	if err == nil || !strings.Contains(err.Error(), "parse image ref") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestDigestReference(t *testing.T) {
	dgst := digest.FromString("image-manifest")
	ref, err := digestReference("registry.example.com/ns/image:latest", dgst)
	if err != nil {
		t.Fatalf("digestReference: %v", err)
	}
	if ref != "registry.example.com/ns/image@"+dgst.String() {
		t.Fatalf("unexpected digest reference %q", ref)
	}
}

func TestDigestReferenceRejectsInvalidReference(t *testing.T) {
	_, err := digestReference("%%%not-valid%%%", digest.FromString("manifest"))
	if err == nil || !strings.Contains(err.Error(), "parse image ref") {
		t.Fatalf("expected parse error, got %v", err)
	}
}
