package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-szalik/cas-storage/cas"
)

const testKey = "cli-key-0123456789abcdef"

func testServer(t *testing.T) string {
	t.Helper()
	auth, err := cas.Config{
		Namespaces: map[string]cas.NamespaceConfig{"t": {}},
		Keys: []cas.KeyConfig{{Key: testKey, Namespace: "t",
			Permissions: []cas.Permission{cas.PermRead, cas.PermCreate, cas.PermDelete, cas.PermLabel}}},
	}.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(cas.Handler(auth, slog.New(slog.DiscardHandler), true))
	t.Cleanup(func() {
		srv.Close()
		_ = auth.Close()
	})
	return srv.URL
}

// run executes cascli with args against url and returns its stdout.
func run(t *testing.T, url, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd(url, testKey, strings.NewReader(stdin), &out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func mustRun(t *testing.T, url, stdin string, args ...string) string {
	t.Helper()
	out, err := run(t, url, stdin, args...)
	if err != nil {
		t.Fatalf("cascli %v: %v", args, err)
	}
	return out
}

func TestCLI(t *testing.T) {
	url := testServer(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(src, []byte("hello cli"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash := strings.TrimSpace(mustRun(t, url, "", "put", src, "-l", "kind=txt", "-l", "src=test"))
	if !cas.ValidHash(hash) {
		t.Fatalf("put printed %q", hash)
	}
	if again := strings.TrimSpace(mustRun(t, url, "hello cli", "put", "-")); again != hash {
		t.Fatalf("put from stdin = %q, want %q", again, hash)
	}

	if got := mustRun(t, url, "", "get", hash); got != "hello cli" {
		t.Fatalf("get = %q", got)
	}
	dst := filepath.Join(dir, "out.txt")
	mustRun(t, url, "", "get", hash, "-o", dst)
	if b, _ := os.ReadFile(dst); string(b) != "hello cli" {
		t.Fatalf("get -o wrote %q", b)
	}

	mustRun(t, url, "", "exists", hash)
	if _, err := run(t, url, "", "exists", strings.Repeat("0", 64)); !errors.Is(err, errAbsent) {
		t.Fatalf("exists(absent) = %v", err)
	}

	mustRun(t, url, "", "label", "set", hash, "lang=en")
	mustRun(t, url, "", "label", "delete", hash, "src")
	if got := mustRun(t, url, "", "labels", hash); got != "kind=txt\nlang=en\n" {
		t.Fatalf("labels = %q", got)
	}
	if got := mustRun(t, url, "", "find", "kind=txt", "lang"); got != hash+"\n" {
		t.Fatalf("find = %q", got)
	}
	if got := mustRun(t, url, "", "find", "kind=pdf"); got != "" {
		t.Fatalf("find(no match) = %q", got)
	}

	if got := mustRun(t, url, "", "delete", hash, "--unused-before", "1h"); !strings.HasPrefix(got, "kept") {
		t.Fatalf("delete of a fresh object = %q", got)
	}
	future := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	if got := mustRun(t, url, "", "delete", hash, "--unused-before", future); got != "deleted 9 bytes\n" {
		t.Fatalf("delete = %q", got)
	}
	if got := mustRun(t, url, "", "delete", hash, "--unused-before", future); got != "absent 0 bytes\n" {
		t.Fatalf("delete again = %q", got)
	}
}

func TestCLIErrors(t *testing.T) {
	url := testServer(t)
	for key, want := range map[string]string{"wrong-key-0123456789": "401", "": "CAS_API_KEY is not set"} {
		var out bytes.Buffer
		cmd := newRootCmd(url, key, strings.NewReader("x"), &out)
		cmd.SetArgs([]string{"put", "-"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("key %q: %v, want %q", key, err, want)
		}
	}
	for _, args := range [][]string{
		{"put", "-", "-l", "novalue"},
		{"label", "set", strings.Repeat("0", 64), "k"},
		{"find", "k=v", "k=w"},
		{"delete", strings.Repeat("0", 64), "--unused-before", "yesterday"},
		{"delete", strings.Repeat("0", 64)},
	} {
		if _, err := run(t, url, "", args...); err == nil {
			t.Errorf("cascli %v: want an error", args)
		}
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"24h":                  now.Add(-24 * time.Hour),
		"0s":                   now,
		"2026-01-02T03:04:05Z": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	} {
		if got, err := parseTime(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseTime("-1h", now); err == nil {
		t.Error("negative duration accepted")
	}
}
