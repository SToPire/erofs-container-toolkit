package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/containerd/v2/pkg/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ResolverFactory func(ctx context.Context, creds func(host string) (string, string, error), hostsDir string) remotes.Resolver

type EagerDaemon struct {
	resolverFactory ResolverFactory
}

func NewEagerDaemon() *EagerDaemon {
	return &EagerDaemon{}
}

func (d *EagerDaemon) Start(context.Context, DaemonConfig) error {
	return nil
}

func (d *EagerDaemon) Stop(context.Context) error {
	return nil
}

func (d *EagerDaemon) BindLayer(ctx context.Context, _ string, cfg RemoteLayerConfig, targetPath string) error {
	if targetPath == "" {
		return fmt.Errorf("target path is required")
	}
	if cfg.ImageRef == "" {
		return fmt.Errorf("image ref is required")
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}

	resolver := d.resolverFactory
	if resolver == nil {
		resolver = defaultResolver
	}

	name, err := repositoryName(cfg.ImageRef)
	if err != nil {
		return err
	}

	r := resolver(ctx, func(string) (string, string, error) {
		return cfg.Username, cfg.Secret, nil
	}, cfg.HostsDir)

	fetcher, err := r.Fetcher(ctx, name)
	if err != nil {
		return fmt.Errorf("create fetcher for %q: %w", name, err)
	}

	rc, err := fetcher.Fetch(ctx, cfg.Layer)
	if err != nil {
		return fmt.Errorf("fetch EROFS layer %q: %w", cfg.Layer.Digest, err)
	}
	defer rc.Close()

	return writeVerifiedBlob(rc, cfg.Layer, targetPath)
}

func (d *EagerDaemon) UnbindLayer(context.Context, string) error {
	return nil
}

func defaultResolver(ctx context.Context, creds func(host string) (string, string, error), hostsDir string) remotes.Resolver {
	hostOptions := dockerconfig.HostOptions{
		Credentials: creds,
	}
	if hostsDir != "" {
		hostOptions.HostDir = dockerconfig.HostDirFromRoot(hostsDir)
	}

	return docker.NewResolver(docker.ResolverOptions{
		Hosts: dockerconfig.ConfigureHosts(ctx, hostOptions),
	})
}

func repositoryName(imageRef string) (string, error) {
	spec, err := reference.Parse(imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}
	return spec.Locator, nil
}

func writeVerifiedBlob(r io.Reader, desc ocispec.Descriptor, targetPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), "layer-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	digester := desc.Digest.Algorithm().Digester()

	n, err := io.Copy(io.MultiWriter(tmp, digester.Hash()), r)
	if err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if desc.Size >= 0 && desc.Size != n {
		return fmt.Errorf("verify blob size for %s: expected %d, got %d", desc.Digest, desc.Size, n)
	}
	if got := digester.Digest(); got != desc.Digest {
		return fmt.Errorf("verify blob %s: got %s", desc.Digest, got)
	}
	return os.Rename(tmpPath, targetPath)
}
