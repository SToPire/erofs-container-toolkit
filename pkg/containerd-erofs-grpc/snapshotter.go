package containerderofsgrpc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/erofs/erofs-container-toolkit/pkg/converter"
)

type Config struct {
	Root             string
	Base             snapshots.Snapshotter
	ManifestProvider ManifestProvider
	BlobProvider     *BlobProvider
	Daemon           DaemonClient
	DaemonConfig     DaemonConfig
}

type Snapshotter struct {
	root      string
	base      snapshots.Snapshotter
	manifests ManifestProvider
	blobs     *BlobProvider
	daemon    DaemonClient
}

func New(cfg Config) (*Snapshotter, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("root is required")
	}
	if cfg.Base == nil {
		return nil, fmt.Errorf("base snapshotter is required")
	}
	if cfg.ManifestProvider == nil {
		return nil, fmt.Errorf("manifest provider is required")
	}
	if cfg.BlobProvider == nil {
		return nil, fmt.Errorf("blob provider is required")
	}
	if cfg.Daemon == nil {
		return nil, fmt.Errorf("daemon client is required")
	}
	if err := cfg.Daemon.Start(context.Background(), cfg.DaemonConfig); err != nil {
		return nil, err
	}

	return &Snapshotter{
		root:      cfg.Root,
		base:      cfg.Base,
		manifests: cfg.ManifestProvider,
		blobs:     cfg.BlobProvider,
		daemon:    cfg.Daemon,
	}, nil
}

func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"parent": parent,
	}).Debug("Prepare")
	defer log.G(ctx).Debug("Prepare completed")

	info := snapshots.Info{}
	for _, opt := range opts {
		if err := opt(&info); err != nil {
			return nil, err
		}
	}
	labels := info.Labels

	targetChainID := labels[LabelSnapshotRef]
	if targetChainID == "" {
		return s.base.Prepare(ctx, key, parent, opts...)
	}

	if _, err := s.base.Stat(ctx, targetChainID); err == nil {
		return nil, errdefs.ErrAlreadyExists
	} else if !errdefs.IsNotFound(err) {
		return nil, err
	}

	manifestDigest, layerIndex, handled, err := s.resolveProxyTarget(ctx, parent, labels)
	if err != nil {
		return nil, err
	}
	if !handled {
		return s.base.Prepare(ctx, key, parent, opts...)
	}

	manifest, err := s.manifests.Get(ctx, manifestDigest, labels)
	if err != nil {
		return nil, err
	}

	layer, err := validateAndSelectLayer(manifest, layerIndex)
	if err != nil {
		return nil, err
	}

	prepareMounts, err := s.base.Prepare(ctx, key, parent, opts...)
	if err != nil {
		return nil, err
	}

	cleanupActive := true
	layerBound := false
	defer func() {
		if cleanupActive {
			if layerBound {
				_ = s.daemon.UnbindLayer(ctx, targetChainID)
			}
			_ = s.base.Remove(ctx, key)
		}
	}()

	snapshotID, err := snapshotIDFromMounts(s.root, prepareMounts)
	if err != nil {
		return nil, err
	}

	layerPath := s.layerBlobPath(snapshotID)
	result, err := s.blobs.Materialize(ctx, BlobConfig{
		TargetChainID:  targetChainID,
		InstanceID:     targetChainID,
		ImageRef:       labels[LabelCRIImageRef],
		ManifestDigest: manifestDigest,
		LayerIndex:     layerIndex,
		Layer:          layer,
		Labels:         labels,
		TargetPath:     layerPath,
	})
	if err != nil {
		return nil, err
	}
	if result.Remote != nil {
		if err := s.daemon.BindLayer(ctx, targetChainID, *result.Remote, layerPath); err != nil {
			return nil, err
		}
		layerBound = true
	}

	commitLabels := map[string]string{
		LabelSnapshotRef:         targetChainID,
		LabelProxyManifestDigest: manifestDigest.String(),
		LabelProxyLayerIndex:     strconv.Itoa(layerIndex),
		LabelProxyLayerDigest:    layer.Digest.String(),
	}
	if err := s.base.Commit(ctx, targetChainID, key, snapshots.WithLabels(commitLabels)); err != nil {
		return nil, err
	}

	cleanupActive = false
	return nil, errdefs.ErrAlreadyExists
}

func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"parent": parent,
	}).Debug("View")
	defer log.G(ctx).Debug("View completed")
	return s.base.View(ctx, key, parent, opts...)
}

func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).Debug("Mounts")
	defer log.G(ctx).Debug("Mounts completed")
	return s.base.Mounts(ctx, key)
}

func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(log.Fields{
		"name": name,
		"key":  key,
	}).Debug("Commit")
	defer log.G(ctx).Debug("Commit completed")
	return s.base.Commit(ctx, name, key, opts...)
}

func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	log.G(ctx).WithField("key", key).Debug("Remove")
	defer log.G(ctx).Debug("Remove completed")

	info, statErr := s.base.Stat(ctx, key)
	err := s.base.Remove(ctx, key)
	if err != nil {
		return err
	}

	if statErr == nil && info.Labels[LabelProxyManifestDigest] != "" {
		if err := s.daemon.UnbindLayer(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	log.G(ctx).WithField("key", key).Debug("Stat")
	defer log.G(ctx).Debug("Stat completed")
	return s.base.Stat(ctx, key)
}

func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	log.G(ctx).WithFields(log.Fields{
		"name":       info.Name,
		"fieldpaths": fieldpaths,
	}).Debug("Update")
	defer log.G(ctx).Debug("Update completed")
	return s.base.Update(ctx, info, fieldpaths...)
}

func (s *Snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	log.G(ctx).WithField("filters", filters).Debug("Walk")
	defer log.G(ctx).Debug("Walk completed")
	return s.base.Walk(ctx, fn, filters...)
}

func (s *Snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	log.G(ctx).WithField("key", key).Debug("Usage")
	defer log.G(ctx).Debug("Usage completed")
	return s.base.Usage(ctx, key)
}

func (s *Snapshotter) Cleanup(ctx context.Context) error {
	log.G(ctx).Debug("Cleanup")
	defer log.G(ctx).Debug("Cleanup completed")
	cleaner, ok := s.base.(interface {
		Cleanup(context.Context) error
	})
	if !ok {
		return errdefs.ErrNotImplemented
	}
	return cleaner.Cleanup(ctx)
}

func (s *Snapshotter) Close() error {
	var errs []error
	if err := s.daemon.Stop(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if closer, ok := s.base.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Snapshotter) resolveProxyTarget(ctx context.Context, parent string, labels map[string]string) (digest.Digest, int, bool, error) {
	if manifestText := labels[converter.EROFSManifestAnnotation]; manifestText != "" {
		dgst, err := digest.Parse(manifestText)
		if err != nil {
			return "", 0, false, fmt.Errorf("parse %s: %w", converter.EROFSManifestAnnotation, err)
		}
		return dgst, 0, true, nil
	}

	if parent == "" {
		return "", 0, false, nil
	}

	info, err := s.base.Stat(ctx, parent)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}

	manifestText := info.Labels[LabelProxyManifestDigest]
	layerIndexText := info.Labels[LabelProxyLayerIndex]
	if manifestText == "" || layerIndexText == "" {
		return "", 0, false, nil
	}

	dgst, err := digest.Parse(manifestText)
	if err != nil {
		return "", 0, false, fmt.Errorf("parse parent proxy manifest digest: %w", err)
	}
	layerIndex, err := strconv.Atoi(layerIndexText)
	if err != nil {
		return "", 0, false, fmt.Errorf("parse parent proxy layer index: %w", err)
	}

	return dgst, layerIndex + 1, true, nil
}

func (s *Snapshotter) layerBlobPath(snapshotID string) string {
	return targetLayerPath(s.root, snapshotID)
}

func snapshotIDFromMounts(root string, mounts []mount.Mount) (string, error) {
	snapshotRoot := filepath.Join(root, "snapshots") + string(filepath.Separator)

	for _, m := range mounts {
		if strings.HasPrefix(m.Source, snapshotRoot) {
			return snapshotIDFromPath(snapshotRoot, m.Source)
		}
		for _, opt := range m.Options {
			if upperdir, ok := strings.CutPrefix(opt, "upperdir="); ok {
				if strings.HasPrefix(upperdir, snapshotRoot) {
					return snapshotIDFromPath(snapshotRoot, upperdir)
				}
			}
		}
	}

	return "", fmt.Errorf("could not determine snapshot id from prepare mounts")
}

func snapshotIDFromPath(snapshotRoot, path string) (string, error) {
	rel := strings.TrimPrefix(path, snapshotRoot)
	if rel == path {
		return "", fmt.Errorf("path %q does not belong to snapshot root %q", path, snapshotRoot)
	}
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("invalid snapshot path %q", path)
	}
	return parts[0], nil
}

func validateAndSelectLayer(manifest *ocispec.Manifest, layerIndex int) (ocispec.Descriptor, error) {
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		return ocispec.Descriptor{}, fmt.Errorf("EROFS manifest must be %s, got %s", ocispec.MediaTypeImageManifest, manifest.MediaType)
	}
	if layerIndex < 0 || layerIndex >= len(manifest.Layers) {
		return ocispec.Descriptor{}, fmt.Errorf("EROFS manifest layer index %d out of range", layerIndex)
	}

	for i, layer := range manifest.Layers {
		if layer.MediaType != converter.EROFSLayerMediaType {
			return ocispec.Descriptor{}, fmt.Errorf("EROFS manifest layer %d has unsupported media type %s", i, layer.MediaType)
		}
	}
	return manifest.Layers[layerIndex], nil
}
