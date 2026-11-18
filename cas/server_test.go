package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const testKey = "test-key-0123456789"

// testAuth grants testKey perms (all when none given) on s as namespace "test".
func testAuth(s *Store, perms ...Permission) *Auth {
	if len(perms) == 0 {
		perms = []Permission{PermRead, PermCreate, PermDelete, PermLabel}
	}
	a := &access{sum: sha256.Sum256([]byte(testKey)), namespace: "test", store: s, perms: map[Permission]bool{}}
	for _, p := range perms {
		a.perms[p] = true
	}
	return &Auth{Namespaces: map[string]*Store{"test": s}, keys: map[[sha256.Size]byte]*access{a.sum: a}}
}

// withTestKey sends testKey with every request that has no key of its own.
func withTestKey(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") == "" && r.Header.Get("Authorization") == "" {
			r.Header.Set("X-API-Key", testKey)
		}
		h.ServeHTTP(w, r)
	})
}

// call sends method path body with the given headers (name, value pairs) and
// returns the status and body.
func call(t *testing.T, h http.Handler, method, path, body string, headers ...string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// twoNamespaces opens namespaces a and b with an all-permission key each.
func twoNamespaces(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	keyA, keyB := "key-a-0123456789abcdef", "key-b-0123456789abcdef"
	all := []Permission{PermRead, PermCreate, PermDelete, PermLabel}
	auth, err := Config{
		Namespaces: map[string]NamespaceConfig{"a": {}, "b": {}},
		Keys: []KeyConfig{
			{Key: keyA, Namespace: "a", Permissions: all},
			{Key: keyB, Namespace: "b", Permissions: all},
		},
	}.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	return Handler(auth, slog.New(slog.DiscardHandler), true), keyA, keyB
}

func TestAuthRejectsMissingAndUnknownKeys(t *testing.T) {
	s, _ := Open(t.TempDir())
	h := Handler(testAuth(s), slog.New(slog.DiscardHandler), true)
	path := "/objects/" + strings.Repeat("0", 64)
	for _, hdr := range [][]string{
		nil,
		{"X-API-Key", "wrong-key-0123456789"},
		{"Authorization", "Bearer wrong-key-0123456789"},
		{"Authorization", "Basic " + testKey},
		{"X-API-Key", testKey[:len(testKey)-1]},
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if hdr != nil {
			req.Header.Set(hdr[0], hdr[1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%v: %d %q", hdr, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
	for _, hdr := range [][]string{{"X-API-Key", testKey}, {"Authorization", "Bearer " + testKey}, {"Authorization", "bearer " + testKey}} {
		if code, _ := call(t, h, http.MethodGet, path, "", hdr...); code != http.StatusNotFound {
			t.Fatalf("%v: %d, want 404", hdr, code)
		}
	}
}

func TestHealthzNeedsNoKey(t *testing.T) {
	h := Handler(&Auth{}, slog.New(slog.DiscardHandler), false)
	if code, body := call(t, h, http.MethodGet, "/healthz", ""); code != http.StatusOK || body != "ok" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	if code, _ := call(t, h, http.MethodPost, "/objects", "x"); code != http.StatusUnauthorized {
		t.Fatalf("POST without keys = %d, want 401", code)
	}
}

func TestPermissions(t *testing.T) {
	s, _ := Open(t.TempDir())
	hash, _, _, _ := s.Put(strings.NewReader("x"))
	obj := "/objects/" + hash
	routes := []struct {
		perm         Permission
		method, path string
	}{
		{PermRead, http.MethodGet, obj},
		{PermRead, http.MethodHead, obj},
		{PermRead, http.MethodGet, obj + "/labels"},
		{PermRead, http.MethodGet, "/objects?label=k"},
		{PermCreate, http.MethodPost, "/objects"},
		{PermDelete, http.MethodDelete, obj + "?unused_before=2000-01-01T00:00:00Z"},
		{PermLabel, http.MethodPut, obj + "/labels/k"},
		{PermLabel, http.MethodDelete, obj + "/labels/k"},
	}
	for _, granted := range []Permission{PermRead, PermCreate, PermDelete, PermLabel} {
		h := withTestKey(Handler(testAuth(s, granted), slog.New(slog.DiscardHandler), true))
		for _, rt := range routes {
			code, _ := call(t, h, rt.method, rt.path, "v")
			if forbidden := code == http.StatusForbidden; forbidden != (rt.perm != granted) {
				t.Errorf("key with %s: %s %s = %d", granted, rt.method, rt.path, code)
			}
		}
	}
}

func TestLabelsAndSearch(t *testing.T) {
	s, _ := Open(t.TempDir())
	h := withTestKey(Handler(testAuth(s), slog.New(slog.DiscardHandler), false))
	_, out := call(t, h, http.MethodPost, "/objects", "one")
	var put PutResult
	_ = json.Unmarshal([]byte(out), &put)
	_, out = call(t, h, http.MethodPost, "/objects", "two")
	var put2 PutResult
	_ = json.Unmarshal([]byte(out), &put2)
	obj := "/objects/" + put.Hash

	if code, body := call(t, h, http.MethodGet, obj+"/labels", ""); code != http.StatusOK || strings.TrimSpace(body) != "{}" {
		t.Fatalf("labels of unlabelled = %d %q", code, body)
	}
	for _, kv := range [][3]string{{put.Hash, "kind", "pdf"}, {put.Hash, "src/site", "a.example"}, {put2.Hash, "kind", "xml"}} {
		if code, body := call(t, h, http.MethodPut, "/objects/"+kv[0]+"/labels/"+kv[1], kv[2]); code != http.StatusNoContent {
			t.Fatalf("PUT label %v = %d %q", kv, code, body)
		}
	}
	code, body := call(t, h, http.MethodGet, obj+"/labels", "")
	var labels map[string]string
	_ = json.Unmarshal([]byte(body), &labels)
	if code != http.StatusOK || len(labels) != 2 || labels["kind"] != "pdf" || labels["src/site"] != "a.example" {
		t.Fatalf("labels = %d %q", code, body)
	}

	find := func(query string) []string {
		t.Helper()
		code, body := call(t, h, http.MethodGet, "/objects?"+query, "")
		if code != http.StatusOK {
			t.Fatalf("find %s = %d %q", query, code, body)
		}
		var res FindResult
		_ = json.Unmarshal([]byte(body), &res)
		if res.Hashes == nil {
			t.Fatalf("find %s: hashes is not a list: %q", query, body)
		}
		return res.Hashes
	}
	if got := find("label=kind=pdf"); len(got) != 1 || got[0] != put.Hash {
		t.Fatalf("kind=pdf = %v", got)
	}
	if got := find("label=kind"); len(got) != 2 {
		t.Fatalf("kind = %v", got)
	}
	if got := find("label=kind&label=src/site=a.example"); len(got) != 1 || got[0] != put.Hash {
		t.Fatalf("kind AND src/site = %v", got)
	}
	if got := find("label=kind=none"); len(got) != 0 {
		t.Fatalf("kind=none = %v", got)
	}

	if code, _ := call(t, h, http.MethodDelete, obj+"/labels/kind", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE label = %d", code)
	}
	if code, _ := call(t, h, http.MethodDelete, obj+"/labels/kind", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE label again = %d", code)
	}
	if got := find("label=kind=pdf"); len(got) != 0 {
		t.Fatalf("kind=pdf after delete = %v", got)
	}

	missing := "/objects/" + strings.Repeat("0", 64)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, missing + "/labels", "", http.StatusNotFound},
		{http.MethodPut, missing + "/labels/k", "v", http.StatusNotFound},
		{http.MethodDelete, missing + "/labels/k", "", http.StatusNotFound},
		{http.MethodPut, obj + "/labels/-bad", "v", http.StatusBadRequest},
		{http.MethodPut, obj + "/labels/k", "", http.StatusBadRequest},
		{http.MethodPut, obj + "/labels/k", strings.Repeat("v", maxLabelBody+1), http.StatusRequestEntityTooLarge},
		{http.MethodGet, "/objects", "", http.StatusBadRequest},
		{http.MethodGet, "/objects?label=", "", http.StatusBadRequest},
		{http.MethodGet, "/objects?label=k=a&label=k=b", "", http.StatusBadRequest},
	} {
		if code, body := call(t, h, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s = %d %q, want %d", c.method, c.path, code, body, c.want)
		}
	}
}

func TestNamespacesAreIsolated(t *testing.T) {
	h, keyA, keyB := twoNamespaces(t)
	code, out := call(t, h, http.MethodPost, "/objects", "only in a", "X-API-Key", keyA)
	if code != http.StatusCreated {
		t.Fatalf("put = %d", code)
	}
	var put PutResult
	_ = json.Unmarshal([]byte(out), &put)
	obj := "/objects/" + put.Hash
	if code, _ := call(t, h, http.MethodPut, obj+"/labels/k", "v", "X-API-Key", keyA); code != http.StatusNoContent {
		t.Fatalf("label = %d", code)
	}

	if code, _ := call(t, h, http.MethodGet, obj, "", "X-API-Key", keyA); code != http.StatusOK {
		t.Fatalf("GET in a = %d", code)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, obj, ""},
		{http.MethodHead, obj, ""},
		{http.MethodGet, obj + "/labels", ""},
		{http.MethodPut, obj + "/labels/k", "v"},
		{http.MethodDelete, obj + "?unused_before=2100-01-01T00:00:00Z", ""},
	} {
		if code, _ := call(t, h, c.method, c.path, c.body, "Authorization", "Bearer "+keyB); code != http.StatusNotFound {
			t.Errorf("%s %s in b = %d, want 404", c.method, c.path, code)
		}
	}
	if _, body := call(t, h, http.MethodGet, "/objects?label=k", "", "X-API-Key", keyB); strings.Contains(body, put.Hash) {
		t.Fatalf("search in b found a's object: %q", body)
	}
	// Same content put in b is a separate copy, created there.
	if code, _ := call(t, h, http.MethodPost, "/objects", "only in a", "X-API-Key", keyB); code != http.StatusCreated {
		t.Fatalf("put in b = %d, want 201", code)
	}
	if code, body := call(t, h, http.MethodGet, obj+"/labels", "", "X-API-Key", keyB); code != http.StatusOK || strings.TrimSpace(body) != "{}" {
		t.Fatalf("labels in b = %d %q", code, body)
	}
}

// The request log names the namespace but never the key.
func TestRequestLogHasNamespaceNotKey(t *testing.T) {
	s, _ := Open(t.TempDir())
	var buf bytes.Buffer
	h := withTestKey(Handler(testAuth(s), slog.New(slog.NewTextHandler(&buf, nil)), false))
	call(t, h, http.MethodPost, "/objects", "x")
	out, _ := io.ReadAll(&buf)
	if !strings.Contains(string(out), "namespace=test") || strings.Contains(string(out), testKey) {
		t.Fatalf("log = %s", out)
	}
}

func TestPutRejectsTooLargeUpload(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.MaxUploadSize = 10
	h := withTestKey(Handler(testAuth(s), slog.New(slog.DiscardHandler), false))
	if code, _ := call(t, h, "POST", "/objects", "0123456789"); code != http.StatusCreated {
		t.Fatalf("at the limit: %d", code)
	}
	if code, _ := call(t, h, "POST", "/objects", "0123456789a"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit (Content-Length): %d", code)
	}
	// Without Content-Length (chunked) the limit cuts the stream.
	req := httptest.NewRequest("POST", "/objects", io.MultiReader(strings.NewReader("01234"), strings.NewReader("56789abc")))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit (chunked): %d", rec.Code)
	}
	if entries, _ := os.ReadDir(s.tmpDir); len(entries) != 0 {
		t.Fatalf("temp files left: %d", len(entries))
	}
}
