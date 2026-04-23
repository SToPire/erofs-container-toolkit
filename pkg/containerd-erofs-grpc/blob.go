package containerderofsgrpc

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type BlobProviderConfig struct {
	ContentStore content.Store
	Credentials  CredentialBackend
}

type BlobProvider struct {
	contentStore content.Store
	credentials  CredentialBackend
}

func NewBlobProvider(cfg BlobProviderConfig) *BlobProvider {
	return &BlobProvider{
		contentStore: cfg.ContentStore,
		credentials:  cfg.Credentials,
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

	if materialized, usage, err := p.localContentStoreLayer(ctx, cfg); err != nil {
		return MaterializeResult{}, err
	} else if materialized {
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
	}

	if err := os.MkdirAll(filepath.Dir(cfg.TargetPath), defaultSnapshotDirPerm); err != nil {
		return MaterializeResult{}, err
	}

	return MaterializeResult{
		Path:   cfg.TargetPath,
		Remote: &layerCfg,
	}, nil
}

func (p *BlobProvider) localContentStoreLayer(ctx context.Context, cfg BlobConfig) (bool, snapshots.Usage, error) {
	if p.contentStore == nil {
		return false, snapshots.Usage{}, nil
	}

	info, err := p.contentStore.Info(ctx, cfg.Layer.Digest)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, snapshots.Usage{}, nil
		}
		return false, snapshots.Usage{}, fmt.Errorf("stat local EROFS layer %s: %w", cfg.Layer.Digest, err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.TargetPath), defaultSnapshotDirPerm); err != nil {
		return false, snapshots.Usage{}, err
	}

	if err := copyContentStoreBlob(ctx, p.contentStore, cfg.Layer, cfg.TargetPath); err != nil {
		return false, snapshots.Usage{}, err
	}

	return true, snapshots.Usage{
		Size:   info.Size,
		Inodes: 1,
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

func copyContentStoreBlob(ctx context.Context, store content.Store, desc ocispec.Descriptor, targetPath string) error {
	reader, err := store.ReaderAt(ctx, desc)
	if err != nil {
		return fmt.Errorf("open local EROFS layer %s: %w", desc.Digest, err)
	}
	defer reader.Close()

	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, defaultLayerBlobPerm)
	if err != nil {
		return fmt.Errorf("create local EROFS layer target %q: %w", targetPath, err)
	}
	defer file.Close()

	if _, err := io.Copy(file, content.NewReader(reader)); err != nil {
		return fmt.Errorf("copy local EROFS layer %s: %w", desc.Digest, err)
	}
	return nil
}
