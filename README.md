# cas-storage

A small, immutable, content-addressed file store over HTTP. The id of an
object is the SHA-256 of its content, so storing the same bytes twice keeps
one copy. Objects live in a local directory (e.g. a Kubernetes volume), split
into namespaces reached with API keys; apart from simple `key=value` labels,
the metadata about what an object means belongs to the caller.

## API

Every request except `GET /healthz` needs an API key, sent as
`X-API-Key: <key>` or `Authorization: Bearer <key>`. A key belongs to one
namespace and carries permissions (see [Configuration](#configuration));
a missing or unknown key gets `401` (with `WWW-Authenticate: Bearer`), a key
without the route's permission gets `403`. All other responses are as
before; every route works on the key's namespace only.

| Method | Path | Permission | Result |
|---|---|---|---|
| `POST` | `/objects` | `CREATE` | stores the request body; `201` new / `200` already present; JSON `{"hash","size","existed"}`, `Location: /objects/<hash>` |
| `GET` | `/objects/{hash}` | `READ` | streams the object (Range and conditional requests supported); `404` if absent |
| `HEAD` | `/objects/{hash}` | `READ` | existence and size |
| `DELETE` | `/objects/{hash}?unused_before=<RFC 3339>` | `DELETE` | only with `STORAGE_ALLOW_DELETE=true`: removes the object (and its labels) unless it was put at or after that time; `200 {"size","deleted"}`, `404` if absent |
| `GET` | `/objects/{hash}/labels` | `READ` | `200 {"key":"value",...}` (`{}` when none); `404` if absent |
| `PUT` | `/objects/{hash}/labels/{key}` | `LABEL` | sets the label; the body is the value (raw text); `204`; `404` if absent |
| `DELETE` | `/objects/{hash}/labels/{key}` | `LABEL` | removes the label; `204` (also when it was not set); `404` if absent |
| `GET` | `/objects?label=k=v&label=k2` | `READ` | `200 {"hashes":[...]}`, sorted: objects carrying every given label (`label=k` matches any value); at least one `label` required |
| `GET` | `/healthz` | none | liveness |

Labels: key `^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$` (may contain `/`), value
1–256 bytes of UTF-8, at most 64 labels per object. A bad key, value or
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

1. The body is streamed to `<root>/tmp` while its SHA-256 is computed, then synced.
2. It is renamed atomically to `<root>/objects/ab/cd/<hash>` (two levels of 2-hex shards).
3. A put of an object that already exists only refreshes its mtime. `DELETE`
   compares `unused_before` with that mtime, so a caller that re-puts an
   object protects it from a concurrent prune.
4. Leftovers in `tmp/` are removed at start.
5. Labels live in `<root>/labels/ab/cd/<hash>.json` (written atomically) and
   are indexed in memory at start for the search.

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
namespaces:
  docs:
    path: /data/docs        # optional, default <STORAGE_DIR>/docs
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

Objects stored before namespaces existed sit in `<STORAGE_DIR>/objects`; to
keep serving them, give one namespace `path: <STORAGE_DIR>` (and name no other
namespace `objects`, `tmp` or `labels`).

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

## Development

`task` lists the tasks: `test`, `lint`, `build`, `run` (uses
`config.example.yaml`, namespace `dev`), `image`, `deploy NS=<namespace>`
(applies `deploy/kubernetes.yaml`, which carries the config as a Secret
mounted at `/etc/cas-storage`; replace its placeholder keys).

## Limits

One replica on one volume (no HA), static API keys only (no TLS: terminate it
in front), no encryption at rest, no listing beyond the label search, no
metrics, no scrubbing of stored hashes.

## License

MIT, see [LICENSE](LICENSE).
