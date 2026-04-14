package containerderofsgrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ResolverFactory func(ctx context.Context, creds func(host string) (string, string, error)) remotes.Resolver

type ManifestProviderConfig struct {
	Credentials     CredentialBackend
	HostsDir        string
	ResolverFactory ResolverFactory
}

type RegistryManifestProvider struct {
	credentials     CredentialBackend
	hostsDir        string
	resolverFactory ResolverFactory
	mu              sync.RWMutex
	cache           map[digest.Digest]*ocispec.Manifest
}

func NewManifestProvider(cfg ManifestProviderConfig) *RegistryManifestProvider {
	return &RegistryManifestProvider{
		credentials:     cfg.Credentials,
		hostsDir:        cfg.HostsDir,
		resolverFactory: cfg.ResolverFactory,
		cache:           make(map[digest.Digest]*ocispec.Manifest),
	}
}

func (p *RegistryManifestProvider) Get(ctx context.Context, dgst digest.Digest, labels map[string]string) (*ocispec.Manifest, error) {
	desc := ocispec.Descriptor{Digest: dgst}

	if manifest, ok := p.getCached(dgst); ok {
		return manifest, nil
	}

	imageRef := labels[LabelCRIImageRef]
	if imageRef == "" {
		return nil, fmt.Errorf("%s is required when the EROFS manifest must be fetched remotely", LabelCRIImageRef)
	}

	ref, err := digestReference(imageRef, dgst)
	if err != nil {
		return nil, err
	}

	resolver := p.resolverFactory
	if resolver == nil {
		resolver = p.newResolver
	}

	r := resolver(ctx, func(host string) (string, string, error) {
		if p.credentials == nil {
			return "", "", nil
		}
		return p.credentials.Lookup(ctx, host)
	})

	name, resolved, err := r.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve remote EROFS manifest %q: %w", ref, err)
	}

	fetcher, err := r.Fetcher(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create fetcher for %q: %w", name, err)
	}

	rc, err := fetcher.Fetch(ctx, resolved)
	if err != nil {
		return nil, fmt.Errorf("fetch EROFS manifest %q: %w", ref, err)
	}
	defer rc.Close()

	manifest, err := p.decodeManifest(ctx, desc, rc)
	if err != nil {
		return nil, err
	}
	p.storeCached(dgst, manifest)
	return manifest, nil
}

func (p *RegistryManifestProvider) newResolver(ctx context.Context, creds func(host string) (string, string, error)) remotes.Resolver {
	hostOptions := dockerconfig.HostOptions{
		Credentials: creds,
	}
	if p.hostsDir != "" {
		hostOptions.HostDir = dockerconfig.HostDirFromRoot(p.hostsDir)
	}

	return docker.NewResolver(docker.ResolverOptions{
		Hosts: dockerconfig.ConfigureHosts(ctx, hostOptions),
	})
}

func (p *RegistryManifestProvider) getCached(dgst digest.Digest) (*ocispec.Manifest, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	manifest, ok := p.cache[dgst]
	if !ok {
		return nil, false
	}
	copied := *manifest
	return &copied, true
}

func (p *RegistryManifestProvider) storeCached(dgst digest.Digest, manifest *ocispec.Manifest) {
	p.mu.Lock()
	defer p.mu.Unlock()

	copied := *manifest
	p.cache[dgst] = &copied
}

func (p *RegistryManifestProvider) decodeManifest(ctx context.Context, desc ocispec.Descriptor, r io.Reader) (*ocispec.Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if got := digest.FromBytes(data); got != desc.Digest {
		return nil, fmt.Errorf("verify manifest %s: got %s", desc.Digest, got)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", desc.Digest, err)
	}
	if manifest.MediaType == "" {
		manifest.MediaType = desc.MediaType
	}
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		return nil, fmt.Errorf("digest %s did not resolve to an OCI image manifest", desc.Digest)
	}

	return &manifest, nil
}
