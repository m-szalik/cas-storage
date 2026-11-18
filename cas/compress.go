package cas

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-szalik/goutils"
)

// Compressed objects live next to where the plain one would, as <hash>.gz.
// An object exists in exactly one of the two forms; the id is always the hash
// of the uncompressed bytes, so clients never see the difference.
const gzSuffix = ".gz"

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

// compressFile gzips src into a new temp file and answers its name when the
// result is smaller than the original; "" means keep src as it is.
func (s *Store) compressFile(src string, size int64) (string, error) {
	if size < minCompressSize || size > maxCompressSize {
		return "", nil
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer goutils.CloseQuietly(in)
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

// CompressExisting gzips the plain objects already on disk that Put would
// compress today. It is idempotent and safe to stop at any point: each object
// switches form with one rename, and readers fall back to the other form.
func (s *Store) CompressExisting(ctx context.Context, log *slog.Logger) error {
	start := time.Now()
	var scanned, compressed int
	var before, after int64
	err := filepath.WalkDir(s.objectsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !ValidHash(d.Name()) {
			return nil
		}
		scanned++
		fi, err := d.Info()
		if err != nil {
			return nil // removed meanwhile
		}
		tmp, err := s.compressFile(p, fi.Size())
		if err != nil {
			log.Warn("compress object", "hash", d.Name(), "err", err)
			return nil
		}
		if tmp == "" {
			return nil
		}
		defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename
		gz, err := s.swapToCompressed(d.Name(), p, tmp, fi.ModTime())
		if err != nil {
			log.Warn("compress object", "hash", d.Name(), "err", err)
			return nil
		}
		if gz > 0 {
			compressed++
			before += fi.Size()
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

// swapToCompressed replaces the plain object p with the gzip file tmp, keeping
// its mtime (Delete's last-put check). It answers the compressed size, or 0
// when p is no longer there to replace.
func (s *Store) swapToCompressed(hash, p, tmp string, mtime time.Time) (int64, error) {
	defer s.lock(hash)()
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// A Put since the scan bumped the mtime: keep the newer one.
	if fi.ModTime().After(mtime) {
		mtime = fi.ModTime()
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		return 0, err
	}
	// The .gz appears before the plain file goes, so Open always finds one.
	if err := os.Rename(tmp, p+gzSuffix); err != nil {
		return 0, err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	gz, err := os.Stat(p + gzSuffix)
	if err != nil {
		return 0, err
	}
	return gz.Size(), nil
}

// gzipSize reads the uncompressed length from the gzip trailer (ISIZE).
func gzipSize(f *os.File) (int64, error) {
	var b [4]byte
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if fi.Size() < 18 {
		return 0, fmt.Errorf("cas: gzip object too short: %s", f.Name())
	}
	if _, err := f.ReadAt(b[:], fi.Size()-4); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(b[:])), nil
}

// gzipReader is an io.ReadSeekCloser over the uncompressed bytes of a gzip
// file. Seeking only moves the offset; a read decompresses forward to it, or
// restarts from the beginning to go back (http.ServeContent seeks back once,
// after sniffing the first 512 bytes, which is cheap).
type gzipReader struct {
	f    *os.File
	size int64
	off  int64 // requested offset
	pos  int64 // offset zr is at
	zr   *gzip.Reader
}

func newGzipReader(f *os.File) (*gzipReader, error) {
	size, err := gzipSize(f)
	if err != nil {
		return nil, err
	}
	return &gzipReader{f: f, size: size}, nil
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
