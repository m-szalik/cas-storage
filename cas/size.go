package cas

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultMaxUploadSize applies when neither the config nor the namespace sets
// max_upload_size.
const DefaultMaxUploadSize ByteSize = 500 * 1000 * 1000

// ByteSize is a size in bytes, written in YAML as a number of bytes or with a
// unit: B, KB, MB, GB, TB (powers of 1000) or KiB, MiB, GiB, TiB (powers of
// 1024), e.g. "500MB". Zero means no limit.
type ByteSize int64

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"B", 1},
}

// ParseByteSize parses a size such as "500MB", "2GiB" or "1024".
func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	num, mult := s, int64(1)
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			num, mult = strings.TrimSpace(n), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || (n > 0 && mult > (1<<63-1)/n) {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return ByteSize(n * mult), nil
}

// UnmarshalYAML accepts a plain number of bytes or a size with a unit.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}
