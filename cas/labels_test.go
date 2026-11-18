package cas

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func putString(t *testing.T, s *Store, content string) string {
	t.Helper()
	hash, _, _, err := s.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestLabelsCRUD(t *testing.T) {
	s, _ := Open(t.TempDir())
	h := putString(t, s, "labelled")

	if m, err := s.Labels(h); err != nil || m == nil || len(m) != 0 {
		t.Fatalf("labels of fresh object = %v, %v", m, err)
	}
	for k, v := range map[string]string{"source": "ted", "lang": "pl", "a/b.c-d_e": "x"} {
		if err := s.SetLabel(h, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetLabel(h, "lang", "en"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"source": "ted", "lang": "en", "a/b.c-d_e": "x"}
	if m, _ := s.Labels(h); !reflect.DeepEqual(m, want) {
		t.Fatalf("labels = %v, want %v", m, want)
	}
	if err := s.DeleteLabel(h, "lang"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLabel(h, "lang"); err != nil {
		t.Fatalf("delete again = %v", err)
	}
	delete(want, "lang")
	if m, _ := s.Labels(h); !reflect.DeepEqual(m, want) {
		t.Fatalf("labels = %v, want %v", m, want)
	}
	// The file goes once the last label does.
	_ = s.DeleteLabel(h, "source")
	_ = s.DeleteLabel(h, "a/b.c-d_e")
	if _, err := os.Stat(s.labelPath(h)); !os.IsNotExist(err) {
		t.Fatalf("label file still there: %v", err)
	}
}

func TestLabelsValidation(t *testing.T) {
	s, _ := Open(t.TempDir())
	h := putString(t, s, "validated")
	for _, kv := range [][2]string{
		{"", "v"}, {"-k", "v"}, {"k k", "v"}, {strings.Repeat("k", 64), "v"},
		{"k", ""}, {"k", strings.Repeat("v", 257)}, {"k", "\xff"},
	} {
		if err := s.SetLabel(h, kv[0], kv[1]); !errors.Is(err, ErrInvalidLabel) {
			t.Fatalf("SetLabel(%q, %q) = %v", kv[0], kv[1], err)
		}
	}
	if err := s.SetLabel(h, strings.Repeat("k", 63), strings.Repeat("v", 256)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLabel(h, "bad key"); !errors.Is(err, ErrInvalidLabel) {
		t.Fatalf("DeleteLabel bad key = %v", err)
	}
	for i := 1; i < maxLabels; i++ {
		if err := s.SetLabel(h, fmt.Sprintf("k%02d", i), "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetLabel(h, "one-too-many", "v"); !errors.Is(err, ErrInvalidLabel) {
		t.Fatalf("65th label = %v", err)
	}
	if err := s.SetLabel(h, "k01", "changed"); err != nil {
		t.Fatalf("changing an existing label at the limit = %v", err)
	}
}

func TestLabelsOnAbsentObject(t *testing.T) {
	s, _ := Open(t.TempDir())
	absent := strings.Repeat("a", 64)
	if _, err := s.Labels(absent); err != ErrNotFound {
		t.Fatalf("Labels = %v", err)
	}
	if _, err := s.Labels("nope"); err != ErrNotFound {
		t.Fatalf("Labels bad hash = %v", err)
	}
	if err := s.SetLabel(absent, "k", "v"); err != ErrNotFound {
		t.Fatalf("SetLabel = %v", err)
	}
	if err := s.DeleteLabel(absent, "k"); err != ErrNotFound {
		t.Fatalf("DeleteLabel = %v", err)
	}
}

func TestFindByLabels(t *testing.T) {
	s, _ := Open(t.TempDir())
	a, b, c := putString(t, s, "a"), putString(t, s, "b"), putString(t, s, "c")
	_ = s.SetLabel(a, "src", "ted")
	_ = s.SetLabel(a, "lang", "pl")
	_ = s.SetLabel(b, "src", "ted")
	_ = s.SetLabel(b, "lang", "en")
	_ = s.SetLabel(c, "src", "bzp")

	sorted := func(hs ...string) []string { sort.Strings(hs); return hs }
	for _, tc := range []struct {
		sel  map[string]string
		want []string
	}{
		{map[string]string{"src": "ted"}, sorted(a, b)},
		{map[string]string{"src": "ted", "lang": "pl"}, []string{a}},
		{map[string]string{"src": ""}, sorted(a, b, c)},
		{map[string]string{"lang": "", "src": "ted"}, sorted(a, b)},
		{map[string]string{"lang": "", "src": "bzp"}, []string{}},
		{map[string]string{"missing": ""}, []string{}},
	} {
		got, err := s.FindByLabels(tc.sel)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("FindByLabels(%v) = %v, %v; want %v", tc.sel, got, err, tc.want)
		}
	}
	if _, err := s.FindByLabels(nil); !errors.Is(err, ErrInvalidLabel) {
		t.Fatalf("empty selector = %v", err)
	}
	if _, err := s.FindByLabels(map[string]string{"bad key": ""}); !errors.Is(err, ErrInvalidLabel) {
		t.Fatalf("bad key = %v", err)
	}
}

func TestLabelsSurviveReopen(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	h := putString(t, s, "persistent")
	_ = s.SetLabel(h, "src", "ted")
	_ = s.SetLabel(h, "lang", "pl")
	// Junk in the labels tree is ignored.
	junk := filepath.Join(root, "labels", "ab", "cd", strings.Repeat("a", 64)+".json")
	_ = os.MkdirAll(filepath.Dir(junk), 0o755)
	_ = os.WriteFile(junk, []byte("{not json"), 0o644)

	s2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"src": "ted", "lang": "pl"}
	if m, _ := s2.Labels(h); !reflect.DeepEqual(m, want) {
		t.Fatalf("labels after reopen = %v", m)
	}
	if got, _ := s2.FindByLabels(map[string]string{"lang": "pl"}); !reflect.DeepEqual(got, []string{h}) {
		t.Fatalf("search after reopen = %v", got)
	}
}

func TestDeleteRemovesLabels(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	h, _, _, _ := s.Put(bytes.NewReader(xmlDoc))
	_ = s.SetLabel(h, "src", "ted")
	if _, deleted, err := s.Delete(h, time.Now().Add(time.Second)); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if _, err := os.Stat(s.labelPath(h)); !os.IsNotExist(err) {
		t.Fatalf("label file still there: %v", err)
	}
	if got, _ := s.FindByLabels(map[string]string{"src": ""}); len(got) != 0 {
		t.Fatalf("deleted object still found: %v", got)
	}
	// Put again: it comes back without the old labels.
	_, _, _, _ = s.Put(bytes.NewReader(xmlDoc))
	if m, err := s.Labels(h); err != nil || len(m) != 0 {
		t.Fatalf("labels after re-put = %v, %v", m, err)
	}
	// A label file left by a crash inside Delete is dropped at Open.
	s.Delete(h, time.Now().Add(time.Second))
	orphan := s.labelPath(h)
	_ = os.MkdirAll(filepath.Dir(orphan), 0o755)
	_ = os.WriteFile(orphan, []byte(`{"src":"ted"}`), 0o644)
	s2, _ := Open(root)
	if got, _ := s2.FindByLabels(map[string]string{"src": ""}); len(got) != 0 {
		t.Fatalf("orphan labels loaded: %v", got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan label file kept: %v", err)
	}
}
