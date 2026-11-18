package cas

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(t.TempDir(), "docs")
	auth, err := LoadConfig(writeConfig(t, `
namespaces:
  docs:
    path: `+custom+`
  images: {}
  empty:
keys:
  - key: "docs-key-0123456789"
    namespace: docs
    permissions: [READ, CREATE, DELETE, LABEL]
  - key: "images-key-0123456789"
    namespace: images
    permissions: [READ]
`), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(auth.Namespaces) != 3 || auth.KeyCount() != 2 {
		t.Fatalf("namespaces = %d, keys = %d", len(auth.Namespaces), auth.KeyCount())
	}
	for _, d := range []string{filepath.Join(custom, "objects"), filepath.Join(dir, "images", "objects"), filepath.Join(dir, "empty", "objects")} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("namespace dir: %v", err)
		}
	}
	if !auth.Namespaces["images"].Compress {
		t.Fatal("compress not set")
	}
	a := auth.lookup("images-key-0123456789")
	if a == nil || a.namespace != "images" || a.store != auth.Namespaces["images"] || !a.perms[PermRead] || a.perms[PermCreate] {
		t.Fatalf("images key = %+v", a)
	}
	if auth.lookup("images-key-012345678") != nil || auth.lookup("") != nil {
		t.Fatal("unknown key resolved")
	}
}

func TestLoadConfigMissingAndEmpty(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"), t.TempDir(), false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	auth, err := LoadConfig(writeConfig(t, ""), t.TempDir(), false)
	if err != nil || auth.KeyCount() != 0 || len(auth.Namespaces) != 0 {
		t.Fatalf("empty file: %v, %+v", err, auth)
	}
}

func TestLoadConfigRejectsInvalid(t *testing.T) {
	key := func(k, ns, perms string) string {
		return "\n  - key: \"" + k + "\"\n    namespace: " + ns + "\n    permissions: [" + perms + "]"
	}
	good := "0123456789abcdef"
	for name, c := range map[string]struct{ yaml, want string }{
		"malformed":        {"namespaces: [", "yaml"},
		"unknown field":    {"namespace:\n  a: {}", "field namespace not found"},
		"bad ns name":      {"namespaces:\n  Docs: {}", "invalid namespace name"},
		"ns name too long": {"namespaces:\n  " + strings.Repeat("a", 64) + ": {}", "invalid namespace name"},
		"shared path":      {"namespaces:\n  a: {path: /tmp/x}\n  b: {path: /tmp/x/}", "share the path"},
		"short key":        {"namespaces:\n  a: {}\nkeys:" + key("short", "a", "READ"), "shorter than 16"},
		"empty key":        {"namespaces:\n  a: {}\nkeys:" + key("", "a", "READ"), "shorter than 16"},
		"duplicate key":    {"namespaces:\n  a: {}\n  b: {}\nkeys:" + key(good, "a", "READ") + key(good, "b", "READ"), "duplicate key"},
		"unknown ns":       {"namespaces:\n  a: {}\nkeys:" + key(good, "b", "READ"), "unknown namespace"},
		"unknown perm":     {"namespaces:\n  a: {}\nkeys:" + key(good, "a", "READ, WRITE"), "unknown permission"},
		"lowercase perm":   {"namespaces:\n  a: {}\nkeys:" + key(good, "a", "read"), "unknown permission"},
		"no perms":         {"namespaces:\n  a: {}\nkeys:" + key(good, "a", ""), "no permissions"},
	} {
		_, err := LoadConfig(writeConfig(t, c.yaml), t.TempDir(), false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}
