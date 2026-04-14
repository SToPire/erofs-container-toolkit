package containerderofsgrpc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/snapshots"
)

type BlobProviderConfig struct {
	Credentials CredentialBackend
	HostsDir    string
}

type BlobProvider struct {
	credentials CredentialBackend
	hostsDir    string
}

func NewBlobProvider(cfg BlobProviderConfig) *BlobProvider {
	return &BlobProvider{
		credentials: cfg.Credentials,
		hostsDir:    cfg.HostsDir,
	}
}

type MaterializeResult struct {
	Path   string
	Usage  snapshots.Usage
	Remote *RemoteLayerConfig
}

func (p *BlobProvider) Materialize(ctx context.Context, cfg BlobConfig) (MaterializeResult, error) {
	if cfg.TargetPath == "" {
		return MaterializeResult{}, fmt.Errorf("target path is required")
	}

	if local, usage, err := p.localLayer(cfg.TargetPath); err != nil {
		return MaterializeResult{}, err
	} else if local {
		return MaterializeResult{
			Path:  cfg.TargetPath,
			Usage: usage,
		}, nil
	}

	if cfg.ImageRef == "" {
		return MaterializeResult{}, fmt.Errorf("%s is required for blob materialization", LabelCRIImageRef)
	}

	host, err := registryHostFromImageRef(cfg.ImageRef)
	if err != nil {
		return MaterializeResult{}, err
	}

	var username, secret string
	if p.credentials != nil {
		username, secret, err = p.credentials.Lookup(ctx, host)
		if err != nil {
			return MaterializeResult{}, err
		}
	}

	layerCfg := RemoteLayerConfig{
		ImageRef:       cfg.ImageRef,
		Host:           host,
		ManifestDigest: cfg.ManifestDigest,
		Layer:          cfg.Layer,
		Username:       username,
		Secret:         secret,
		HostsDir:       p.hostsDir,
	}

	if err := os.MkdirAll(filepath.Dir(cfg.TargetPath), defaultSnapshotDirPerm); err != nil {
		return MaterializeResult{}, err
	}

	return MaterializeResult{
		Path:   cfg.TargetPath,
		Remote: &layerCfg,
	}, nil
}

func (p *BlobProvider) localLayer(targetPath string) (bool, snapshots.Usage, error) {
	info, err := os.Stat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, snapshots.Usage{}, nil
		}
		return false, snapshots.Usage{}, err
	}
	if info.IsDir() {
		return false, snapshots.Usage{}, fmt.Errorf("target path %q is a directory", targetPath)
	}
	return true, snapshots.Usage{
		Size:   info.Size(),
		Inodes: 1,
	}, nil
}

func targetLayerPath(root, snapshotID string) string {
	if root == "" || snapshotID == "" {
		return ""
	}
	return filepath.Join(root, "snapshots", snapshotID, "layer.erofs")
}
