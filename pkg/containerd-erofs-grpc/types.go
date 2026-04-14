package containerderofsgrpc

import (
	"context"

	daemonclient "github.com/erofs/erofs-container-toolkit/pkg/containerd-erofs-grpc/daemon"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	LabelSnapshotRef         = "containerd.io/snapshot.ref"
	LabelCRIImageRef         = "containerd.io/snapshot/cri.image-ref"
	LabelProxyManifestDigest = "containerd.io/snapshot/erofs.proxy.manifest.digest"
	LabelProxyLayerIndex     = "containerd.io/snapshot/erofs.proxy.layer.index"
	LabelProxyLayerDigest    = "containerd.io/snapshot/erofs.proxy.layer.digest"
	defaultLayerBlobPerm     = 0o644
	defaultSnapshotDirPerm   = 0o700
)

type ManifestProvider interface {
	Get(ctx context.Context, dgst digest.Digest, labels map[string]string) (*ocispec.Manifest, error)
}

type CredentialBackend interface {
	Lookup(ctx context.Context, host string) (username, secret string, err error)
}

type BlobConfig struct {
	TargetChainID  string
	InstanceID     string
	ImageRef       string
	ManifestDigest digest.Digest
	LayerIndex     int
	Layer          ocispec.Descriptor
	Labels         map[string]string
	TargetPath     string
}

type DaemonConfig = daemonclient.DaemonConfig

type RemoteLayerConfig = daemonclient.RemoteLayerConfig

type DaemonClient = daemonclient.DaemonClient
