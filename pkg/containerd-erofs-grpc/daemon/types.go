package daemon

import (
	"context"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type DaemonConfig struct {
	Root string `json:"root"`
}

type RemoteLayerConfig struct {
	ImageRef string `json:"image_ref"`
	Host     string `json:"host"`

	ManifestDigest digest.Digest      `json:"manifest_digest"`
	Layer          ocispec.Descriptor `json:"layer"`

	Username string `json:"username,omitempty"`
	Secret   string `json:"secret,omitempty"`
	HostsDir string `json:"hosts_dir,omitempty"`
}

type DaemonClient interface {
	Start(ctx context.Context, cfg DaemonConfig) error
	Stop(ctx context.Context) error
	BindLayer(ctx context.Context, instanceID string, cfg RemoteLayerConfig, targetPath string) error
	UnbindLayer(ctx context.Context, instanceID string) error
}
