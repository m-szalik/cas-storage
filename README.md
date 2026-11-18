# cas-storage

A small, immutable, content-addressed file store over HTTP. The id of an
object is the SHA-256 of its content, so storing the same bytes twice keeps
one copy. Objects live in a local directory (e.g. a Kubernetes volume); the
metadata about what an object means belongs to the caller.

## API

| Method | Path | Result |
|---|---|---|
| `POST` | `/objects` | stores the request body; `201` new / `200` already present; JSON `{"hash","size","existed"}`, `Location: /objects/<hash>` |
| `GET` | `/objects/{hash}` | streams the object (Range and conditional requests supported); `404` if absent |
| `HEAD` | `/objects/{hash}` | existence and size |
| `DELETE` | `/objects/{hash}?unused_before=<RFC 3339>` | only with `STORAGE_ALLOW_DELETE=true`: removes the object unless it was put at or after that time; `200 {"size","deleted"}`, `404` if absent |
| `GET` | `/healthz` | liveness |

There is no authentication: run it inside a trusted network.

```sh
curl -s --data-binary @report.pdf http://localhost:8080/objects
curl -s -o report.pdf http://localhost:8080/objects/<hash>
```

## How it stores

1. The body is streamed to `<root>/tmp` while its SHA-256 is computed, then synced.
2. It is renamed atomically to `<root>/objects/ab/cd/<hash>` (two levels of 2-hex shards).
3. A put of an object that already exists only refreshes its mtime. `DELETE`
   compares `unused_before` with that mtime, so a caller that re-puts an
   object protects it from a concurrent prune.
4. Leftovers in `tmp/` are removed at start.

### Compression

With `STORAGE_COMPRESS=true`, text objects (plain text, HTML, XML, JSON;
1 KiB–4 GiB) are kept gzipped as `<hash>.gz` when that saves at least 10%.
The hash is still that of the uncompressed bytes and reads return them
unchanged. Objects already on disk are compressed in the background at start.
A Range read of a compressed object decompresses from its beginning.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `STORAGE_DIR` | `/data` | root directory |
| `LISTEN_ADDR` | `:8080` | HTTP address |
| `STORAGE_ALLOW_DELETE` | `false` | enable `DELETE /objects/{hash}` |
| `STORAGE_COMPRESS` | `false` | gzip text objects |

## Go client

```go
import "github.com/m-szalik/cas-storage/client"

c := client.New("http://cas-storage:8080")
hash, err := c.PutObject(ctx, data)       // or PutStream(ctx, r)
body, err := c.GetObject(ctx, hash)       // or OpenObject(ctx, hash) to stream
ok, err := c.HeadObject(ctx, hash)
size, deleted, err := c.DeleteObject(ctx, hash, time.Now().Add(-24*time.Hour))
```

## Development

`task` lists the tasks: `test`, `lint`, `build`, `run`, `image`,
`deploy NS=<namespace>` (applies `deploy/kubernetes.yaml`).

## Limits

One replica on one volume (no HA), no auth, no encryption at rest, no
listing, no metrics, no scrubbing of stored hashes.

## License

MIT, see [LICENSE](LICENSE).
