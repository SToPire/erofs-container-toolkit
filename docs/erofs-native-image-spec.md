# EROFS Native Container Image Specification

## 1. Introduction

This document specifies the format and conventions for native EROFS container
images that are fully compliant with the
[OCI Image Specification](https://github.com/opencontainers/image-spec).

EROFS (Enhanced Read-Only File System) is a high-performance, read-only
filesystem designed for resource-efficient container image storage and
distribution.  A **native EROFS image** stores each filesystem layer directly
in EROFS format rather than in the traditional tar/tar+gzip archive format,
enabling the container runtime to mount the layer directly without unpacking.

### 1.1 Goals

* Full compliance with the OCI Image Specification.
* Seamless coexistence with legacy OCI/Docker images within the same image
  index so that existing toolchains remain functional.
* Transparent fallback: clients unaware of EROFS MUST be able to pull and run
  the legacy variant without any modification.
* Support for both patched containerd (native EROFS layer support) and
  unpatched containerd (via the external `containerd-erofs-grpc` proxy
  snapshotter).

### 1.2 Terminology

| Term | Definition |
|------|-----------|
| **Native EROFS image** | An OCI image whose layer blobs are EROFS filesystem images instead of tar archives. |
| **Legacy OCI image** | A conventional OCI image using `application/vnd.oci.image.layer.v1.tar` (or compressed variants) as layer media types. |
| **Image index** | An OCI [Image Index](https://github.com/opencontainers/image-spec/blob/main/image-index.md) (`application/vnd.oci.image.index.v1+json`) that may reference multiple manifests for different platforms or image variants. |
| **Proxy snapshotter** | The `containerd-erofs-grpc` component that implements the containerd snapshotter and differ gRPC interfaces to handle EROFS layers on behalf of containerd. |

## 2. Image Index Layout

A native EROFS image MUST be published behind an OCI Image Index so that it
can coexist with its legacy OCI counterpart.  The index enables runtime
toolchains to select the appropriate manifest variant.

### 2.1 Identifying the EROFS Manifest via `os.features`

The OCI Image Index specification defines a
[`platform`](https://github.com/opencontainers/image-spec/blob/main/image-index.md#image-index-property-descriptions)
object on each manifest descriptor.  The `platform` object includes an
`os.features` field (array of strings).

An EROFS native manifest MUST be identified by the presence of the string
`"erofs"` in the `os.features` array of its platform descriptor within the
image index:

```jsonc
{
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "size": 8213,
  "digest": "sha256:...",
  "platform": {
    "architecture": "amd64",
    "os": "linux",
    "os.features": ["erofs"]
  }
}
```

All other `platform` fields (`architecture`, `os`, `os.version`, `variant`)
MUST match those of the corresponding legacy OCI manifest so that the EROFS
variant can be unambiguously associated with its legacy counterpart.

### 2.2 Manifest Ordering

To ensure backward compatibility, the ordering of manifests inside the image
index MUST follow this rule:

> **For each platform, the legacy OCI manifest MUST appear before the
> corresponding native EROFS manifest.**

This guarantees that container runtimes and tools that do not understand EROFS
will select the first matching manifest for a given platform and architecture,
which will be the legacy OCI variant.

#### Example Image Index

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "size": 7143,
      "digest": "sha256:aaa...",
      "platform": {
        "architecture": "amd64",
        "os": "linux"
      }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "size": 8213,
      "digest": "sha256:bbb...",
      "platform": {
        "architecture": "amd64",
        "os": "linux",
        "os.features": ["erofs"]
      }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "size": 7682,
      "digest": "sha256:ccc...",
      "platform": {
        "architecture": "arm64",
        "os": "linux"
      }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "size": 8421,
      "digest": "sha256:ddd...",
      "platform": {
        "architecture": "arm64",
        "os": "linux",
        "os.features": ["erofs"]
      }
    }
  ]
}
```

In the example above, for `linux/amd64` the legacy manifest (`sha256:aaa...`)
precedes the EROFS manifest (`sha256:bbb...`), and likewise for `linux/arm64`.

## 3. Image Manifest

An EROFS native image manifest is a standard
[OCI Image Manifest](https://github.com/opencontainers/image-spec/blob/main/manifest.md)
(`application/vnd.oci.image.manifest.v1+json`).

### 3.1 Layer Media Type

Each layer in an EROFS native manifest MUST use the following media type:

```
application/vnd.erofs.layer.v1
```

This media type indicates that the blob is a raw EROFS filesystem image.

### 3.2 Config

The image config MUST conform to the
[OCI Image Configuration](https://github.com/opencontainers/image-spec/blob/main/config.md)
specification.  The `rootfs.diff_ids` entries MUST correspond to the diffIDs
of the EROFS layer blobs (i.e., the digest of the uncompressed EROFS blob).

### 3.3 Example EROFS Manifest

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "size": 1470,
    "digest": "sha256:eee..."
  },
  "layers": [
    {
      "mediaType": "application/vnd.erofs.layer.v1",
      "size": 10485760,
      "digest": "sha256:fff..."
    },
    {
      "mediaType": "application/vnd.erofs.layer.v1",
      "size": 2097152,
      "digest": "sha256:111..."
    }
  ]
}
```

## 4. Legacy Manifest Annotations for Proxy Snapshotter Discovery

When a native EROFS image coexists with a legacy OCI image in the same index,
older versions of containerd do not recognise the `application/vnd.erofs.layer.v1` layer
media type and will refuse to process the EROFS manifest.  To support these
environments, the external **proxy snapshotter** (`containerd-erofs-grpc`) can
transparently fetch and mount EROFS layers on behalf of containerd.

To enable this, the **legacy OCI manifest** that accompanies an EROFS native
manifest MUST carry a special annotation on the **first** layer so that the
proxy snapshotter can detect the existence of a corresponding EROFS variant and
handle it accordingly.  Only the first layer needs to carry this annotation
because the proxy snapshotter, upon encountering it, fetches the full EROFS
native manifest and can then map every subsequent layer by positional index.

### 4.1 Annotation Key

The annotation key used on the **first** layer of the legacy OCI manifest is:

```
containerd.io/snapshot/erofs.native-image.manifest.digest
```

### 4.2 Annotation Value

The value MUST be the digest of the corresponding EROFS native manifest in the
same image index, expressed as an OCI content-addressable digest string (e.g.,
`sha256:bbb...`).

### 4.3 Example: Annotated Legacy Manifest

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "size": 1470,
    "digest": "sha256:222..."
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "size": 5242880,
      "digest": "sha256:333...",
      "annotations": {
        "containerd.io/snapshot/erofs.native-image.manifest.digest": "sha256:bbb..."
      }
    },
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "size": 1048576,
      "digest": "sha256:444..."
    }
  ]
}
```

Note that only the first layer carries the annotation; subsequent layers do not.

### 4.4 Proxy Snapshotter Workflow

When `containerd-erofs-grpc` receives a `Prepare()` request from containerd for
a legacy OCI layer:

1. On the **first** layer, it inspects the annotation
   `containerd.io/snapshot/erofs.native-image.manifest.digest`.
2. If the annotation is present, the proxy snapshotter fetches the referenced
   EROFS native manifest from the registry (or from the local content store if
   already available) and caches it for subsequent layers.
3. For every layer (including the first), it determines the corresponding EROFS
   layer by positional index within the EROFS manifest's layer array.
4. It resolves credentials, prepares a per-instance lazy-access config for the
   shared daemon, and materializes a committed snapshot under the legacy
   `chainID`.
5. At runtime, the mount path reads blob data lazily through that shared
   daemon.

This process is entirely transparent to containerd — from its perspective it is
simply unpacking a normal OCI image.

## 5. Deployment Scenarios

### 5.1 Containerd with Native EROFS Support

When containerd natively supports the `application/vnd.erofs.layer.v1` layer media type
(containerd v2.3+ with the EROFS media type upstreamed, or earlier
versions with the relative patch applied):

1. The client queries the image index and selects the manifest whose
   `platform.os.features` contains `"erofs"`.
2. Containerd pulls the EROFS manifests and layer descriptors directly.
3. The EROFS snapshotter prepares mounts from the raw EROFS layer blobs.

### 5.2 Legacy Containerd with Proxy Snapshotter

When containerd does NOT natively support EROFS layers:

1. The client queries the image index and — because the legacy manifest
   appears first — selects the legacy OCI manifest for the target platform.
2. Containerd begins unpacking layers via the configured proxy snapshotter
   (`containerd-erofs-grpc`).
3. On the first layer, the proxy snapshotter reads the
   `containerd.io/snapshot/erofs.native-image.manifest.digest` annotation.
4. It fetches the referenced EROFS native manifest if needed, then prepares a
   lazy blob instance in the shared daemon for the selected EROFS layer.
5. It commits the snapshot under the legacy `chainID` and returns the normal
   remote-snapshotter signal so containerd skips legacy tar pull/apply.
6. Later runtime mounts read blob data lazily through the shared daemon.

### 5.3 Clients Without EROFS Support

Clients that have no knowledge of EROFS at all (no proxy snapshotter, no
EROFS plugin) will:

1. Select the first matching manifest for their platform — the legacy OCI
   manifest.
2. Pull and unpack tar-based layers as usual.
3. The layer annotations are ignored since no EROFS-aware snapshotter is
   present.

## 6. Conversion

The `ctr-erofs` tool converts existing OCI/Docker images into native EROFS
images.  During conversion, the tool:

1. Pulls the source image.
2. Decompresses each tar layer (if compressed).
3. Invokes `mkfs.erofs --tar=f` to convert the tar stream into an EROFS
   filesystem image.
4. Produces a new manifest with `application/vnd.erofs.layer.v1` layers.
5. (Optionally) Assembles an image index containing both the original legacy
   manifest and the new EROFS manifest, following the ordering and annotation
   conventions described in this specification.

### 6.1 Example

```bash
# Convert an OCI image, producing a multi-manifest index with both legacy
# and EROFS variants:
ctr-erofs image convert \
  --erofs --oci \
  --erofs-compressors "deflate,9" \
  example.com/myimage:latest \
  example.com/myimage:latest
```

## 7. Media Type Summary

| Media Type | Description |
|-----------|-------------|
| `application/vnd.oci.image.index.v1+json` | Image index referencing legacy and EROFS manifests |
| `application/vnd.oci.image.manifest.v1+json` | Image manifest (used by both legacy and EROFS variants) |
| `application/vnd.oci.image.config.v1+json` | Image configuration |
| `application/vnd.oci.image.layer.v1.tar` | Legacy uncompressed tar layer |
| `application/vnd.oci.image.layer.v1.tar+gzip` | Legacy gzip-compressed tar layer |
| `application/vnd.erofs.layer.v1` | Native EROFS filesystem layer |

## 8. Annotation Summary

| Annotation Key | Scope | Value | Purpose |
|---------------|-------|-------|---------|
| `containerd.io/snapshot/erofs.native-image.manifest.digest` | First layer (legacy manifest) | Digest of the EROFS native manifest | Enables the proxy snapshotter to discover and resolve the corresponding EROFS manifest |

## 9. Security Considerations

* All digest-based references provide content integrity verification consistent
  with the OCI specification.
* The proxy snapshotter and its shared daemon MUST verify the digest of EROFS
  layer data when it is fetched from the registry, exactly as containerd does
  for standard layers.
* Registry authentication and transport security (TLS) apply identically to
  EROFS blobs.

## Appendix A: Reference Implementations

* **`ctr-erofs`** — CLI tool for converting OCI images to EROFS native format.
* **`containerd-erofs-grpc`** — Proxy snapshotter and differ for containerd,
  enabling transparent EROFS layer support without modifying containerd itself.
* **containerd EROFS snapshotter plugin** — Native EROFS support in
  containerd v2.1+.
