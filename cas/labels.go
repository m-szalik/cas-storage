package cas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Labels are a small key=value map per object, kept in
// <root>/labels/ab/cd/<hash>.json and mirrored in memory for search.

// ErrInvalidLabel is returned for a malformed label key or value, too many
// labels on one object, or an empty search.
var ErrInvalidLabel = errors.New("invalid label")

const (
	maxLabels     = 64
	maxLabelValue = 256
	labelSuffix   = ".json"
)

var labelKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

// labelIndex mirrors the label files: labels by hash and hashes by key and
// value. Writers hold the object's hash lock as well, so the file and the
// index change together.
type labelIndex struct {
	mu     sync.RWMutex
	byHash map[string]map[string]string
	byKey  map[string]map[string]map[string]struct{} // key -> value -> hashes
}

func validLabelKey(k string) bool { return labelKeyRe.MatchString(k) }

func validLabelValue(v string) bool {
	return len(v) > 0 && len(v) <= maxLabelValue && utf8.ValidString(v)
}

func checkLabels(m map[string]string) error {
	if len(m) > maxLabels {
		return fmt.Errorf("%w: more than %d labels", ErrInvalidLabel, maxLabels)
	}
	for k, v := range m {
		if !validLabelKey(k) {
			return fmt.Errorf("%w: key %q", ErrInvalidLabel, k)
		}
		if !validLabelValue(v) {
			return fmt.Errorf("%w: value of %q", ErrInvalidLabel, k)
		}
	}
	return nil
}

func (s *Store) labelPath(hash string) string {
	return filepath.Join(s.labelsDir, hash[0:2], hash[2:4], hash+labelSuffix)
}

// set replaces hash's labels in the index (none drops it).
func (x *labelIndex) set(hash string, m map[string]string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for k, v := range x.byHash[hash] {
		hs := x.byKey[k][v]
		delete(hs, hash)
		if len(hs) == 0 {
			delete(x.byKey[k], v)
			if len(x.byKey[k]) == 0 {
				delete(x.byKey, k)
			}
		}
	}
	if len(m) == 0 {
		delete(x.byHash, hash)
		return
	}
	x.byHash[hash] = m
	for k, v := range m {
		if x.byKey[k] == nil {
			x.byKey[k] = map[string]map[string]struct{}{}
		}
		if x.byKey[k][v] == nil {
			x.byKey[k][v] = map[string]struct{}{}
		}
		x.byKey[k][v][hash] = struct{}{}
	}
}

// current answers a copy of hash's labels, never nil.
func (x *labelIndex) current(hash string) map[string]string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	m := make(map[string]string, len(x.byHash[hash]))
	for k, v := range x.byHash[hash] {
		m[k] = v
	}
	return m
}

// loadLabels rebuilds the index from the label files. Unreadable or invalid
// files are skipped; files of objects that are gone (a crash inside Delete)
// are removed.
func (s *Store) loadLabels() error {
	s.labels.byHash = map[string]map[string]string{}
	s.labels.byKey = map[string]map[string]map[string]struct{}{}
	return filepath.WalkDir(s.labelsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == s.labelsDir {
				return fmt.Errorf("cas: labels: %w", err)
			}
			return nil
		}
		hash, ok := strings.CutSuffix(d.Name(), labelSuffix)
		if d.IsDir() || !ok || !ValidHash(hash) || p != s.labelPath(hash) {
			return nil
		}
		if !s.exists(s.objectPath(hash)) {
			_ = os.Remove(p)
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var m map[string]string
		if json.Unmarshal(b, &m) != nil || checkLabels(m) != nil {
			return nil
		}
		s.labels.set(hash, m)
		return nil
	})
}

// writeLabels stores m as hash's labels (none removes the file) and updates
// the index. The caller holds hash's lock.
func (s *Store) writeLabels(hash string, m map[string]string) error {
	p := s.labelPath(hash)
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cas: remove labels: %w", err)
		}
		s.labels.set(hash, nil)
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := s.writeFileAtomic(p, b); err != nil {
		return fmt.Errorf("cas: write labels: %w", err)
	}
	s.labels.set(hash, m)
	return nil
}

// writeFileAtomic writes b to a temp file, flushes it and renames it to p, so
// p is always either the old or the new content; the directory is synced so
// the rename survives a crash.
func (s *Store) writeFileAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(s.tmpDir, "labels-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := mkdirAll(filepath.Dir(p)); err != nil {
		return err
	}
	if err := os.Rename(name, p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// dropLabels removes all of hash's labels. The caller holds hash's lock.
func (s *Store) dropLabels(hash string) error { return s.writeLabels(hash, nil) }

// Labels answers the object's labels (empty when it has none).
func (s *Store) Labels(hash string) (map[string]string, error) {
	if !ValidHash(hash) || !s.exists(s.objectPath(hash)) {
		return nil, ErrNotFound
	}
	return s.labels.current(hash), nil
}

// SetLabel sets the object's label key to value.
func (s *Store) SetLabel(hash, key, value string) error {
	if !ValidHash(hash) {
		return ErrNotFound
	}
	if !validLabelKey(key) {
		return fmt.Errorf("%w: key %q", ErrInvalidLabel, key)
	}
	if !validLabelValue(value) {
		return fmt.Errorf("%w: value of %q", ErrInvalidLabel, key)
	}
	defer s.lock(hash)()
	if !s.exists(s.objectPath(hash)) {
		return ErrNotFound
	}
	m := s.labels.current(hash)
	if old, ok := m[key]; ok && old == value {
		return nil
	}
	m[key] = value
	if err := checkLabels(m); err != nil {
		return err
	}
	return s.writeLabels(hash, m)
}

// DeleteLabel removes the object's label key; a missing label is not an error.
func (s *Store) DeleteLabel(hash, key string) error {
	if !ValidHash(hash) {
		return ErrNotFound
	}
	if !validLabelKey(key) {
		return fmt.Errorf("%w: key %q", ErrInvalidLabel, key)
	}
	defer s.lock(hash)()
	if !s.exists(s.objectPath(hash)) {
		return ErrNotFound
	}
	m := s.labels.current(hash)
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return s.writeLabels(hash, m)
}

// FindByLabels answers, sorted, the hashes of the objects that carry every
// selector key with its value; an empty value matches any value.
func (s *Store) FindByLabels(sel map[string]string) ([]string, error) {
	if len(sel) == 0 {
		return nil, fmt.Errorf("%w: no selector", ErrInvalidLabel)
	}
	for k, v := range sel {
		if !validLabelKey(k) {
			return nil, fmt.Errorf("%w: key %q", ErrInvalidLabel, k)
		}
		if v != "" && !validLabelValue(v) {
			return nil, fmt.Errorf("%w: value of %q", ErrInvalidLabel, k)
		}
	}
	x := &s.labels
	x.mu.RLock()
	defer x.mu.RUnlock()
	// Scan the hashes of the most selective selector, check the rest.
	var cands []map[string]struct{}
	best := -1
	for k, v := range sel {
		var sets []map[string]struct{}
		if v != "" {
			sets = []map[string]struct{}{x.byKey[k][v]}
		} else {
			for _, hs := range x.byKey[k] {
				sets = append(sets, hs)
			}
		}
		n := 0
		for _, hs := range sets {
			n += len(hs)
		}
		if best < 0 || n < best {
			cands, best = sets, n
		}
	}
	out := make([]string, 0, best)
	for _, hs := range cands {
		for h := range hs {
			if matches(x.byHash[h], sel) {
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func matches(m, sel map[string]string) bool {
	for k, v := range sel {
		got, ok := m[k]
		if !ok || (v != "" && got != v) {
			return false
		}
	}
	return true
}
