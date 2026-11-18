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
	"strconv"
	"sync"
	"time"

	"github.com/m-szalik/goutils"
	bolt "go.etcd.io/bbolt"
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
	// MaxUploadSize is the largest object the HTTP API accepts, 0 for no
	// limit (Put itself does not check it).
	MaxUploadSize int64
	// locks serialise, per hash, the steps that pick or change an object's
	// on-disk form or its labels (lock striping: different objects rarely
	// share a lock, so they do not wait for each other).
	locks [256]sync.Mutex
	// db holds the labels (see labels.go).
	db *bolt.DB
}

// Open prepares the on-disk layout under root and returns a Store. Close it
// to release its label database.
func Open(root string) (*Store, error) {
	s := &Store{
		objectsDir: filepath.Join(root, "objects"),
		tmpDir:     filepath.Join(root, "tmp"),
	}
	for _, d := range []string{s.objectsDir, s.tmpDir} {
		if err := mkdirAll(d); err != nil {
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
	if err := s.openLabels(root); err != nil {
		return nil, err
	}
	return s, nil
}

// lock takes the lock of hash's stripe and answers its unlock.
func (s *Store) lock(hash string) func() {
	n, _ := strconv.ParseUint(hash[:2], 16, 8)
	m := &s.locks[n]
	m.Lock()
	return m.Unlock
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

	defer s.lock(hash)()
	if s.touch(s.objectPath(hash)) {
		return hash, size, true, nil
	}
	if err := mkdirAll(filepath.Dir(dst)); err != nil {
		return "", 0, false, fmt.Errorf("cas: mkdir shard: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", 0, false, fmt.Errorf("cas: rename: %w", err)
	}
	// The new name is durable only once its directory is synced: without
	// it a power loss could lose an object already answered as created.
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return "", 0, false, fmt.Errorf("cas: sync shard: %w", err)
	}
	return hash, size, false, nil
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
		if os.IsNotExist(err) {
			// Deleted and put again (plain) between the two opens.
			f, err = os.Open(p)
			gz = false
		}
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
// writer still needs it), together with its labels. It answers the object's
// size and whether it was removed; an absent object is ErrNotFound.
func (s *Store) Delete(hash string, unusedBefore time.Time) (size int64, deleted bool, err error) {
	if !ValidHash(hash) {
		return 0, false, ErrNotFound
	}
	defer s.lock(hash)()
	f, fi, err := s.Open(hash)
	if err != nil {
		return 0, false, err
	}
	goutils.CloseQuietly(f)
	if !fi.ModTime().Before(unusedBefore) {
		return fi.Size(), false, nil
	}
	// Labels go first: a crash in between leaves an unlabelled object, never
	// labels of an absent one.
	if err := s.dropLabels(hash); err != nil {
		return 0, false, err
	}
	p, other := s.objectPath(hash), s.objectPath(hash)+gzSuffix
	if _, gz := fi.(gzInfo); gz {
		p, other = other, p
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return 0, false, ErrNotFound
		}
		return 0, false, err
	}
	// Both forms exist only after a crash in the middle of CompressExisting.
	_ = os.Remove(other)
	return fi.Size(), true, nil
}
