package cas

import (
	"context"
	"io"
	"time"
)

// Blob names one stored form of an object: its plain bytes or, with Gzip,
// their gzip compression.
type Blob struct {
	Hash string
	Gzip bool
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	// Size is the number of stored bytes (compressed for a gzip blob).
	Size int64
	// ModTime is when the blob was last put; Delete's unused_before check
	// relies on it.
	ModTime time.Time
}

// Backend is where a Store keeps its blobs: the storage provider (the file
// system today, S3 and others later). It only stores and serves bytes under
// a Blob name; hashing, deduplication, compression, labels and locking stay
// in the Store, which also serialises the calls for one hash.
//
// Every method answers ErrNotFound for a blob that does not exist, and must
// be safe for concurrent use.
type Backend interface {
	// Import moves the local file at path in as b, with modification time
	// mtime, atomically and durably: once it returns, b is readable and
	// survives a crash. On success path is gone. An existing b is replaced
	// (the Store imports over one only to repair a crash leftover).
	Import(path string, b Blob, mtime time.Time) error
	// Open returns b's content (the caller closes it) and its info.
	Open(b Blob) (io.ReadSeekCloser, BlobInfo, error)
	// Stat answers b's info.
	Stat(b Blob) (BlobInfo, error)
	// Touch sets b's modification time.
	Touch(b Blob, mtime time.Time) error
	// Remove deletes b.
	Remove(b Blob) error
	// Walk calls fn for every stored blob until fn or ctx fails. Blobs added
	// or removed meanwhile may or may not be seen.
	Walk(ctx context.Context, fn func(b Blob, info BlobInfo) error) error
}
