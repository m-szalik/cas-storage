// Package cas implements a content-addressed file store on a local volume.
//
// Objects are immutable: the id of a stored file is the hex SHA-256 of its
// bytes, so writing identical content twice stores nothing new. It does bump
// the object's mtime: that is when the object was last put, which Delete
// checks so an object a new writer just put again is never removed.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/m-szalik/goutils"
)

var (
	// ErrNotFound is returned when an object does not exist.
	ErrNotFound = errors.New("not found")

	hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Store is a content-addressed store rooted at a directory (a PV mount).
type Store struct {
	objectsDir string
	tmpDir     string
	// Compress stores new text, XML, HTML and JSON objects gzipped (see
	// compress.go). Objects already gzipped are read either way.
	Compress bool
	// mu serialises the steps that pick or change an object's on-disk form.
	mu sync.Mutex
}

// Open prepares the on-disk layout under root and returns a Store.
func Open(root string) (*Store, error) {
	s := &Store{
		objectsDir: filepath.Join(root, "objects"),
		tmpDir:     filepath.Join(root, "tmp"),
	}
	for _, d := range []string{s.objectsDir, s.tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("cas: mkdir %s: %w", d, err)
		}
	}
	// Purge leftover temp uploads from a crashed Put: the service is a
	// single-replica Recreate deployment, so nothing else is writing tmpDir.
	if entries, err := os.ReadDir(s.tmpDir); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(s.tmpDir, e.Name()))
		}
	}
	return s, nil
}

// objectPath shards by the first two byte-pairs to keep directories small.
func (s *Store) objectPath(hash string) string {
	return filepath.Join(s.objectsDir, hash[0:2], hash[2:4], hash)
}

// ValidHash reports whether h is a well-formed object id.
func ValidHash(h string) bool { return hashRe.MatchString(h) }

// Put streams r to a temp file while hashing it, then atomically moves it to
// its content-addressed path. existed is true when the content was already
// present (the temp file is discarded and no write happens).
func (s *Store) Put(r io.Reader) (hash string, size int64, existed bool, err error) {
	tmp, err := os.CreateTemp(s.tmpDir, "put-*")
	if err != nil {
		return "", 0, false, fmt.Errorf("cas: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return "", 0, false, fmt.Errorf("cas: write: %w", err)
	}
	// Flush to disk before the rename so a crash cannot leave a partial object
	// at the final content-addressed path that the dedup fast-path then trusts.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, false, fmt.Errorf("cas: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, false, fmt.Errorf("cas: close temp: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	dst := s.objectPath(hash)
	src := tmpName
	if s.Compress && !s.exists(dst) {
		gz, err := s.compressFile(tmpName, size)
		if err != nil {
			return "", 0, false, fmt.Errorf("cas: compress: %w", err)
		}
		if gz != "" {
			defer func() { _ = os.Remove(gz) }() // no-op after a successful rename
			src, dst = gz, dst+gzSuffix
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.touch(s.objectPath(hash)) {
		return hash, size, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, false, fmt.Errorf("cas: mkdir shard: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", 0, false, fmt.Errorf("cas: rename: %w", err)
	}
	return hash, size, false, nil
}

// exists reports whether the object at p is stored in either form.
func (s *Store) exists(p string) bool {
	if _, err := os.Stat(p); err == nil {
		return true
	}
	_, err := os.Stat(p + gzSuffix)
	return err == nil
}

// touch bumps the mtime of the object at p, in whichever form it is stored,
// and reports whether it exists.
func (s *Store) touch(p string) bool {
	now := time.Now()
	return os.Chtimes(p, now, now) == nil || os.Chtimes(p+gzSuffix, now, now) == nil
}

// gzInfo reports a compressed object's uncompressed size.
type gzInfo struct {
	os.FileInfo
	size int64
}

func (i gzInfo) Size() int64 { return i.size }

// Open returns the object's content (caller closes it) and its info, for
// streaming. A compressed object reads as its original bytes and size.
func (s *Store) Open(hash string) (io.ReadSeekCloser, os.FileInfo, error) {
	if !ValidHash(hash) {
		return nil, nil, ErrNotFound
	}
	p := s.objectPath(hash)
	f, err := os.Open(p)
	gz := false
	if os.IsNotExist(err) {
		f, err = os.Open(p + gzSuffix)
		gz = true
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !gz {
		return f, fi, nil
	}
	r, err := newGzipReader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return r, gzInfo{FileInfo: fi, size: r.size}, nil
}

// Delete removes the object unless it was put at or after unusedBefore (a
// writer still needs it). It answers the object's size and whether it was
// removed; an absent object is ErrNotFound.
func (s *Store) Delete(hash string, unusedBefore time.Time) (size int64, deleted bool, err error) {
	if !ValidHash(hash) {
		return 0, false, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, fi, err := s.Open(hash)
	if err != nil {
		return 0, false, err
	}
	goutils.CloseQuietly(f)
	if !fi.ModTime().Before(unusedBefore) {
		return fi.Size(), false, nil
	}
	p := s.objectPath(hash)
	if _, gz := fi.(gzInfo); gz {
		p += gzSuffix
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return 0, false, ErrNotFound
		}
		return 0, false, err
	}
	return fi.Size(), true, nil
}
