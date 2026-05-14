---
title: Artifact
sidebar_position: 4
---

# Artifact

Blob storage with metadata. The right block for generated files —
images, audio, structured outputs, anything that's bigger than a record
and that you want to fetch back later by id.

## API

```proto
service Artifact {
  rpc Put   (stream PutRequest)  returns (PutResponse);   // client-stream
  rpc Get   (GetRequest)         returns (stream GetResponse);
  rpc Stat  (StatRequest)        returns (StatResponse);
  rpc Delete(DeleteRequest)      returns (DeleteResponse);
}

message PutRequest {
  oneof body {
    PutInit init = 1;    // first message
    PutChunk chunk = 2;  // every subsequent message
  }
}
```

`Put` is **client-streaming**: the first message carries `PutInit`
(namespace, optional id, content type, user metadata); every subsequent
message carries `PutChunk` bytes. The service streams those bytes through
a `TeeReader → sha256` so the response carries the **server-computed
SHA-256** and **byte count** of the upload.

`Get` is **server-streaming**: the first message carries `GetHeader`
(metadata); every subsequent message carries `GetChunk` data. Range reads
via `offset` / `length` work on every driver.

## Drivers

| Driver | Notes |
|---|---|
| `memory` | In-memory map. Lossy on restart; fine for tests. |
| `fs` | Local filesystem with `<base>/<ns>/<2-char-shard>/<id>` layout and atomic temp-file-then-rename writes. Metadata in a sibling `.json`. |
| `s3` | minio-go. Works against AWS S3, MinIO, Cloudflare R2, any S3-compatible store. Per-object metadata in `x-amz-meta-*` headers. |

The service supports an optional `metaPatcher` interface so the FS and
memory drivers persist the post-stream SHA-256 + size into their metadata
record. S3 doesn't implement it today; a future slice can do a
`CopyObject` self-copy with updated user-metadata to close that gap.

## Configuration

```yaml
backends:
  - name: blob-local
    driver: fs
    options:
      base_dir: /var/lib/memsidecar/blobs
  - name: blob-s3
    driver: s3
    options:
      endpoint: s3.amazonaws.com
      bucket: my-bucket
      use_ssl: true
      access_key_env: AWS_ACCESS_KEY_ID
      secret_key_env: AWS_SECRET_ACCESS_KEY

namespaces:
  - { block: artifact, name: blobs, backend: blob-local }
```

## gRPC example (HTTP/JSON gateway can't do client-stream)

```bash
# Encode the payload once for the multi-message JSON stream.
PAYLOAD_B64=$(base64 < ./generated.png | tr -d '\n')

printf '{"init":{"namespace":"blobs","id":"render-1","content_type":"image/png"}}\n{"chunk":{"data":"%s"}}\n' "$PAYLOAD_B64" | \
  grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" -d @ \
    127.0.0.1:7777 memsidecar.artifact.v1.Artifact/Put

grpcurl -plaintext -H "x-memsidecar-capability: Bearer $TOKEN" \
  -d '{"namespace":"blobs","id":"render-1"}' \
  127.0.0.1:7777 memsidecar.artifact.v1.Artifact/Stat
```

The `Put` response carries the SHA-256 the server computed while streaming
— use it to verify integrity client-side.

## Python example

The Python SDK wraps the streaming into a bytes-in / bytes-out helper
that chunks at 64 KiB internally:

```python
ref = m.artifact.put("blobs", open("render.png","rb").read(),
                     id="render-1", content_type="image/png")
assert ref.size == ...
data = m.artifact.get("blobs", "render-1")
```

## Op names

| Op | Method |
|---|---|
| `artifact.put` | `Artifact/Put` |
| `artifact.get` | `Artifact/Get` |
| `artifact.stat` | `Artifact/Stat` |
| `artifact.delete` | `Artifact/Delete` |

## Notes

- The HTTP/JSON gateway can't surface client-streaming RPCs — `Put` is
  gRPC-only. `Stat` / `Get` / `Delete` work over HTTP fine (server-stream
  Get emits NDJSON envelopes).
- Capability scope checks on `Put` happen after the first message
  arrives (since that's where the namespace is); a token without scope is
  rejected before any payload bytes are written to the driver.
