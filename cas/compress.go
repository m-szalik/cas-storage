package cas

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/m-szalik/goutils"
)

// A compressed object is stored as its gzip blob (Blob.Gzip) in place of the
// plain one. An object exists in exactly one of the two forms; the id is
// always the hash of the uncompressed bytes, so clients never see the
// difference.

const (
	// minCompressSize skips objects too small for gzip to pay off.
	minCompressSize = 1 << 10
	// maxCompressSize keeps the gzip trailer's 32-bit length exact.
	maxCompressSize = 1<<32 - 1
)

// compressible reports whether content starting with head is text, XML, HTML
// or JSON: the formats worth compressing (PDFs, archives and images are
// already compressed).
func compressible(head []byte) bool {
	return strings.HasPrefix(http.DetectContentType(head), "text/")
}

// compressFile gzips the size bytes of in into a new temp file and answers
// its name when the result is smaller than the original; "" means keep the
// content as it is.
func (s *Store) compressFile(in io.ReadSeeker, size int64) (string, error) {
	if size < minCompressSize || size > maxCompressSize {
		return "", nil
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(in, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if !compressible(head[:n]) {
		return "", nil
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(s.tmpDir, "gz-*")
	if err != nil {
		return "", err
	}
	name := out.Name()
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(name)
		}
	}()
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	fi, err := out.Stat()
	if err != nil {
		return "", err
	}
	if fi.Size() >= size*9/10 {
		return "", nil
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// CompressExisting gzips the plain objects already stored that Put would
// compress today. It is idempotent and safe to stop at any point: the gzip
// blob is imported before the plain one is removed, and readers fall back to
// the other form.
func (s *Store) CompressExisting(ctx context.Context, log *slog.Logger) error {
	start := time.Now()
	var scanned, compressed int
	var before, after int64
	err := s.blobs.Walk(ctx, func(b Blob, info BlobInfo) error {
		if b.Gzip {
			return nil
		}
		scanned++
		tmp, err := s.compressBlob(b, info.Size)
		if err != nil {
			log.Warn("compress object", "hash", b.Hash, "err", err)
			return nil
		}
		if tmp == "" {
			return nil
		}
		defer func() { _ = os.Remove(tmp) }() // no-op after a successful import
		gz, err := s.swapToCompressed(b.Hash, tmp, info.ModTime)
		if err != nil {
			log.Warn("compress object", "hash", b.Hash, "err", err)
			return nil
		}
		if gz > 0 {
			compressed++
			before += info.Size
			after += gz
		}
		if scanned%50000 == 0 {
			log.Info("compressing existing objects", "scanned", scanned, "compressed", compressed, "saved_bytes", before-after)
		}
		return nil
	})
	log.Info("existing objects compressed", "scanned", scanned, "compressed", compressed,
		"bytes_before", before, "bytes_after", after, "saved_bytes", before-after, "dur", time.Since(start).String())
	return err
}

// compressBlob is compressFile on the plain blob b; "" also when b is gone.
func (s *Store) compressBlob(b Blob, size int64) (string, error) {
	if size < minCompressSize || size > maxCompressSize {
		return "", nil
	}
	r, _, err := s.blobs.Open(b)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer goutils.CloseQuietly(r)
	return s.compressFile(r, size)
}

// swapToCompressed replaces the plain blob of hash with the gzip file tmp,
// keeping its mtime (Delete's last-put check). It answers the compressed
// size, or 0 when the plain blob is no longer there to replace.
func (s *Store) swapToCompressed(hash, tmp string, mtime time.Time) (int64, error) {
	defer s.lock(hash)()
	plain, gz := Blob{Hash: hash}, Blob{Hash: hash, Gzip: true}
	info, err := s.blobs.Stat(plain)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// A Put since the scan bumped the mtime: keep the newer one.
	if info.ModTime.After(mtime) {
		mtime = info.ModTime
	}
	// The gzip blob is durable before the plain one goes, so Open always
	// finds one.
	if err := s.blobs.Import(tmp, gz, mtime); err != nil {
		return 0, err
	}
	if err := s.blobs.Remove(plain); err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	gzInfo, err := s.blobs.Stat(gz)
	if err != nil {
		return 0, err
	}
	return gzInfo.Size, nil
}

// gzipSize reads the uncompressed length from the gzip trailer (ISIZE) of
// the size bytes of r.
func gzipSize(r io.ReadSeeker, size int64) (int64, error) {
	var b [4]byte
	if size < 18 {
		return 0, errors.New("cas: gzip object too short")
	}
	if _, err := r.Seek(size-4, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(b[:])), nil
}

// gzipReader is an io.ReadSeekCloser over the uncompressed bytes of a gzip
// blob. Seeking only moves the offset; a read decompresses forward to it, or
// restarts from the beginning to go back (http.ServeContent seeks back once,
// after sniffing the first 512 bytes, which is cheap).
type gzipReader struct {
	f    io.ReadSeekCloser
	size int64
	off  int64 // requested offset
	pos  int64 // offset zr is at
	zr   *gzip.Reader
}

// newGzipReader reads the gzip blob f of stored length size.
func newGzipReader(f io.ReadSeekCloser, size int64) (*gzipReader, error) {
	n, err := gzipSize(f, size)
	if err != nil {
		return nil, err
	}
	return &gzipReader{f: f, size: n}, nil
}

func (g *gzipReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += g.off
	case io.SeekEnd:
		offset += g.size
	default:
		return 0, errors.New("cas: bad whence")
	}
	if offset < 0 {
		return 0, errors.New("cas: negative offset")
	}
	g.off = offset
	return offset, nil
}

func (g *gzipReader) Read(p []byte) (int, error) {
	if g.zr == nil || g.off < g.pos {
		if _, err := g.f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		if g.zr == nil {
			zr, err := gzip.NewReader(g.f)
			if err != nil {
				return 0, err
			}
			g.zr = zr
		} else if err := g.zr.Reset(g.f); err != nil {
			return 0, err
		}
		g.pos = 0
	}
	if g.off > g.pos {
		n, err := io.CopyN(io.Discard, g.zr, g.off-g.pos)
		g.pos += n
		if err != nil {
			return 0, err
		}
	}
	n, err := g.zr.Read(p)
	g.pos += int64(n)
	g.off = g.pos
	return n, err
}

func (g *gzipReader) Close() error { return g.f.Close() }
