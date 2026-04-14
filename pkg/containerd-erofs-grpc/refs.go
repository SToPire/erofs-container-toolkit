package containerderofsgrpc

import (
	"fmt"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/opencontainers/go-digest"
)

func parseImageReference(imageRef string) (reference.Spec, error) {
	spec, err := reference.Parse(imageRef)
	if err != nil {
		return reference.Spec{}, fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}
	return spec, nil
}

func registryHostFromImageRef(imageRef string) (string, error) {
	spec, err := parseImageReference(imageRef)
	if err != nil {
		return "", err
	}
	return spec.Hostname(), nil
}

func digestReference(imageRef string, dgst digest.Digest) (string, error) {
	spec, err := parseImageReference(imageRef)
	if err != nil {
		return "", err
	}
	return spec.Locator + "@" + dgst.String(), nil
}
