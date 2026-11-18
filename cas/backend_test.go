package cas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testBackend checks the Backend contract; run it for every provider.
func testBackend(t *testing.T, b Backend) {
	t.Helper()
	ctx := context.Background()
	hash := strings.Repeat("ab", 32)
	plain, gz := Blob{Hash: hash}, Blob{Hash: hash, Gzip: true}
	stage := func(content string) string {
		p := filepath.Join(t.TempDir(), "staged")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := b.Stat(plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat(absent) = %v", err)
	}
	if _, _, err := b.Open(plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(absent) = %v", err)
	}
	if err := b.Touch(plain, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Touch(absent) = %v", err)
	}
	if err := b.Remove(plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove(absent) = %v", err)
	}

	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	src := stage("hello backend")
	if err := b.Import(src, plain, mtime); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("staged file kept: %v", err)
	}
	r, info, err := b.Open(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if _, err := r.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "hello backend" || string(tail) != "backend" {
		t.Fatalf("content = %q, after seek %q", got, tail)
	}
	if info.Size != 13 || !info.ModTime.Equal(mtime) {
		t.Fatalf("Open info = %+v, want size 13, mtime %v", info, mtime)
	}
	if _, err := b.Stat(gz); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the other form exists: %v", err)
	}

	later := mtime.Add(time.Minute)
	if err := b.Touch(plain, later); err != nil {
		t.Fatal(err)
	}
	if info, err := b.Stat(plain); err != nil || !info.ModTime.Equal(later) {
		t.Fatalf("Stat after Touch = %+v, %v", info, err)
	}

	if err := b.Import(stage("zipped"), gz, mtime); err != nil {
		t.Fatal(err)
	}
	if err := b.Import(stage("zipped again"), gz, mtime); err != nil {
		t.Fatalf("Import over an existing blob: %v", err)
	}
	var seen []Blob
	if err := b.Walk(ctx, func(bl Blob, _ BlobInfo) error {
		seen = append(seen, bl)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(seen, func(i, j int) bool { return !seen[i].Gzip })
	if !reflect.DeepEqual(seen, []Blob{plain, gz}) {
		t.Fatalf("Walk = %v", seen)
	}

	for _, bl := range []Blob{plain, gz} {
		if err := b.Remove(bl); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Stat(bl); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat after Remove = %v", err)
		}
	}
}

func TestFSBackend(t *testing.T) {
	b, err := NewFSBackend(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	testBackend(t, b)
}

func TestMemBackend(t *testing.T) { testBackend(t, newMemBackend()) }

// memBackend keeps blobs in memory: a second provider, so the Store is
// tested apart from the file system.
type memBackend struct {
	mu    sync.Mutex
	blobs map[Blob]memBlob
}

type memBlob struct {
	data  []byte
	mtime time.Time
}

func newMemBackend() *memBackend { return &memBackend{blobs: map[Blob]memBlob{}} }

func (m *memBackend) Import(path string, b Blob, mtime time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.blobs[b] = memBlob{data: data, mtime: mtime}
	m.mu.Unlock()
	return os.Remove(path)
}

func (m *memBackend) Open(b Blob) (io.ReadSeekCloser, BlobInfo, error) {
	info, err := m.Stat(b)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return nopCloser{bytes.NewReader(m.blobs[b].data)}, info, nil
}

type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

func (m *memBackend) Stat(b Blob) (BlobInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bl, ok := m.blobs[b]
	if !ok {
		return BlobInfo{}, ErrNotFound
	}
	return BlobInfo{Size: int64(len(bl.data)), ModTime: bl.mtime}, nil
}

func (m *memBackend) Touch(b Blob, mtime time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bl, ok := m.blobs[b]
	if !ok {
		return ErrNotFound
	}
	bl.mtime = mtime
	m.blobs[b] = bl
	return nil
}

func (m *memBackend) Remove(b Blob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[b]; !ok {
		return ErrNotFound
	}
	delete(m.blobs, b)
	return nil
}

func (m *memBackend) Walk(ctx context.Context, fn func(Blob, BlobInfo) error) error {
	m.mu.Lock()
	var all []Blob
	for b := range m.blobs {
		all = append(all, b)
	}
	m.mu.Unlock()
	for _, b := range all {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := m.Stat(b)
		if err != nil {
			continue // removed meanwhile
		}
		if err := fn(b, info); err != nil {
			return err
		}
	}
	return nil
}

func TestStoreOnAnotherBackend(t *testing.T) {
	mem := newMemBackend()
	s, err := New(t.TempDir(), mem)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Compress = true

	hash, _, existed, err := s.Put(bytes.NewReader(xmlDoc))
	if err != nil || existed {
		t.Fatalf("Put = %v, %v", existed, err)
	}
	if _, err := mem.Stat(Blob{Hash: hash, Gzip: true}); err != nil {
		t.Fatalf("text object not stored gzipped: %v", err)
	}
	if _, _, existed, _ := s.Put(bytes.NewReader(xmlDoc)); !existed {
		t.Fatal("second Put not deduplicated")
	}
	r, info, err := s.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, xmlDoc) || info.Size != int64(len(xmlDoc)) || !info.Gzip {
		t.Fatalf("Open = %d bytes, info %+v", len(got), info)
	}

	if err := s.SetLabel(hash, "kind", "xml"); err != nil {
		t.Fatal(err)
	}
	if found, _ := s.FindByLabels(map[string]string{"kind": "xml"}); !reflect.DeepEqual(found, []string{hash}) {
		t.Fatalf("FindByLabels = %v", found)
	}

	// A plain object compressed afterwards keeps its content and mtime.
	s.Compress = false
	plainDoc := bytes.Repeat([]byte("plain text line\n"), 200)
	h2, _, _, _ := s.Put(bytes.NewReader(plainDoc))
	old := time.Now().Add(-time.Hour)
	_ = mem.Touch(Blob{Hash: h2}, old)
	if err := s.CompressExisting(context.Background(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Stat(Blob{Hash: h2}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plain blob kept: %v", err)
	}
	if info, err := mem.Stat(Blob{Hash: h2, Gzip: true}); err != nil || !info.ModTime.Equal(old) {
		t.Fatalf("gzip blob = %+v, %v", info, err)
	}

	if _, deleted, err := s.Delete(hash, time.Now().Add(time.Minute)); err != nil || !deleted {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	if len(mem.blobs) != 1 {
		t.Fatalf("%d blobs left, want 1", len(mem.blobs))
	}
	if _, err := s.Labels(hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Labels of a deleted object = %v", err)
	}
}
