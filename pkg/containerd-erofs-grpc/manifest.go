package containerderofsgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	maxManifestBytes        = 4 * 1024 * 1024
	maxManifestCacheEntries = 256
)

type ResolverFactory func(ctx context.Context, creds func(host string) (string, string, error)) remotes.Resolver

type ManifestProviderConfig struct {
	ContentStore    content.Store
	Credentials     CredentialBackend
	ResolverFactory ResolverFactory
}

type RegistryManifestProvider struct {
	contentStore    content.Store
	credentials     CredentialBackend
	resolverFactory ResolverFactory
	mu              sync.RWMutex
	cache           map[digest.Digest]*ocispec.Manifest
	cacheOrder      []digest.Digest
}

func NewManifestProvider(cfg ManifestProviderConfig) *RegistryManifestProvider {
	return &RegistryManifestProvider{
		contentStore:    cfg.ContentStore,
		credentials:     cfg.Credentials,
		resolverFactory: cfg.ResolverFactory,
		cache:           make(map[digest.Digest]*ocispec.Manifest),
	}
}

func (p *RegistryManifestProvider) Get(ctx context.Context, dgst digest.Digest, labels map[string]string) (*ocispec.Manifest, error) {
	desc := ocispec.Descriptor{Digest: dgst}

	if manifest, ok := p.getCached(dgst); ok {
		return manifest, nil
	}

	if manifest, ok, err := p.getLocal(ctx, desc); err != nil {
		return nil, err
	} else if ok {
		p.storeCached(dgst, manifest)
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

	manifest, err := p.decodeManifest(desc, rc)
	if err != nil {
		return nil, err
	}
	p.storeCached(dgst, manifest)
	return manifest, nil
}

func (p *RegistryManifestProvider) getLocal(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Manifest, bool, error) {
	if p.contentStore == nil {
		return nil, false, nil
	}

	data, err := content.ReadBlob(ctx, p.contentStore, desc)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read local EROFS manifest %s: %w", desc.Digest, err)
	}

	manifest, err := p.decodeManifest(desc, bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	return manifest, true, nil
}

func (p *RegistryManifestProvider) newResolver(ctx context.Context, creds func(host string) (string, string, error)) remotes.Resolver {
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: dockerconfig.ConfigureHosts(ctx, dockerconfig.HostOptions{
			Credentials: creds,
		}),
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
	if _, ok := p.cache[dgst]; !ok {
		p.cacheOrder = append(p.cacheOrder, dgst)
	}
	p.cache[dgst] = &copied
	for len(p.cacheOrder) > maxManifestCacheEntries {
		evicted := p.cacheOrder[0]
		p.cacheOrder = p.cacheOrder[1:]
		delete(p.cache, evicted)
	}
}

func (p *RegistryManifestProvider) decodeManifest(desc ocispec.Descriptor, r io.Reader) (*ocispec.Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("manifest %s exceeds maximum size %d bytes", desc.Digest, maxManifestBytes)
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
