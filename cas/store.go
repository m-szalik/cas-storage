// Package cas implements a content-addressed file store over a pluggable
// storage Backend (the local file system by default).
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

// Store is a content-addressed store. Its blobs live in a Backend; uploads
// are staged and labels kept under a local root directory (a PV mount).
type Store struct {
	blobs  Backend
	tmpDir string
	// Compress stores new text, XML, HTML and JSON objects gzipped (see
	// compress.go). Objects already gzipped are read either way.
	Compress bool
	// MaxUploadSize is the largest object the HTTP API accepts, 0 for no
	// limit (Put itself does not check it).
	MaxUploadSize int64
	// locks serialise, per hash, the steps that pick or change an object's
	// stored form or its labels (lock striping: different objects rarely
	// share a lock, so they do not wait for each other).
	locks [256]sync.Mutex
	// db holds the labels (see labels.go).
	db *bolt.DB
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	// Size is the object's (uncompressed) length.
	Size int64
	// ModTime is when the object was last put.
	ModTime time.Time
	// Gzip reports that the object is stored compressed.
	Gzip bool
}

// Open returns a Store keeping its blobs on the file system in
// <root>/objects. Close it to release its label database.
func Open(root string) (*Store, error) {
	blobs, err := NewFSBackend(filepath.Join(root, "objects"))
	if err != nil {
		return nil, err
	}
	return New(root, blobs)
}

// New returns a Store keeping its blobs in blobs, and its staged uploads
// (<root>/tmp) and labels (<root>/labels.db) under root. Close it to release
// its label database.
func New(root string, blobs Backend) (*Store, error) {
	s := &Store{blobs: blobs, tmpDir: filepath.Join(root, "tmp")}
	if err := mkdirAll(s.tmpDir); err != nil {
		return nil, fmt.Errorf("cas: mkdir %s: %w", s.tmpDir, err)
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

// ValidHash reports whether h is a well-formed object id.
func ValidHash(h string) bool { return hashRe.MatchString(h) }

// Put streams r to a temp file while hashing it, then imports it into the
// backend under its hash. existed is true when the content was already
// present (the temp file is discarded and no write happens).
func (s *Store) Put(r io.Reader) (hash string, size int64, existed bool, err error) {
	tmp, err := os.CreateTemp(s.tmpDir, "put-*")
	if err != nil {
		return "", 0, false, fmt.Errorf("cas: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful import

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return "", 0, false, fmt.Errorf("cas: write: %w", err)
	}
	// Flush to disk before the import so a crash cannot leave a partial
	// object under its hash that the dedup fast-path then trusts.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, false, fmt.Errorf("cas: sync temp: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	src, b := tmpName, Blob{Hash: hash}
	if s.Compress && !s.exists(hash) {
		gz, err := s.compressFile(tmp, size)
		if err != nil {
			_ = tmp.Close()
			return "", 0, false, fmt.Errorf("cas: compress: %w", err)
		}
		if gz != "" {
			defer func() { _ = os.Remove(gz) }() // no-op after a successful import
			src, b.Gzip = gz, true
		}
	}
	if err := tmp.Close(); err != nil {
		return "", 0, false, fmt.Errorf("cas: close temp: %w", err)
	}

	defer s.lock(hash)()
	if s.touch(hash) {
		return hash, size, true, nil
	}
	if err := s.blobs.Import(src, b, time.Now()); err != nil {
		return "", 0, false, err
	}
	return hash, size, false, nil
}

// exists reports whether the object is stored in either form.
func (s *Store) exists(hash string) bool {
	if _, err := s.blobs.Stat(Blob{Hash: hash}); err == nil {
		return true
	}
	_, err := s.blobs.Stat(Blob{Hash: hash, Gzip: true})
	return err == nil
}

// touch bumps the mtime of the object, in whichever form it is stored, and
// reports whether it exists.
func (s *Store) touch(hash string) bool {
	now := time.Now()
	return s.blobs.Touch(Blob{Hash: hash}, now) == nil || s.blobs.Touch(Blob{Hash: hash, Gzip: true}, now) == nil
}

// Open returns the object's content (caller closes it) and its info, for
// streaming. A compressed object reads as its original bytes and size.
func (s *Store) Open(hash string) (io.ReadSeekCloser, ObjectInfo, error) {
	if !ValidHash(hash) {
		return nil, ObjectInfo{}, ErrNotFound
	}
	r, bi, err := s.blobs.Open(Blob{Hash: hash})
	gz := false
	if errors.Is(err, ErrNotFound) {
		r, bi, err = s.blobs.Open(Blob{Hash: hash, Gzip: true})
		gz = true
		if errors.Is(err, ErrNotFound) {
			// Deleted and put again (plain) between the two opens.
			r, bi, err = s.blobs.Open(Blob{Hash: hash})
			gz = false
		}
	}
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	info := ObjectInfo{Size: bi.Size, ModTime: bi.ModTime, Gzip: gz}
	if !gz {
		return r, info, nil
	}
	zr, err := newGzipReader(r, bi.Size)
	if err != nil {
		_ = r.Close()
		return nil, ObjectInfo{}, err
	}
	info.Size = zr.size
	return zr, info, nil
}

// Delete removes the object unless it was put at or after unusedBefore (a
// writer still needs it), together with its labels. It answers the object's
// size and whether it was removed; an absent object is ErrNotFound.
func (s *Store) Delete(hash string, unusedBefore time.Time) (size int64, deleted bool, err error) {
	if !ValidHash(hash) {
		return 0, false, ErrNotFound
	}
	defer s.lock(hash)()
	f, info, err := s.Open(hash)
	if err != nil {
		return 0, false, err
	}
	goutils.CloseQuietly(f)
	if !info.ModTime.Before(unusedBefore) {
		return info.Size, false, nil
	}
	// Labels go first: a crash in between leaves an unlabelled object, never
	// labels of an absent one.
	if err := s.dropLabels(hash); err != nil {
		return 0, false, err
	}
	if err := s.blobs.Remove(Blob{Hash: hash, Gzip: info.Gzip}); err != nil {
		return 0, false, err
	}
	// Both forms exist only after a crash in the middle of CompressExisting.
	_ = s.blobs.Remove(Blob{Hash: hash, Gzip: !info.Gzip})
	return info.Size, true, nil
}
