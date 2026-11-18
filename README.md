# cas-storage

A small, immutable, content-addressed file store over HTTP. The id of an
object is the SHA-256 of its content, so storing the same bytes twice keeps
one copy. Objects live in a local directory (e.g. a Kubernetes volume), split
into namespaces reached with API keys; apart from simple `key=value` labels,
the metadata about what an object means belongs to the caller.

## API

The full description is the OpenAPI 3.1 document
[`api/openapi.yaml`](api/openapi.yaml); a test keeps it in line with the
routes.

Every request except `GET /healthz` needs an API key, sent as
`X-API-Key: <key>` or `Authorization: Bearer <key>`. A key belongs to one
namespace and carries permissions (see [Configuration](#configuration));
a missing or unknown key gets `401` (with `WWW-Authenticate: Bearer`), a key
without the route's permission gets `403`. All other responses are as
before; every route works on the key's namespace only.

| Method | Path | Permission | Result |
|---|---|---|---|
| `POST` | `/objects` | `CREATE` | stores the request body; `201` new / `200` already present; JSON `{"hash","size","existed"}`, `Location: /objects/<hash>`; `413` over the namespace's `max_upload_size` |
| `GET` | `/objects/{hash}` | `READ` | streams the object (Range and conditional requests supported); `404` if absent |
| `HEAD` | `/objects/{hash}` | `READ` | existence and size |
| `DELETE` | `/objects/{hash}?unused_before=<RFC 3339>` | `DELETE` | only with `STORAGE_ALLOW_DELETE=true`: removes the object (and its labels) unless it was put at or after that time; `200 {"size","deleted"}`, `404` if absent |
| `GET` | `/objects/{hash}/labels` | `READ` | `200 {"key":"value",...}` (`{}` when none); `404` if absent |
| `PUT` | `/objects/{hash}/labels/{key}` | `LABEL` | sets the label; the body is the value (raw text); `204`; `404` if absent |
| `DELETE` | `/objects/{hash}/labels/{key}` | `LABEL` | removes the label; `204` (also when it was not set); `404` if absent |
| `GET` | `/objects?label=k=v&label=k2` | `READ` | `200 {"hashes":[...]}`, sorted: objects carrying every given label (`label=k` matches any value); at least one `label` required |
| `GET` | `/healthz` | none | liveness |

Labels: key `^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$` (may contain `/`), value
1–256 bytes of UTF-8 without NUL, at most 64 labels per object. A bad key, value or
selector gets `400`. Labels are only stored and searched; they do not change
the object.

```sh
H='X-API-Key: dev-key-0123456789abcdef'
curl -s -H "$H" --data-binary @report.pdf http://localhost:8080/objects
curl -s -H "$H" -o report.pdf http://localhost:8080/objects/<hash>
curl -s -H "$H" -X PUT --data-raw pdf http://localhost:8080/objects/<hash>/labels/kind
curl -s -H "$H" 'http://localhost:8080/objects?label=kind=pdf'
```

## How it stores

Each namespace is a separate store under its own directory (`<root>`, by
default `<STORAGE_DIR>/<namespace>`); namespaces share nothing, so the same
content put in two namespaces is stored twice.

The blobs go through a storage provider (`cas.Backend`: import, open, stat,
touch, remove, walk). The only provider today is the file system
(`cas.FSBackend`, `<root>/objects`); hashing, deduplication, compression,
labels and locking sit above it in `cas.Store`, so another provider (e.g. S3)
only has to store bytes. Uploads are staged and labels kept under `<root>`
whatever the provider.

1. The body is streamed to `<root>/tmp` while its SHA-256 is computed, then synced.
2. It is renamed atomically to `<root>/objects/ab/cd/<hash>` (two levels of 2-hex shards).
3. A put of an object that already exists only refreshes its mtime. `DELETE`
   compares `unused_before` with that mtime, so a caller that re-puts an
   object protects it from a concurrent prune.
4. Leftovers in `tmp/` are removed at start.
5. Labels live in the bbolt database `<root>/labels.db`: a bucket of the
   labels by hash and an index bucket (`key\0value\0hash`) that the search
   scans by prefix. Concurrent label changes share one commit. Label files of
   the earlier `<root>/labels/ab/cd/<hash>.json` layout are imported at start
   and removed.

### Compression

With `STORAGE_COMPRESS=true`, text objects (plain text, HTML, XML, JSON;
1 KiB–4 GiB) are kept gzipped as `<hash>.gz` when that saves at least 10%.
The hash is still that of the uncompressed bytes and reads return them
unchanged. Objects already on disk are compressed in the background at start.
A Range read of a compressed object decompresses from its beginning.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `STORAGE_CONFIG` | `/etc/cas-storage/config.yaml` | namespaces and API keys (YAML) |
| `STORAGE_DIR` | `/data` | parent of the namespace directories |
| `LISTEN_ADDR` | `:8080` | HTTP address |
| `STORAGE_ALLOW_DELETE` | `false` | enable `DELETE /objects/{hash}` |
| `STORAGE_COMPRESS` | `false` | gzip text objects |

The config file:

```yaml
max_upload_size: 500MB      # optional, the default; 0 = no limit
namespaces:
  docs:
    path: /data/docs        # optional, default <STORAGE_DIR>/docs
    max_upload_size: 2GiB   # optional, overrides the global limit
  images: {}
keys:
  - key: "a-long-random-secret-1"
    namespace: docs
    permissions: [READ, CREATE, DELETE, LABEL]
  - key: "a-long-random-secret-2"
    namespace: images
    permissions: [READ]
```

1. Namespace names match `^[a-z0-9][a-z0-9_-]{0,62}$`; two namespaces may not
   share a path.
2. Keys are unique, at least 16 characters, name an existing namespace and at
   least one of `READ`, `CREATE`, `DELETE`, `LABEL`.
3. A malformed or invalid file stops the start. A missing file, or one with no
   keys, is logged as a warning and every request gets `401`.
4. Keys are never logged; the request log names the namespace.
5. `max_upload_size` caps a `POST /objects` body (`413` above it), globally
   and per namespace. It is a number of bytes or a size with a unit: `B`,
   `KB`, `MB`, `GB`, `TB` (powers of 1000) or `KiB`, `MiB`, `GiB`, `TiB`
   (powers of 1024). Default `500MB`; `0` means no limit.

Objects stored before namespaces existed sit in `<STORAGE_DIR>/objects`; to
keep serving them, give one namespace `path: <STORAGE_DIR>`. A namespace path
may not be another namespace's `objects`, `tmp`, `labels` or `labels.db`.

## Go client

```go
import "github.com/m-szalik/cas-storage/client"

c := client.New("http://cas-storage:8080", client.WithAPIKey(key))
hash, err := c.PutObject(ctx, data)       // or PutStream(ctx, r)
body, err := c.GetObject(ctx, hash)       // or OpenObject(ctx, hash) to stream
ok, err := c.HeadObject(ctx, hash)
size, deleted, err := c.DeleteObject(ctx, hash, time.Now().Add(-24*time.Hour))

err = c.SetLabel(ctx, hash, "kind", "pdf")
labels, err := c.Labels(ctx, hash)        // map[kind:pdf]
err = c.DeleteLabel(ctx, hash, "kind")
hashes, err := c.FindByLabels(ctx, map[string]string{"kind": "pdf", "source": ""}) // "" = any value
```

## CLI

`cascli` (a binary from the GitHub Releases, `go install
github.com/m-szalik/cas-storage/cmd/cascli@latest`, or `task build` →
`bin/cascli`) wraps the Go client. The server and the key come
from the environment only, so the key stays out of the shell history:

```sh
export CAS_URL=http://cas-storage:8080   # default http://localhost:8080
export CAS_API_KEY=a-long-random-secret  # required

hash=$(cascli put report.pdf -l kind=pdf -l year=2026)   # or: cascli put - < file
cascli get "$hash" -o report.pdf          # stdout without -o
cascli exists "$hash"                     # exit 0 present, 1 absent
cascli labels "$hash"                     # key=value lines
cascli label set "$hash" lang=en src=ted
cascli label delete "$hash" src
cascli find kind=pdf year                 # hashes; a bare key matches any value
cascli delete "$hash" --unused-before 24h # or an RFC 3339 time
```

Errors exit with 2 (and `exists` with 1 for an absent object).

## Development

`task` lists the tasks: `test`, `lint`, `build`, `run` (uses
`config.example.yaml`, namespace `dev`), `image`, `deploy NS=<namespace>`
(applies `deploy/kubernetes.yaml`, which carries the config as a Secret
mounted at `/etc/cas-storage`; replace its placeholder keys), `helm:lint`,
`helm:template [VALUES=<file>]`, `helm:install NS=<namespace> [VALUES=<file>]`.

## Helm chart

`deploy/helm/cas-storage` deploys the same single-replica setup (Recreate, one
PVC kept on uninstall, non-root, read-only root file system):

```yaml
# my-values.yaml
image:
  repository: registry.example/cas-storage
  tag: v1.2.3
allowDelete: false
compress: true
config:                       # rendered into a Secret as config.yaml
  max_upload_size: 500MB
  namespaces:
    docs: {}
  keys:
    - key: "a-long-random-secret"
      namespace: docs
      permissions: [READ, CREATE, DELETE, LABEL]
# existingSecret: cas-storage-config   # or: a Secret with config.yaml, keeps keys out of values
persistence:
  size: 50Gi
  storageClass: ""
```

```sh
helm upgrade --install cas-storage oci://ghcr.io/m-szalik/charts/cas-storage --version 1.2.3 \
  -n storage --create-namespace -f my-values.yaml
# or from the working tree: helm upgrade --install cas-storage deploy/helm/cas-storage ...
```

A change to `config` restarts the pod (config checksum annotation); with
`existingSecret`, restart it yourself after editing the Secret. See
`values.yaml` for resources, scheduling and extra environment variables.

## Publishing

`.github/workflows/publish.yml` runs the tests and `helm lint`, then:

1. On a push to `main`: image `ghcr.io/m-szalik/cas-storage` tagged `main` and
   `sha-<short>`.
2. On a tag `v1.2.3`: image tagged `1.2.3`, `1.2` and `latest`, and chart
   `1.2.3` (appVersion `1.2.3`, so it runs that image) pushed to
   `oci://ghcr.io/m-szalik/charts/cas-storage`.

`.github/workflows/release.yml` runs on the same tags: `task dist` builds
`cascli` (linux, darwin, windows) and `cas-storage` (linux), amd64 and arm64,
and attaches the archives and `sha256sums.txt` to the GitHub Release of the tag
(created with generated notes when missing).

Images are built for `linux/amd64` and `linux/arm64` (cross-compiled, no
emulation). Only `GITHUB_TOKEN` is used; new GHCR packages are private until
made public in the package settings.

## Limits

One replica on one volume (no HA), static API keys only (no TLS: terminate it
in front), no encryption at rest, no listing beyond the label search, no
metrics, no scrubbing of stored hashes.

## License

MIT, see [LICENSE](LICENSE).
