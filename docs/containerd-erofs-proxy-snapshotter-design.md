# Design of `containerd-erofs-grpc` for Legacy containerd Integration

## 1. Introduction

This document describes the design of the `containerd-erofs-grpc` proxy
snapshotter for integrating the EROFS native image format with containerd
versions that do not understand the EROFS native layer media type.

The target image format is defined in `docs/erofs-native-image-spec.md`.
In particular, Section 4.4 defines the proxy snapshotter workflow for legacy
containerd deployments.

This design focuses on the snapshotter-side behavior of `Prepare()`, `View()`,
`Commit()`, `Stat()`, `Mounts()`, and related snapshotter interfaces, plus the
minimum shared-daemon contract required for lazy blob access. It does **not**
define:

- full lifecycle policy for the image-pulling daemon beyond the shared-daemon
  contract used by `BlobProvider`;
- registry authentication and credential management;
- long-running background prefetch, cache warming, or GC policy;
- CRI integration details outside the snapshotter contract itself.

## 2. Problem Statement

The EROFS native image format publishes, for each platform, both:

- a legacy OCI manifest using tar-based layer media types; and
- an EROFS native manifest using `application/vnd.erofs.layer.v1`.

On legacy containerd, the runtime selects the legacy manifest because the EROFS
native layer media type is unknown. During unpack, containerd therefore computes
`diffIDs` and `chainIDs` from the **legacy image configuration referenced by the
selected legacy manifest**, and it drives the snapshotter as if it were unpacking
ordinary tar layers.

The proxy snapshotter must preserve this control-plane behavior while changing
only the data plane:

- containerd must continue to use the legacy manifest and its `chainID`s;
- the committed snapshots created under those `chainID`s must actually be backed
  by EROFS layer blobs;
- the unpack path must remain transparent to containerd.

In other words, the proxy snapshotter must make a legacy `chainID` resolve to an
EROFS-backed committed snapshot.

## 3. Goals

The proxy snapshotter design has the following goals:

- integrate with unmodified containerd through the existing snapshotter and
  remote-snapshotter contracts;
- consume the discovery annotation defined in
  `docs/erofs-native-image-spec.md` Section 4;
- avoid pulling and unpacking legacy tar layers when a corresponding EROFS
  native manifest is available;
- preserve the legacy `chainID` namespace expected by containerd;
- reuse the existing EROFS snapshotter mount model as much as possible;
- keep the first implementation simple, deterministic, and compatible with the
  current image-format design.

## 4. Non-Goals

This design does **not** attempt to:

- change how legacy containerd chooses a manifest from an image index;
- make legacy containerd natively understand EROFS layer media types;
- replace containerd's `diffID` or `chainID` computation;
- support parallel unpack in the first iteration;
- define a generic remote fetching service API beyond the minimum provider
  abstraction required by the snapshotter.

## 5. Background: What containerd already computes

When unpacking the selected image manifest, containerd reads the image config,
extracts `rootfs.diff_ids`, and computes the per-layer `chainID`s before calling
the snapshotter.

For legacy containerd in this deployment model, the selected manifest is the
**legacy manifest**, so the relevant values are:

- `diffIDs` from the legacy image config; and
- `chainIDs` derived from those legacy `diffIDs`.

The snapshotter does not need to calculate these values itself. Instead,
containerd passes the target `chainID` to the snapshotter using the standard
remote-snapshotter label:

- `containerd.io/snapshot.ref`

This is the authoritative committed-snapshot name for the layer currently being
prepared.

As a consequence, the proxy snapshotter must **commit snapshots under the legacy
`chainID`**, even though the underlying layer data comes from the EROFS native
manifest.

## 6. Image Format Preconditions

This design relies on the image format conventions already defined in
`docs/erofs-native-image-spec.md`:

1. For each platform, the legacy manifest appears before the EROFS native
   manifest in the image index.
2. The first layer of the legacy manifest carries the annotation:

   `containerd.io/snapshot/erofs.native-image.manifest.digest`

3. The annotation value is the digest of the corresponding EROFS native
   manifest.
4. The EROFS native manifest contains the same number of layers as the legacy
   manifest, and layers are positionally aligned.

The converter in `pkg/converter/dual_manifest.go` already writes this
annotation onto the first layer of the legacy manifest.

## 7. Key Decisions

### 7.1 Do not export the `rebase` capability

The first implementation of `containerd-erofs-grpc` MUST NOT export the
`rebase` snapshotter capability.

Rationale:

- when `rebase` is available, containerd may prepare multiple layers in
  parallel;
- in that mode, `Prepare()` for non-first layers may observe an empty `parent`
  and only learn the real parent later at `Commit()` time;
- the Section 4.4 image-format workflow is intentionally simple: it discovers
  the EROFS manifest on the first layer and resolves later layers by position;
- disabling `rebase` forces sequential unpack, which makes `parent` available
  at `Prepare()` time for non-first layers and keeps layer-index recovery
  deterministic.

This decision is specifically about the **proxy snapshotter implementation**.
It does not change the upstream EROFS snapshotter behavior.

Future work may add `rebase` support after the proxy snapshotter can resolve a
layer index directly from per-layer labels such as manifest digest and layer
digest, without relying on the `parent` chain.

### 7.2 Preserve the legacy `chainID` namespace

The proxy snapshotter MUST use the `chainID` computed by containerd from the
legacy image config as the committed snapshot name.

Rationale:

- legacy containerd selected the legacy manifest and therefore reasons entirely
  in terms of the legacy config's `diffIDs` and derived `chainID`s;
- later snapshotter calls use those same `chainID`s as parents;
- runtime rootfs assembly also expects the unpacked snapshot graph to be keyed
  by the legacy `chainID`s.

Therefore:

- the **snapshot name** visible to containerd is the legacy `chainID`;
- the **snapshot payload** stored by the proxy snapshotter is a daemon-backed
  reference to the corresponding EROFS layer blob.

This is the central indirection performed by the proxy snapshotter.

## 8. High-Level Architecture

The proxy snapshotter is structured as three cooperating parts:

1. **Proxy control path**
   - interprets snapshot labels from containerd;
   - detects whether a layer belongs to an annotated legacy manifest;
   - resolves the corresponding EROFS native manifest and layer index;
   - resolves registry credentials in the snapshotter;
   - configures a lazy blob instance in the shared daemon;
   - materializes a committed snapshot under the legacy `chainID`;
   - returns `ErrAlreadyExists` so containerd skips legacy layer pull/apply.

2. **Shared lazy-fetch daemon**
   - is a single daemon process shared by all mounted instances;
   - receives per-instance configuration from the snapshotter;
   - performs actual registry reads for blob data on demand;
   - keeps auth scope per instance rather than as a daemon-global setting.

3. **EROFS mount backend**
   - reuses the EROFS snapshotter's on-disk layout and mount semantics;
   - serves runtime `Prepare()`, `View()`, and `Mounts()` for already committed
     EROFS-backed snapshots whose data path is backed by the shared daemon.

The control path is custom to the proxy snapshotter. The mount backend should
reuse as much as possible from the existing EROFS snapshotter implementation.

### 8.1 Shared daemon model

The first implementation assumes:

- one daemon process is shared by many mounted instances;
- each committed proxy snapshot has its own instance-specific daemon config;
- the snapshotter owns credential resolution and passes resolved credentials to
  the daemon on a per-instance basis.

This matches the intended nydus-style split for this project: the snapshotter
decides which credentials apply to a specific image layer instance, while the
daemon consumes that instance config when serving lazy reads later.

## 8.2 Proxy plugin registration

When `containerd-erofs-grpc` is registered as a containerd proxy snapshotter,
its `proxy_plugins` configuration MUST NOT advertise `rebase`.

Example:

```toml
[proxy_plugins.erofs]
  type = "snapshot"
  address = "/run/containerd-erofs-grpc/containerd-erofs-grpc.sock"
  capabilities = []
```

Omitting `capabilities` entirely is also acceptable as long as `rebase` is not
present.

## 9. Data Sources and Inputs

## 9.1 Snapshotter labels from containerd

The proxy snapshotter consumes the following labels when handling unpack-time
`Prepare()` requests:

- `containerd.io/snapshot.ref`
  - target committed snapshot name;
  - this is the legacy `chainID` calculated by containerd.
- `containerd.io/snapshot/erofs.native-image.manifest.digest`
  - present on the first legacy layer only;
  - points to the EROFS native manifest.
- `containerd.io/snapshot/cri.image-ref`
  - required whenever remote manifest fetch or lazy remote blob access is
    needed;
  - provides the registry context used for both manifest resolution and
    per-instance daemon configuration.
- `containerd.io/snapshot/cri.manifest-digest`
  - optional;
  - useful for diagnostics and future parallel-unpack support.
- `containerd.io/snapshot/cri.layer-digest`
  - optional;
  - useful for diagnostics and future parallel-unpack support.

Only `containerd.io/snapshot.ref` is required by the containerd remote
snapshotter contract. The EROFS native-manifest annotation is the format-
specific trigger defined by our image specification.

## 9.2 Local persistent state

The proxy snapshotter maintains persistent state in snapshot metadata labels so
that later layers and daemon restarts do not depend on in-memory state.

Each committed EROFS-backed snapshot created by the proxy snapshotter stores
implementation-private labels describing which EROFS object the legacy
`chainID` has been mapped to.

These labels have the following meaning:

- `containerd.io/snapshot/erofs.proxy.manifest.digest`
  - the digest of the EROFS native manifest currently associated with this
    legacy `chainID`;
  - this tells later layers which EROFS manifest must be consulted;
  - this label is required for recovering state from the previous committed
    parent snapshot.
- `containerd.io/snapshot/erofs.proxy.layer.index`
  - the positional index of the EROFS layer within that EROFS native manifest;
  - this allows the next sequential layer to derive its own position as
    `parentIndex + 1`;
  - this label is required for later-layer mapping.
- `containerd.io/snapshot/erofs.proxy.layer.digest`
  - the digest of the concrete EROFS layer blob referenced by this snapshot's
    lazy data path;
  - this is mainly for consistency checks, diagnostics, and future extensions;
  - it is not strictly required for the minimal sequential-mapping algorithm,
    but it is strongly recommended.

Normatively:

- `containerd.io/snapshot/erofs.proxy.manifest.digest`: MUST be stored;
- `containerd.io/snapshot/erofs.proxy.layer.index`: MUST be stored;
- `containerd.io/snapshot/erofs.proxy.layer.digest`: SHOULD be stored.

Taken together, these labels mean:

> this legacy `chainID` is backed by layer `i` of EROFS manifest `M`, and the
> EROFS blob referenced by its lazy data path has digest `D`.

These labels are implementation details, not part of the image format.

## 9.3 Per-instance daemon configuration

In addition to snapshot metadata labels, the proxy snapshotter maintains a
per-instance daemon configuration for each committed proxy snapshot.

Recommended contents:

- an instance identifier;
- the image reference and registry host;
- the manifest digest and selected EROFS layer descriptor;
- resolved credentials for that instance; and
- any registry host settings the daemon needs for lazy access.

Recommended properties:

- all instances share one daemon process;
- instance configs are isolated from one another;
- instance configs are persisted under the snapshotter root so daemon restart
  recovery does not depend on in-memory state alone.

## 9.4 Local transient cache

An in-memory cache MAY be used to avoid repeatedly reading and parsing the same
EROFS manifest.

Recommended cache key:

- EROFS native manifest digest

Recommended cache value:

- parsed OCI manifest object;
- optionally, prevalidated positional mapping metadata.

The cache is purely an optimization. Correctness must not depend on it.

## 10. Provider Abstraction

The snapshotter needs a way to obtain:

- the EROFS native manifest by digest; and
- a lazy-access handle for the EROFS layer blob referenced by that manifest.

The first implementation defines the following provider roles:

- `ManifestProvider`
  - get a manifest by digest, preferring containerd's local content store;
  - if missing, fetch it from the registry using credentials resolved by the
    snapshotter.
- `BlobProvider`
  - do **not** eagerly fetch the blob during `Prepare()`;
  - instead, configure the shared daemon for one concrete lazy blob instance and
    return the stub / reference that the snapshot backend should store.

The first implementation should follow this policy:

1. check containerd's local content store for manifests;
2. if a manifest is missing, fetch it remotely;
3. for blobs, create or refresh per-instance daemon state and return a lazy
   reference rather than downloading the blob.

The credential-management design for authenticating against remote registries
and for handing credentials to the shared daemon is described in
`docs/proxy-snapshotter-credential-design.md`.

## 11. Snapshotter Behavior by Interface

### 11.1 `Prepare(ctx, key, parent, opts...)`

`Prepare()` is the most important interface in this design.

It has two operating modes.

#### Mode A: unpack-time remote-snapshot path

This mode is selected when the labels contain:

- `containerd.io/snapshot.ref`

In this mode, the proxy snapshotter attempts to materialize a committed snapshot
for the target legacy `chainID` before containerd pulls or applies the legacy
layer.

##### Step 1: Read the target `chainID`

The snapshotter reads:

- `targetChainID = labels["containerd.io/snapshot.ref"]`

If a committed snapshot with that name already exists, `Prepare()` returns
`ErrAlreadyExists` immediately.

##### Step 2: Determine whether this layer belongs to an annotated legacy image

There are two cases.

**Case A: first layer of the legacy manifest**

- the incoming labels contain
  `containerd.io/snapshot/erofs.native-image.manifest.digest`;
- this value is the EROFS native manifest digest;
- the layer index is `0`.

**Case B: non-first layer**

- the incoming labels do not contain the EROFS manifest annotation;
- `parent` is expected to be the previous committed legacy `chainID` because the
  proxy snapshotter does not export `rebase`;
- the snapshotter loads the parent's internal labels:
  - `containerd.io/snapshot/erofs.proxy.manifest.digest`
  - `containerd.io/snapshot/erofs.proxy.layer.index`
- the current layer index is `parentIndex + 1`.

If neither case applies, the snapshotter MUST treat the layer as a normal legacy
unpack and fall back to the regular EROFS snapshotter `Prepare()` behavior.
This makes the proxy opt-in and preserves compatibility with ordinary images.

##### Step 3: Resolve and validate the EROFS native manifest

The snapshotter resolves the EROFS native manifest using the manifest digest
obtained in Step 2.

Validation requirements:

- the digest must reference an OCI image manifest;
- every mapped layer must use `application/vnd.erofs.layer.v1`;
- the requested positional layer index must exist.

If the EROFS manifest is malformed or inconsistent with the positional mapping
rules, `Prepare()` MUST fail.

##### Step 4: Resolve the target EROFS layer

The snapshotter selects:

- `erofsLayer = erofsManifest.layers[layerIndex]`

The selected layer blob is then handed to the blob provider so it can prepare a
lazy-access instance in the shared daemon.

##### Step 5: Configure the shared daemon for lazy access

The blob provider prepares per-instance daemon state for the selected layer.

Implementation requirements:

- resolve credentials in the snapshotter, not in the daemon;
- create or refresh one daemon instance config for this committed snapshot;
- scope registry credentials to that instance only;
- return a stub or reference object that the snapshot backend can persist at the
  normal layer location.

No eager blob download happens in this step.

##### Step 6: Materialize a committed snapshot under the legacy `chainID`

The proxy snapshotter creates a committed snapshot whose **name** is the legacy
`targetChainID`, but whose **payload** is a lazy reference to the EROFS layer
blob selected in Step 4 and prepared in Step 5.

Implementation requirements:

- reuse the EROFS snapshotter on-disk layout for committed snapshots;
- place the daemon-backed blob stub or reference at the committed snapshot's
  `layer.erofs` location;
- record zero or estimated usage rather than assuming the full blob is present
  locally;
- persist internal labels containing manifest digest and layer index.

The active-snapshot transaction used during materialization is purely internal
and should not survive successful completion.

##### Step 7: Return `ErrAlreadyExists`

Once the committed snapshot under `targetChainID` exists, `Prepare()` returns
`ErrAlreadyExists`.

This is the standard remote-snapshotter signal telling containerd that the
committed snapshot already exists and that pull/apply of the current legacy
layer can be skipped.

#### Mode B: ordinary snapshotter path

If `containerd.io/snapshot.ref` is absent, the request is not part of the
remote-snapshot unpack flow.

In this case, `Prepare()` behaves like the normal EROFS snapshotter `Prepare()`.
This covers, for example, runtime writable snapshots created on top of already
committed parent snapshots.

### 11.2 `View(ctx, key, parent, opts...)`

`View()` should reuse the standard EROFS snapshotter semantics.

Expected behavior:

- if `parent` is an EROFS-backed committed snapshot already materialized by the
  proxy path, `View()` returns the normal readonly EROFS/overlay mount stack
  backed by that snapshot's daemon instance;
- no special remote-unpack optimization is required here.

The proxy logic may inspect labels for diagnostics, but the first design does
not require `View()` to perform manifest discovery.

### 11.3 `Commit(ctx, name, key, opts...)`

For the proxy-unpack path, successful `Prepare()` returns `ErrAlreadyExists`, so
containerd skips legacy layer apply and normally never calls `Commit()` for that
layer.

Therefore, `Commit()` only needs to support the ordinary EROFS snapshotter path,
for example for writable runtime layers.

Expected behavior:

- delegate to the EROFS snapshotter's normal commit logic;
- do not invent a new proxy-specific unpack-time commit path through this
  interface.

### 11.4 `Stat(ctx, key)`

`Stat()` must report committed snapshots under the legacy `chainID` names that
containerd expects.

For proxy-created snapshots, `Stat()` should expose:

- the committed snapshot kind;
- internal labels required for subsequent layer mapping;
- usage derived from the persisted stub or from an explicit estimate.

`Stat()` is critical because containerd calls it after `Prepare()` returns
`ErrAlreadyExists` in order to verify that the target committed snapshot is
present.

### 11.5 `Mounts(ctx, key)`

`Mounts()` should behave like the normal EROFS snapshotter `Mounts()` for active
or readonly snapshots.

Once a committed snapshot has been materialized under the legacy `chainID`, the
runtime path does not care that the content originated from an EROFS native
manifest. It only needs correct EROFS-backed mounts whose data path resolves
through the shared daemon.

### 11.6 `Remove(ctx, key)`

`Remove()` should:

- delete the snapshot metadata and on-disk state through the normal EROFS
  snapshotter path;
- revoke the corresponding per-instance daemon state when present; and
- drop any optional in-memory cache entries that are no longer useful.

Manifest caches are best-effort optimizations. `Remove()` correctness must not
depend on cache eviction.

### 11.7 `Usage()`, `Update()`, `Walk()`, and `Close()`

These interfaces can be thin wrappers or direct delegation to the underlying
EROFS snapshotter implementation.

## 12. On-Disk Layout

The proxy snapshotter should reuse the existing EROFS snapshotter layout and
mount logic rather than inventing a second format.

In particular, the committed snapshot payload should still be represented as an
object at the normal committed-layer location, for example:

- `snapshots/<id>/layer.erofs`

For proxy-created snapshots, that object is a daemon-backed stub or reference,
not necessarily the full EROFS blob downloaded during unpack.

This allows the runtime mount path to remain aligned with the existing EROFS
snapshotter implementation while keeping blob access fully lazy.

The proxy-specific logic is therefore concentrated in:

- how a committed snapshot's lazy reference is materialized during unpack; and
- how the EROFS layer is resolved and registered with the shared daemon from
  legacy-manifest metadata.

## 13. Layer-Mapping Rules

The layer mapping algorithm for the first implementation is intentionally simple
and follows the image specification.

### 13.1 First layer

The proxy snapshotter reads the first-layer annotation:

- `containerd.io/snapshot/erofs.native-image.manifest.digest`

It resolves the EROFS manifest and selects layer index `0`.

### 13.2 Later layers

For every later layer:

- `parent` identifies the previously committed legacy `chainID`;
- the snapshotter loads the parent's internal proxy labels;
- it recovers the EROFS manifest digest and previous layer index;
- it sets `currentIndex = previousIndex + 1`.

This design works because the proxy snapshotter does not export `rebase`, so
containerd prepares layers sequentially and supplies the previous committed
layer as `parent`.

## 14. Error Handling and Fallback Policy

The proxy snapshotter should distinguish between three classes of cases.

### 14.1 No opt-in metadata present

Examples:

- first layer has no EROFS manifest annotation;
- later layer has no parent proxy labels.

Behavior:

- fall back to the ordinary snapshotter path.

This keeps non-annotated legacy images working unchanged.

### 14.2 Opt-in metadata present but invalid

Examples:

- the manifest digest is malformed;
- the referenced object is not an OCI manifest;
- the positional index is out of range;
- the mapped layer media type is not `application/vnd.erofs.layer.v1`.

Behavior:

- fail the snapshotter request.

This indicates an image-format or conversion bug and should not silently fall
back after the image has explicitly opted into the proxy path.

### 14.3 Required EROFS content temporarily unavailable

Examples:

- the manifest is not in local content store and `ManifestProvider` cannot fetch
  it;
- the selected EROFS blob cannot be registered with the shared daemon;
- the snapshot-local blob stub cannot be created.

Behavior:

- return an error from `Prepare()`.

The first implementation prioritizes correctness and explicit failure over
silent divergence once the proxy path has been selected.

## 15. Detailed `Prepare()` Pseudocode

```go
func (s *containerdErofsGRPCSnapshotter) Prepare(ctx, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    info := snapshots.Info{}
    for _, opt := range opts {
        if err := opt(&info); err != nil {
            return nil, err
        }
    }
    labels := info.Labels

    targetChainID := labels["containerd.io/snapshot.ref"]
    if targetChainID == "" {
        return s.erofs.Prepare(ctx, key, parent, opts...)
    }

    if _, err := s.Stat(ctx, targetChainID); err == nil {
        return nil, errdefs.ErrAlreadyExists
    } else if !errdefs.IsNotFound(err) {
        return nil, err
    }

    manifestDigest := labels["containerd.io/snapshot/erofs.native-image.manifest.digest"]
    layerIndex := -1

    if manifestDigest != "" {
        layerIndex = 0
    } else if parent != "" {
        parentInfo, err := s.Stat(ctx, parent)
        if err != nil {
            return nil, err
        }
        manifestDigest = parentInfo.Labels["containerd.io/snapshot/erofs.proxy.manifest.digest"]
        parentIndexStr := parentInfo.Labels["containerd.io/snapshot/erofs.proxy.layer.index"]
        if manifestDigest != "" && parentIndexStr != "" {
            parentIndex := atoi(parentIndexStr)
            layerIndex = parentIndex + 1
        }
    }

    if manifestDigest == "" || layerIndex < 0 {
        return s.erofs.Prepare(ctx, key, parent, opts...)
    }

    mfst, err := s.manifestProvider.Get(ctx, manifestDigest, labels)
    if err != nil {
        return nil, err
    }

    desc, err := validateAndSelectErofsLayer(mfst, layerIndex)
    if err != nil {
        return nil, err
    }

    blobPath, usage, err := s.blobProvider.Materialize(ctx, desc, labels)
    if err != nil {
        return nil, err
    }

    if err := s.materializeCommittedSnapshot(ctx, targetChainID, blobPath, usage, map[string]string{
        "containerd.io/snapshot/erofs.proxy.manifest.digest": manifestDigest,
        "containerd.io/snapshot/erofs.proxy.layer.index":      strconv.Itoa(layerIndex),
        "containerd.io/snapshot/erofs.proxy.layer.digest":     desc.Digest.String(),
    }); err != nil {
        return nil, err
    }

    return nil, errdefs.ErrAlreadyExists
}
```

The exact mechanics of `materializeCommittedSnapshot()` are
implementation-specific, but it must create a committed snapshot that is
mountable by the normal EROFS snapshotter path and visible under the legacy
`targetChainID`. For proxy-created snapshots, that means persisting the
daemon-backed lazy reference rather than downloading the full blob during
`Prepare()`.

## 16. Expected End-to-End Flow

### 16.1 First layer of an annotated legacy manifest

1. containerd selects the legacy manifest.
2. containerd reads the legacy image config and computes legacy `chainID`s.
3. containerd calls `Prepare()` with:
   - `containerd.io/snapshot.ref = <legacy chainID for layer 0>`
   - `containerd.io/snapshot/erofs.native-image.manifest.digest = <erofs manifest digest>`
4. the proxy snapshotter resolves EROFS layer `0`.
5. it configures a lazy blob instance in the shared daemon.
6. it materializes a committed snapshot named by the legacy `chainID`.
7. it returns `ErrAlreadyExists`.
8. containerd verifies the snapshot with `Stat(<legacy chainID>)` and skips
   legacy tar pull/apply.

### 16.2 Later layers

1. containerd calls `Prepare()` sequentially for the next legacy layer.
2. `parent` is the previous committed legacy `chainID`.
3. the proxy snapshotter reads the parent's internal labels.
4. it increments the previous EROFS layer index.
5. it configures the next lazy blob instance in the shared daemon.
6. it materializes the next committed snapshot under the next legacy `chainID`.
7. it returns `ErrAlreadyExists`.

### 16.3 Runtime rootfs setup

1. the image has already been unpacked into committed snapshots named by legacy
   `chainID`s.
2. runtime `Prepare()` or `View()` operates on those committed snapshots.
3. the EROFS snapshotter mount path produces the correct EROFS/overlay mount
   stack.

At runtime, containerd does not need to know that the committed snapshots came
from an EROFS native manifest.

## 17. Why the Design is Transparent to containerd

From containerd's perspective:

- it selected a normal legacy manifest;
- it computed normal legacy `chainID`s;
- it asked the snapshotter for snapshots keyed by those `chainID`s;
- the snapshotter reported that the committed snapshots already existed;
- later runtime mount operations succeeded normally.

The only component that understands the EROFS native manifest indirection is the
proxy snapshotter.

## 18. Future Work

The following improvements are intentionally deferred:

- exporting `rebase` after implementing parent-free layer-index discovery;
- parallel unpack support;
- stronger integrity cross-checks between legacy and EROFS manifests;
- smarter manifest and blob cache eviction policies;
- configurable fallback policies for transient provider failures;
- explicit observability counters for proxy hits, fallback hits, and materialize
  latency.

## 19. Summary

The first version of `containerd-erofs-grpc` should be implemented as a proxy
snapshotter that:

- does **not** export `rebase` capability;
- relies on the first-layer annotation defined by the image specification;
- recovers later-layer mapping from the previous committed parent snapshot;
- commits snapshots under the **legacy `chainID`** namespace computed by
  containerd from the legacy image config;
- stores daemon-backed EROFS layer references as the actual payload of those
  snapshots;
- returns `ErrAlreadyExists` from unpack-time `Prepare()` once the EROFS-backed
  committed snapshot has been materialized.

This design preserves compatibility with legacy containerd while allowing the
snapshotter to serve EROFS native layers transparently.
