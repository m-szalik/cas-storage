package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-szalik/goutils"
)

func TestPutIsContentAddressedAndIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("provision of Portuguese language training services")
	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])

	hash, size, existed, err := s.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if hash != wantHex {
		t.Fatalf("hash = %s, want %s", hash, wantHex)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
	if existed {
		t.Fatal("first put reported existed=true")
	}

	// Second identical put must dedup.
	_, _, existed2, err := s.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if !existed2 {
		t.Fatal("second put reported existed=false")
	}

	// Read back and compare.
	f, fi, err := s.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer goutils.CloseQuietly(f)
	if fi.Size() != int64(len(content)) {
		t.Fatalf("stat size = %d", fi.Size())
	}
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, content) {
		t.Fatalf("read back mismatch")
	}
}

func TestOpenPurgesLeftoverTempUploads(t *testing.T) {
	root := t.TempDir()
	tmpDir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(tmpDir, "put-123456")
	if err := os.WriteFile(leftover, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("leftover temp file not purged: %v", err)
	}
}

func TestOpenMissingReturnsNotFound(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.Open(strings.Repeat("a", 64)); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Open("not-a-hash"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
