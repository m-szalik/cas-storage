package cas

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/m-szalik/goutils"
)

var (
	xmlDoc = []byte(`<?xml version="1.0"?><notice>` + strings.Repeat("<lot>Provision of training services</lot>", 200) + `</notice>`)
	pdfDoc = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x00, 0x9c, 0x37, 0xe1}, 2000)...)
)

func onDisk(t *testing.T, s *Store, hash string) (plain, gz bool) {
	t.Helper()
	_, err := os.Stat(s.objectPath(hash))
	_, errGz := os.Stat(s.objectPath(hash) + gzSuffix)
	return err == nil, errGz == nil
}

func readBack(t *testing.T, s *Store, hash string) ([]byte, int64) {
	t.Helper()
	f, fi, err := s.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer goutils.CloseQuietly(f)
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return got, fi.Size()
}

func TestPutCompressesTextOnly(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Compress = true

	hash, size, _, err := s.Put(bytes.NewReader(xmlDoc))
	if err != nil || size != int64(len(xmlDoc)) {
		t.Fatalf("put = %d, %v", size, err)
	}
	if plain, gz := onDisk(t, s, hash); plain || !gz {
		t.Fatalf("xml stored plain=%v gz=%v", plain, gz)
	}
	if got, n := readBack(t, s, hash); !bytes.Equal(got, xmlDoc) || n != int64(len(xmlDoc)) {
		t.Fatalf("read back %d bytes, size %d", len(got), n)
	}
	if _, _, existed, _ := s.Put(bytes.NewReader(xmlDoc)); !existed {
		t.Fatal("re-put of a compressed object not deduplicated")
	}

	pdf, _, _, _ := s.Put(bytes.NewReader(pdfDoc))
	if plain, gz := onDisk(t, s, pdf); !plain || gz {
		t.Fatalf("pdf stored plain=%v gz=%v", plain, gz)
	}
}

// Clients get the same bytes, length, type and ranges from a compressed
// object as from a plain one.
func TestCompressedObjectServesLikePlain(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	plain, _ := Open(t.TempDir())
	packed, _ := Open(t.TempDir())
	packed.Compress = true
	hash, _, _, _ := plain.Put(bytes.NewReader(xmlDoc))
	_, _, _, _ = packed.Put(bytes.NewReader(xmlDoc))

	get := func(s *Store, method, rng string) (*http.Response, []byte) {
		srv := httptest.NewServer(Handler(s, log, false))
		defer srv.Close()
		req, _ := http.NewRequest(method, srv.URL+"/objects/"+hash, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer goutils.CloseQuietly(resp.Body)
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	for _, c := range []struct{ method, rng string }{
		{http.MethodGet, ""}, {http.MethodHead, ""},
		{http.MethodGet, "bytes=100-199"}, {http.MethodGet, "bytes=-50"},
	} {
		want, wantBody := get(plain, c.method, c.rng)
		got, gotBody := get(packed, c.method, c.rng)
		if got.StatusCode != want.StatusCode || !bytes.Equal(gotBody, wantBody) ||
			got.ContentLength != want.ContentLength ||
			got.Header.Get("Content-Type") != want.Header.Get("Content-Type") {
			t.Fatalf("%s %q: got %d len=%d type=%q, want %d len=%d type=%q", c.method, c.rng,
				got.StatusCode, got.ContentLength, got.Header.Get("Content-Type"),
				want.StatusCode, want.ContentLength, want.Header.Get("Content-Type"))
		}
	}
}

func TestCompressedObjectSeeksBack(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Compress = true
	hash, _, _, _ := s.Put(bytes.NewReader(xmlDoc))
	f, _, err := s.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer goutils.CloseQuietly(f)
	buf := make([]byte, 10)
	for _, off := range []int64{5000, 100, 0, int64(len(xmlDoc)) - 10} {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(f, buf); err != nil || !bytes.Equal(buf, xmlDoc[off:off+10]) {
			t.Fatalf("at %d read %q, %v", off, buf, err)
		}
	}
}

func TestCompressExisting(t *testing.T) {
	s, _ := Open(t.TempDir())
	xml, _, _, _ := s.Put(bytes.NewReader(xmlDoc))
	pdf, _, _, _ := s.Put(bytes.NewReader(pdfDoc))
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	_ = os.Chtimes(s.objectPath(xml), old, old)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := s.CompressExisting(context.Background(), log); err != nil {
		t.Fatal(err)
	}
	if plain, gz := onDisk(t, s, xml); plain || !gz {
		t.Fatalf("xml after compress plain=%v gz=%v", plain, gz)
	}
	if plain, gz := onDisk(t, s, pdf); !plain || gz {
		t.Fatalf("pdf after compress plain=%v gz=%v", plain, gz)
	}
	if got, _ := readBack(t, s, xml); !bytes.Equal(got, xmlDoc) {
		t.Fatal("compressed object reads back different bytes")
	}
	// The last-put time survives, so Delete still decides by it.
	if size, deleted, err := s.Delete(xml, old); err != nil || deleted || size != int64(len(xmlDoc)) {
		t.Fatalf("delete before last put = %d, %v, %v", size, deleted, err)
	}
	if _, deleted, err := s.Delete(xml, time.Now()); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if plain, gz := onDisk(t, s, xml); plain || gz {
		t.Fatal("deleted object still on disk")
	}
	// A second pass finds nothing to do.
	if err := s.CompressExisting(context.Background(), log); err != nil {
		t.Fatal(err)
	}
}
