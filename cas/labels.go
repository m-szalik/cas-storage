package cas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

// Labels are a small key=value map per object, kept in the bbolt database
// <root>/labels.db:
//
//	bucket "labels": <hash> -> JSON map of the object's labels
//	bucket "index":  <key> 0x00 <value> 0x00 <hash> -> empty (for search)

// ErrInvalidLabel is returned for a malformed label key or value, too many
// labels on one object, or an empty search.
var ErrInvalidLabel = errors.New("invalid label")

const (
	maxLabels     = 64
	maxLabelValue = 256
	labelsDBFile  = "labels.db"
)

var (
	labelKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

	labelsBucket = []byte("labels")
	indexBucket  = []byte("index")
)

func validLabelKey(k string) bool { return labelKeyRe.MatchString(k) }

// validLabelValue also rejects NUL, the separator of the index keys.
func validLabelValue(v string) bool {
	return len(v) > 0 && len(v) <= maxLabelValue && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
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

// indexKey is the index entry of hash's label k=v; indexPrefix(k, v) is its
// prefix, indexPrefix(k, "") the prefix of k with any value.
func indexKey(k, v, hash string) []byte { return append(indexPrefix(k, v), hash...) }

func indexPrefix(k, v string) []byte {
	b := append([]byte(k), 0)
	if v != "" {
		b = append(append(b, v...), 0)
	}
	return b
}

// openLabels opens (or creates) the label database and imports the label
// files of the earlier <root>/labels/ab/cd/<hash>.json layout.
func (s *Store) openLabels(root string) error {
	db, err := bolt.Open(filepath.Join(root, labelsDBFile), 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("cas: open labels: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{labelsBucket, indexBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.db = db
		err = s.importLabelFiles(filepath.Join(root, "labels"))
	}
	if err != nil {
		_ = db.Close()
		s.db = nil
		return fmt.Errorf("cas: labels: %w", err)
	}
	return nil
}

// importLabelFiles moves the labels of the file layout
// (<dir>/ab/cd/<hash>.json) into the database and removes those files and the
// directories they leave empty. Invalid files and those of absent objects are
// dropped; anything else in dir is left alone.
func (s *Store) importLabelFiles(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	var files, dirs []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			hash, ok := strings.CutSuffix(d.Name(), ".json")
			if !ok || !ValidHash(hash) || p != filepath.Join(dir, hash[0:2], hash[2:4], d.Name()) {
				return nil
			}
			files = append(files, p)
			if !s.exists(hash) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			var m map[string]string
			if json.Unmarshal(b, &m) != nil || checkLabels(m) != nil {
				return nil
			}
			return putLabels(tx, hash, m)
		})
	})
	if err != nil {
		return fmt.Errorf("import %s: %w", dir, err)
	}
	for _, f := range files {
		_ = os.Remove(f)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only empty ones go
	}
	return nil
}

// Close closes the label database.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// getLabels answers hash's labels, never nil.
func getLabels(tx *bolt.Tx, hash string) (map[string]string, error) {
	m := map[string]string{}
	if b := tx.Bucket(labelsBucket).Get([]byte(hash)); b != nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("cas: labels of %s: %w", hash, err)
		}
	}
	return m, nil
}

// putLabels replaces hash's labels and their index entries (none removes
// them).
func putLabels(tx *bolt.Tx, hash string, m map[string]string) error {
	old, err := getLabels(tx, hash)
	if err != nil {
		return err
	}
	idx := tx.Bucket(indexBucket)
	for k, v := range old {
		if err := idx.Delete(indexKey(k, v, hash)); err != nil {
			return err
		}
	}
	if len(m) == 0 {
		return tx.Bucket(labelsBucket).Delete([]byte(hash))
	}
	for k, v := range m {
		if err := idx.Put(indexKey(k, v, hash), nil); err != nil {
			return err
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return tx.Bucket(labelsBucket).Put([]byte(hash), b)
}

// dropLabels removes all of hash's labels. The caller holds hash's lock.
func (s *Store) dropLabels(hash string) error {
	return s.db.Batch(func(tx *bolt.Tx) error { return putLabels(tx, hash, nil) })
}

// Labels answers the object's labels (empty when it has none).
func (s *Store) Labels(hash string) (m map[string]string, err error) {
	if !ValidHash(hash) || !s.exists(hash) {
		return nil, ErrNotFound
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		m, err = getLabels(tx, hash)
		return err
	})
	return m, err
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
	return s.updateLabels(hash, func(m map[string]string) bool {
		if old, ok := m[key]; ok && old == value {
			return false
		}
		m[key] = value
		return true
	})
}

// DeleteLabel removes the object's label key; a missing label is not an error.
func (s *Store) DeleteLabel(hash, key string) error {
	if !ValidHash(hash) {
		return ErrNotFound
	}
	if !validLabelKey(key) {
		return fmt.Errorf("%w: key %q", ErrInvalidLabel, key)
	}
	return s.updateLabels(hash, func(m map[string]string) bool {
		if _, ok := m[key]; !ok {
			return false
		}
		delete(m, key)
		return true
	})
}

// updateLabels applies change to the labels of an existing object, under its
// lock (so it cannot race a Delete), and stores them when change reports a
// difference. Concurrent updates share one bbolt commit (Batch); change may
// run more than once, so it must only depend on the map it gets.
func (s *Store) updateLabels(hash string, change func(map[string]string) bool) error {
	defer s.lock(hash)()
	if !s.exists(hash) {
		return ErrNotFound
	}
	return s.db.Batch(func(tx *bolt.Tx) error {
		m, err := getLabels(tx, hash)
		if err != nil {
			return err
		}
		if !change(m) {
			return nil
		}
		if err := checkLabels(m); err != nil {
			return err
		}
		return putLabels(tx, hash, m)
	})
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
	var found map[string]int // hash -> number of selectors it matched
	err := s.db.View(func(tx *bolt.Tx) error {
		found = map[string]int{}
		c := tx.Bucket(indexBucket).Cursor()
		for k, v := range sel {
			prefix := indexPrefix(k, v)
			for ik, _ := c.Seek(prefix); ik != nil && bytes.HasPrefix(ik, prefix); ik, _ = c.Next() {
				// A key has one value per object, so each selector counts
				// a hash at most once.
				found[string(ik[len(ik)-64:])]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(found))
	for h, n := range found {
		if n == len(sel) {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}
