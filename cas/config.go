package cas

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Permission is what an API key may do in its namespace.
type Permission string

// Permissions a key can be granted.
const (
	PermRead   Permission = "READ"   // GET/HEAD objects, read labels, search
	PermCreate Permission = "CREATE" // POST objects
	PermDelete Permission = "DELETE" // DELETE objects
	PermLabel  Permission = "LABEL"  // set and delete labels
)

const minKeyLen = 16

var namespaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Config is the YAML configuration: the namespaces and the API keys.
type Config struct {
	Namespaces map[string]NamespaceConfig `yaml:"namespaces"`
	Keys       []KeyConfig                `yaml:"keys"`
}

// NamespaceConfig describes one namespace (a fully separate store).
type NamespaceConfig struct {
	// Path is the store root; empty means <STORAGE_DIR>/<namespace>.
	Path string `yaml:"path"`
}

// KeyConfig is one API key, bound to exactly one namespace.
type KeyConfig struct {
	Key         string       `yaml:"key"`
	Namespace   string       `yaml:"namespace"`
	Permissions []Permission `yaml:"permissions"`
}

// Auth resolves API keys to their namespace store and permissions. The zero
// value knows no key, so it denies every request.
type Auth struct {
	// Namespaces holds the open store of every configured namespace.
	Namespaces map[string]*Store
	keys       map[[sha256.Size]byte]*access
}

// access is what one API key grants.
type access struct {
	sum       [sha256.Size]byte
	namespace string
	store     *Store
	perms     map[Permission]bool
}

// LoadConfig reads the YAML file at path and opens its namespaces (see
// Config.Open). A missing file returns an error matching fs.ErrNotExist.
func LoadConfig(path, storageDir string, compress bool) (*Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("cas: config %s: %w", path, err)
	}
	auth, err := cfg.Open(storageDir, compress)
	if err != nil {
		return nil, fmt.Errorf("cas: config %s: %w", path, err)
	}
	return auth, nil
}

// Open validates the configuration and opens one Store per namespace,
// defaulting a namespace's path to <storageDir>/<name>.
func (c Config) Open(storageDir string, compress bool) (*Auth, error) {
	paths := map[string]string{} // cleaned absolute path -> namespace
	roots := map[string]string{} // namespace -> root
	for name, ns := range c.Namespaces {
		if !namespaceRe.MatchString(name) {
			return nil, fmt.Errorf("invalid namespace name %q", name)
		}
		p := ns.Path
		if p == "" {
			p = filepath.Join(storageDir, name)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("namespace %s: path %q: %w", name, p, err)
		}
		if other, ok := paths[abs]; ok {
			return nil, fmt.Errorf("namespaces %s and %s share the path %s", other, name, abs)
		}
		paths[abs] = name
		roots[name] = abs
	}

	auth := &Auth{Namespaces: map[string]*Store{}, keys: map[[sha256.Size]byte]*access{}}
	for i, k := range c.Keys {
		if len(k.Key) < minKeyLen {
			return nil, fmt.Errorf("key #%d: shorter than %d characters", i+1, minKeyLen)
		}
		if _, ok := roots[k.Namespace]; !ok {
			return nil, fmt.Errorf("key #%d: unknown namespace %q", i+1, k.Namespace)
		}
		if len(k.Permissions) == 0 {
			return nil, fmt.Errorf("key #%d: no permissions", i+1)
		}
		a := &access{sum: sha256.Sum256([]byte(k.Key)), namespace: k.Namespace, perms: map[Permission]bool{}}
		for _, p := range k.Permissions {
			switch p {
			case PermRead, PermCreate, PermDelete, PermLabel:
				a.perms[p] = true
			default:
				return nil, fmt.Errorf("key #%d: unknown permission %q", i+1, p)
			}
		}
		if _, dup := auth.keys[a.sum]; dup {
			return nil, fmt.Errorf("key #%d: duplicate key", i+1)
		}
		auth.keys[a.sum] = a
	}

	for name, root := range roots {
		s, err := Open(root)
		if err != nil {
			return nil, fmt.Errorf("namespace %s: %w", name, err)
		}
		s.Compress = compress
		auth.Namespaces[name] = s
	}
	for _, a := range auth.keys {
		a.store = auth.Namespaces[a.namespace]
	}
	return auth, nil
}

// KeyCount is the number of configured API keys.
func (a *Auth) KeyCount() int { return len(a.keys) }

// lookup returns what key grants, or nil for an unknown key.
func (a *Auth) lookup(key string) *access {
	sum := sha256.Sum256([]byte(key))
	acc := a.keys[sum]
	if acc == nil || subtle.ConstantTimeCompare(acc.sum[:], sum[:]) != 1 {
		return nil
	}
	return acc
}
