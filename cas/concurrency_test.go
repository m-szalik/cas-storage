package cas

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// objectFiles counts the files under the objects tree.
func objectFiles(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(s.objectsDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func readAll(s *Store, hash string) ([]byte, error) {
	f, _, err := s.Open(hash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// variant answers a distinct compressible document per i.
func variant(i int) []byte {
	return append(bytes.Clone(xmlDoc), fmt.Sprint(i)...)
}

func TestConcurrentPutsOfSameContent(t *testing.T) {
	for _, compress := range []bool{false, true} {
		t.Run(fmt.Sprintf("compress=%v", compress), func(t *testing.T) {
			s, _ := Open(t.TempDir())
			s.Compress = compress
			const n = 32
			var created atomic.Int32
			hashes := make([]string, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					h, _, existed, err := s.Put(bytes.NewReader(xmlDoc))
					if err != nil {
						t.Error(err)
						return
					}
					if !existed {
						created.Add(1)
					}
					hashes[i] = h
				}()
			}
			wg.Wait()
			if created.Load() != 1 {
				t.Fatalf("existed=false %d times, want 1", created.Load())
			}
			if got := objectFiles(t, s); got != 1 {
				t.Fatalf("%d files on disk, want 1", got)
			}
			if got, err := readAll(s, hashes[0]); err != nil || !bytes.Equal(got, xmlDoc) {
				t.Fatalf("read back %d bytes, %v", len(got), err)
			}
		})
	}
}

func TestConcurrentPutsOfDifferentContent(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Compress = true
	const n = 64
	var wg sync.WaitGroup
	hashes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, _, existed, err := s.Put(bytes.NewReader(variant(i)))
			if err != nil || existed {
				t.Errorf("put %d: existed=%v err=%v", i, existed, err)
			}
			hashes[i] = h
		}()
	}
	wg.Wait()
	if got := objectFiles(t, s); got != n {
		t.Fatalf("%d files on disk, want %d", got, n)
	}
	for i, h := range hashes {
		if got, err := readAll(s, h); err != nil || !bytes.Equal(got, variant(i)) {
			t.Fatalf("object %d: %v", i, err)
		}
	}
}

func TestConcurrentReadsDuringCompressExisting(t *testing.T) {
	s, _ := Open(t.TempDir())
	const n = 64
	hashes := make([]string, n)
	docs := make([][]byte, n)
	for i := range hashes {
		docs[i] = variant(i)
		hashes[i], _, _, _ = s.Put(bytes.NewReader(docs[i]))
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := r; ; j++ {
				select {
				case <-done:
					return
				default:
				}
				i := j % n
				got, err := readAll(s, hashes[i])
				if err != nil || !bytes.Equal(got, docs[i]) {
					t.Errorf("read %d during compress: %d bytes, %v", i, len(got), err)
					return
				}
				reads.Add(1)
			}
		}()
	}
	// Puts and label changes of the same objects run alongside.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, _, existed, err := s.Put(bytes.NewReader(docs[i])); err != nil || !existed {
				t.Errorf("re-put %d: existed=%v err=%v", i, existed, err)
			}
			if err := s.SetLabel(hashes[i], "i", fmt.Sprint(i)); err != nil {
				t.Errorf("label %d: %v", i, err)
			}
		}
	}()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := s.CompressExisting(context.Background(), log); err != nil {
		t.Fatal(err)
	}
	close(done)
	wg.Wait()
	if reads.Load() == 0 {
		t.Fatal("no reads happened")
	}
	for i, h := range hashes {
		if plain, gz := onDisk(t, s, h); plain || !gz {
			t.Fatalf("object %d plain=%v gz=%v", i, plain, gz)
		}
	}
	if got := objectFiles(t, s); got != n {
		t.Fatalf("%d files on disk, want %d", got, n)
	}
}

func TestConcurrentPutAndDelete(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Compress = true
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, _, _, err := s.Put(bytes.NewReader(xmlDoc)); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			h, _, _, _ := s.Put(bytes.NewReader(pdfDoc))
			_, _, err := s.Delete(h, time.Now().Add(time.Hour))
			if err != nil && err != ErrNotFound {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// Each object is in at most one form, and xmlDoc is still there.
	if n := objectFiles(t, s); n > 2 {
		t.Fatalf("%d files on disk", n)
	}
	h, _, existed, _ := s.Put(bytes.NewReader(xmlDoc))
	if !existed {
		t.Fatal("xml object lost")
	}
	if got, err := readAll(s, h); err != nil || !bytes.Equal(got, xmlDoc) {
		t.Fatalf("read back: %v", err)
	}
}

func TestConcurrentSetLabelKeepsAll(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	h, _, _, _ := s.Put(bytes.NewReader(xmlDoc))
	const n = maxLabels
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.SetLabel(h, fmt.Sprintf("k%02d", i), fmt.Sprint(i)); err != nil {
				t.Error(err)
			}
			_, _ = s.Labels(h)
			_, _ = s.FindByLabels(map[string]string{"k00": ""})
		}()
	}
	wg.Wait()
	if m, _ := s.Labels(h); len(m) != n {
		t.Fatalf("%d labels, want %d", len(m), n)
	}
	_ = s.Close()
	s2, _ := Open(root)
	defer s2.Close()
	if m, _ := s2.Labels(h); len(m) != n {
		t.Fatalf("%d labels on disk, want %d", len(m), n)
	}
}

func TestConcurrentSetLabelAndDelete(t *testing.T) {
	s, _ := Open(t.TempDir())
	for round := 0; round < 20; round++ {
		h, _, _, _ := s.Put(bytes.NewReader(xmlDoc))
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := s.SetLabel(h, fmt.Sprintf("k%d", i), "v")
				if err != nil && err != ErrNotFound {
					t.Error(err)
				}
			}()
		}
		if _, _, err := s.Delete(h, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		// A label set after the delete got ErrNotFound; none may linger.
		if hasLabelRow(t, s, h) {
			t.Fatal("labels of a deleted object left in the database")
		}
		if got, _ := s.FindByLabels(map[string]string{"k0": ""}); len(got) != 0 {
			t.Fatalf("labels of a deleted object indexed: %v", got)
		}
	}
}
