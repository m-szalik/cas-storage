package cas

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-szalik/goutils"
)

// gzSuffix names a compressed blob on disk: <hash>.gz next to where the plain
// one would be.
const gzSuffix = ".gz"

// FSBackend keeps blobs in a directory, sharded by the first two byte-pairs
// of the hash (<dir>/ab/cd/<hash>[.gz]) to keep directories small. Import
// renames, so the imported files must be on the same file system.
type FSBackend struct {
	dir string
}

// NewFSBackend returns a Backend on dir, creating it when missing.
func NewFSBackend(dir string) (*FSBackend, error) {
	if err := mkdirAll(dir); err != nil {
		return nil, fmt.Errorf("cas: mkdir %s: %w", dir, err)
	}
	return &FSBackend{dir: dir}, nil
}

func (f *FSBackend) path(b Blob) string {
	p := filepath.Join(f.dir, b.Hash[0:2], b.Hash[2:4], b.Hash)
	if b.Gzip {
		p += gzSuffix
	}
	return p
}

// notFound maps a missing file to ErrNotFound.
func notFound(err error) error {
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}

func (f *FSBackend) Import(path string, b Blob, mtime time.Time) error {
	dst := f.path(b)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		return err
	}
	if err := mkdirAll(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("cas: mkdir shard: %w", err)
	}
	if err := os.Rename(path, dst); err != nil {
		return fmt.Errorf("cas: rename: %w", err)
	}
	// The new name is durable only once its directory is synced: without it
	// a power loss could lose an object already answered as created.
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("cas: sync shard: %w", err)
	}
	return nil
}

func (f *FSBackend) Open(b Blob) (io.ReadSeekCloser, BlobInfo, error) {
	file, err := os.Open(f.path(b))
	if err != nil {
		return nil, BlobInfo{}, notFound(err)
	}
	fi, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, BlobInfo{}, err
	}
	return file, BlobInfo{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (f *FSBackend) Stat(b Blob) (BlobInfo, error) {
	fi, err := os.Stat(f.path(b))
	if err != nil {
		return BlobInfo{}, notFound(err)
	}
	return BlobInfo{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (f *FSBackend) Touch(b Blob, mtime time.Time) error {
	return notFound(os.Chtimes(f.path(b), mtime, mtime))
}

func (f *FSBackend) Remove(b Blob) error {
	return notFound(os.Remove(f.path(b)))
}

func (f *FSBackend) Walk(ctx context.Context, fn func(b Blob, info BlobInfo) error) error {
	return filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		hash, gz := strings.CutSuffix(d.Name(), gzSuffix)
		if !ValidHash(hash) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil // removed meanwhile
		}
		return fn(Blob{Hash: hash, Gzip: gz}, BlobInfo{Size: fi.Size(), ModTime: fi.ModTime()})
	})
}

// mkdirAll is os.MkdirAll that also syncs the parent of every directory it
// creates, so the new entries survive a crash.
func mkdirAll(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s: not a directory", dir)
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return syncDir(parent)
}

// syncDir flushes the directory dir, making its entries (renames, new files
// and subdirectories) durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer goutils.CloseQuietly(f)
	return f.Sync()
}
