package client

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m-szalik/cas-storage/cas"
)

const testKey = "client-key-0123456789"

// newServer serves one namespace; testKey may do perms (all when none given).
func newServer(t *testing.T, perms ...cas.Permission) *httptest.Server {
	t.Helper()
	if len(perms) == 0 {
		perms = []cas.Permission{cas.PermRead, cas.PermCreate, cas.PermDelete, cas.PermLabel}
	}
	auth, err := cas.Config{
		Namespaces: map[string]cas.NamespaceConfig{"test": {}},
		Keys:       []cas.KeyConfig{{Key: testKey, Namespace: "test", Permissions: perms}},
	}.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(cas.Handler(auth, slog.New(slog.DiscardHandler), true))
	t.Cleanup(srv.Close)
	return srv
}

func TestRoundTrip(t *testing.T) {
	srv := newServer(t)
	c := New(srv.URL, WithAPIKey(testKey))
	ctx := context.Background()

	body := []byte("hello, cas")
	hash, err := c.PutObject(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := c.PutStream(ctx, bytes.NewReader(body))
	if err != nil || streamed != hash {
		t.Fatalf("PutStream = %q, %v; want %q", streamed, err, hash)
	}

	got, err := c.GetObject(ctx, hash)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("GetObject = %q, %v", got, err)
	}
	r, err := c.OpenObject(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	streamedBody, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(streamedBody, body) {
		t.Fatalf("OpenObject = %q", streamedBody)
	}

	if ok, err := c.HeadObject(ctx, hash); err != nil || !ok {
		t.Fatalf("HeadObject = %v, %v", ok, err)
	}
	missing := strings.Repeat("0", 64)
	if ok, err := c.HeadObject(ctx, missing); err != nil || ok {
		t.Fatalf("HeadObject(missing) = %v, %v", ok, err)
	}

	size, deleted, err := c.DeleteObject(ctx, hash, time.Now().Add(time.Minute))
	if err != nil || !deleted || size != int64(len(body)) {
		t.Fatalf("DeleteObject = %d, %v, %v", size, deleted, err)
	}
	if _, deleted, err := c.DeleteObject(ctx, hash, time.Now().Add(time.Minute)); err != nil || deleted {
		t.Fatalf("DeleteObject(again) = %v, %v", deleted, err)
	}
}

func TestLabels(t *testing.T) {
	srv := newServer(t)
	c := New(srv.URL, WithAPIKey(testKey))
	ctx := context.Background()

	a, _ := c.PutObject(ctx, []byte("a"))
	b, _ := c.PutObject(ctx, []byte("b"))
	if labels, err := c.Labels(ctx, a); err != nil || len(labels) != 0 {
		t.Fatalf("Labels(unlabelled) = %v, %v", labels, err)
	}
	for _, l := range [][3]string{{a, "kind", "pdf"}, {a, "src/site", "x y&z"}, {b, "kind", "xml"}} {
		if err := c.SetLabel(ctx, l[0], l[1], l[2]); err != nil {
			t.Fatalf("SetLabel %v: %v", l, err)
		}
	}
	labels, err := c.Labels(ctx, a)
	if err != nil || len(labels) != 2 || labels["kind"] != "pdf" || labels["src/site"] != "x y&z" {
		t.Fatalf("Labels = %v, %v", labels, err)
	}
	if got, err := c.FindByLabels(ctx, map[string]string{"kind": "pdf", "src/site": "x y&z"}); err != nil || len(got) != 1 || got[0] != a {
		t.Fatalf("FindByLabels(kind=pdf, src/site) = %v, %v", got, err)
	}
	if got, err := c.FindByLabels(ctx, map[string]string{"kind": ""}); err != nil || len(got) != 2 {
		t.Fatalf("FindByLabels(kind) = %v, %v", got, err)
	}
	if err := c.DeleteLabel(ctx, a, "src/site"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLabel(ctx, a, "src/site"); err != nil {
		t.Fatalf("DeleteLabel(again) = %v", err)
	}
	if got, err := c.FindByLabels(ctx, map[string]string{"src/site": ""}); err != nil || len(got) != 0 {
		t.Fatalf("FindByLabels(src/site) after delete = %v, %v", got, err)
	}

	missing := strings.Repeat("0", 64)
	if _, err := c.Labels(ctx, missing); err == nil {
		t.Fatal("Labels(missing): no error")
	}
	if err := c.SetLabel(ctx, missing, "k", "v"); err == nil {
		t.Fatal("SetLabel(missing): no error")
	}
	if _, err := c.FindByLabels(ctx, nil); err == nil {
		t.Fatal("FindByLabels(nil): no error")
	}
}

func TestAPIKey(t *testing.T) {
	srv := newServer(t, cas.PermRead)
	ctx := context.Background()
	if _, err := New(srv.URL).PutObject(ctx, []byte("x")); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("PutObject without key = %v, want 401", err)
	}
	if _, err := New(srv.URL, WithAPIKey(testKey)).PutObject(ctx, []byte("x")); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("PutObject read-only = %v, want 403", err)
	}
	if ok, err := New(srv.URL, WithAPIKey(testKey)).HeadObject(ctx, strings.Repeat("0", 64)); err != nil || ok {
		t.Fatalf("HeadObject = %v, %v", ok, err)
	}
}
