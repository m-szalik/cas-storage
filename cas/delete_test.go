package cas

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// An object put again after the cut-off is in use and stays; one put only
// before it goes.
func TestDeleteSparesARecentlyPutObject(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, _, _, err := s.Put(strings.NewReader("annex"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.objectPath(hash), old, old); err != nil {
		t.Fatal(err)
	}
	cut := time.Now().Add(-24 * time.Hour)

	// A writer puts it again: its mtime moves to now.
	if _, _, existed, err := s.Put(strings.NewReader("annex")); err != nil || !existed {
		t.Fatalf("re-put existed=%v err=%v", existed, err)
	}
	if size, deleted, err := s.Delete(hash, cut); err != nil || deleted || size != 5 {
		t.Fatalf("delete of a re-put object = %d, %v, %v", size, deleted, err)
	}

	if err := os.Chtimes(s.objectPath(hash), old, old); err != nil {
		t.Fatal(err)
	}
	if size, deleted, err := s.Delete(hash, cut); err != nil || !deleted || size != 5 {
		t.Fatalf("delete = %d, %v, %v", size, deleted, err)
	}
	if _, _, err := s.Delete(hash, cut); err != ErrNotFound {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestDeleteRouteNeedsAllowDelete(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, _, _, _ := s.Put(bytes.NewReader([]byte("x")))
	q := "?unused_before=" + url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	off := httptest.NewServer(Handler(s, log, false))
	defer off.Close()
	req, _ := http.NewRequest(http.MethodDelete, off.URL+"/objects/"+hash+q, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("delete without allowDelete = %d", resp.StatusCode)
	}

	on := httptest.NewServer(Handler(s, log, true))
	defer on.Close()
	req, _ = http.NewRequest(http.MethodDelete, on.URL+"/objects/"+hash+q, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var res DeleteResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !res.Deleted || res.Size != 1 {
		t.Fatalf("delete = %d %+v", resp.StatusCode, res)
	}
	req, _ = http.NewRequest(http.MethodDelete, on.URL+"/objects/"+hash, nil)
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete without unused_before = %d", resp.StatusCode)
	}
}
