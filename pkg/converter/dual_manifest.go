package converter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type DualManifestConverter struct {
	cs         content.Store
	platformMC platforms.MatchComparer
	erofsOpts  []Option
}

func NewDualManifestConverter(cs content.Store, platformMC platforms.MatchComparer, erofsOpts ...Option) *DualManifestConverter {
	return &DualManifestConverter{
		cs:         cs,
		platformMC: platformMC,
		erofsOpts:  erofsOpts,
	}
}

func (d *DualManifestConverter) Convert(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	switch desc.MediaType {
	case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
		return d.convertIndex(ctx, desc)
	case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		return d.convertManifestToDualIndex(ctx, desc)
	default:
		return nil, nil
	}
}

func (d *DualManifestConverter) convertIndex(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	index, labels, err := readJSONWithLabels[ocispec.Index](ctx, d.cs, desc)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = make(map[string]string)
	}

	var newManifests []ocispec.Descriptor
	var newLabels = make(map[string]string)

	for i, m := range index.Manifests {
		if m.Platform != nil && !d.platformMC.Match(*m.Platform) {
			continue
		}

		legacyDesc, erofsDesc, err := d.convertSingleManifest(ctx, m)
		if err != nil {
			return nil, err
		}

		newManifests = append(newManifests, *legacyDesc, *erofsDesc)

		// Build GC labels for index -> manifest references
		newLabels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", i*2)] = legacyDesc.Digest.String()
		newLabels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", i*2+1)] = erofsDesc.Digest.String()
	}

	newIndex := ocispec.Index{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageIndex,
		Manifests:   newManifests,
		Annotations: index.Annotations,
	}

	return writeJSON(ctx, d.cs, &newIndex, desc, newLabels)
}

func (d *DualManifestConverter) convertManifestToDualIndex(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	legacyDesc, erofsDesc, err := d.convertSingleManifest(ctx, desc)
	if err != nil {
		return nil, err
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{*legacyDesc, *erofsDesc},
	}

	labels := map[string]string{
		"containerd.io/gc.ref.content.m.0": legacyDesc.Digest.String(),
		"containerd.io/gc.ref.content.m.1": erofsDesc.Digest.String(),
	}

	return writeJSON(ctx, d.cs, &index, desc, labels)
}

func (d *DualManifestConverter) convertSingleManifest(ctx context.Context, legacyDesc ocispec.Descriptor) (*ocispec.Descriptor, *ocispec.Descriptor, error) {
	legacyManifest, _, err := readJSONWithLabels[ocispec.Manifest](ctx, d.cs, legacyDesc)
	if err != nil {
		return nil, nil, err
	}

	// Convert layers to EROFS
	layerConvertFunc := LayerConvertFunc(d.erofsOpts...)
	var erofsLayers []ocispec.Descriptor
	var diffIDs []digest.Digest

	for _, layer := range legacyManifest.Layers {
		newLayer, err := layerConvertFunc(ctx, d.cs, layer)
		if err != nil {
			return nil, nil, err
		}
		if newLayer == nil {
			newLayer = &layer
		}
		erofsLayers = append(erofsLayers, *newLayer)

		info, err := d.cs.Info(ctx, newLayer.Digest)
		if err != nil {
			return nil, nil, err
		}
		diffIDs = append(diffIDs, digest.Digest(info.Labels["containerd.io/uncompressed"]))
	}

	// Create EROFS config
	config, _, err := readJSONWithLabels[ocispec.Image](ctx, d.cs, legacyManifest.Config)
	if err != nil {
		return nil, nil, err
	}
	legacyPlatform := platformFromImageConfig(config)
	config.RootFS.DiffIDs = diffIDs
	erofsConfigLabels := map[string]string{}
	erofsConfigDesc, err := writeJSON(ctx, d.cs, config, legacyManifest.Config, erofsConfigLabels)
	if err != nil {
		return nil, nil, err
	}

	// Build GC labels for EROFS manifest
	erofsManifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": erofsConfigDesc.Digest.String(),
	}
	for i, layer := range erofsLayers {
		erofsManifestLabels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = layer.Digest.String()
	}

	// Create EROFS manifest
	erofsManifest := ocispec.Manifest{
		Versioned:   legacyManifest.Versioned,
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      *erofsConfigDesc,
		Layers:      erofsLayers,
		Annotations: legacyManifest.Annotations,
	}
	erofsManifestDesc, err := writeJSON(ctx, d.cs, &erofsManifest, legacyDesc, erofsManifestLabels)
	if err != nil {
		return nil, nil, err
	}

	// Create new legacy manifest with annotation
	newLegacyManifest := *legacyManifest
	legacyManifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": legacyManifest.Config.Digest.String(),
	}
	if len(newLegacyManifest.Layers) > 0 {
		newLegacyManifest.Layers = make([]ocispec.Descriptor, len(legacyManifest.Layers))
		copy(newLegacyManifest.Layers, legacyManifest.Layers)
		if newLegacyManifest.Layers[0].Annotations == nil {
			newLegacyManifest.Layers[0].Annotations = make(map[string]string)
		}
		newLegacyManifest.Layers[0].Annotations[EROFSManifestAnnotation] = erofsManifestDesc.Digest.String()

		// Build GC labels for legacy manifest
		for i, layer := range newLegacyManifest.Layers {
			legacyManifestLabels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = layer.Digest.String()
		}
	}

	newLegacyDesc, err := writeJSON(ctx, d.cs, &newLegacyManifest, legacyDesc, legacyManifestLabels)
	if err != nil {
		return nil, nil, err
	}
	newLegacyDesc.Platform = clonePlatform(legacyPlatform)
	erofsManifestDesc.Platform = clonePlatform(legacyPlatform)
	if erofsManifestDesc.Platform != nil {
		erofsManifestDesc.Platform.OSFeatures = appendUniqueFeature(erofsManifestDesc.Platform.OSFeatures, "erofs")
	}

	return newLegacyDesc, erofsManifestDesc, nil
}

func platformFromImageConfig(img *ocispec.Image) *ocispec.Platform {
	if img == nil {
		return nil
	}
	return &ocispec.Platform{
		Architecture: img.Architecture,
		OS:           img.OS,
		OSVersion:    img.OSVersion,
		OSFeatures:   slices.Clone(img.OSFeatures),
		Variant:      img.Variant,
	}
}

func clonePlatform(platform *ocispec.Platform) *ocispec.Platform {
	if platform == nil {
		return nil
	}
	cloned := *platform
	cloned.OSFeatures = slices.Clone(platform.OSFeatures)
	return &cloned
}

func appendUniqueFeature(features []string, feature string) []string {
	for _, existing := range features {
		if existing == feature {
			return features
		}
	}
	return append(features, feature)
}

func readJSONWithLabels[T any](ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*T, map[string]string, error) {
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil {
		return nil, nil, err
	}
	data, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, nil, err
	}
	var obj T
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, nil, err
	}
	return &obj, info.Labels, nil
}

func writeJSON(ctx context.Context, cs content.Store, obj interface{}, refDesc ocispec.Descriptor, labels map[string]string) (*ocispec.Descriptor, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}

	dgst := digest.FromBytes(data)
	ref := fmt.Sprintf("erofs-dual-%s", dgst)

	w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
	if err != nil {
		return nil, err
	}
	if err := content.Copy(ctx, w, bytes.NewReader(data), int64(len(data)), dgst, content.WithLabels(labels)); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := ensureContentLabels(ctx, cs, dgst, labels); err != nil {
		return nil, err
	}

	mediaType := refDesc.MediaType
	if _, ok := obj.(*ocispec.Index); ok {
		mediaType = ocispec.MediaTypeImageIndex
	} else if _, ok := obj.(*ocispec.Manifest); ok {
		mediaType = ocispec.MediaTypeImageManifest
	}

	result := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    dgst,
		Size:      int64(len(data)),
		Platform:  refDesc.Platform,
	}
	return &result, nil
}
