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

func TestRoundTrip(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(cas.Handler(store, slog.New(slog.DiscardHandler), true))
	defer srv.Close()
	c := New(srv.URL)
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
