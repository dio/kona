// Package source publishes bounded, immutable JSON documents for request-time reads.
package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"time"
)

const MaxBytes = 1 << 20

type Config struct {
	Inline   json.RawMessage `json:"inline,omitempty"`
	Filename string          `json:"filename,omitempty"`
	Remote   *Remote         `json:"remote,omitempty"`
}
type Remote struct {
	Cluster   string `json:"cluster"`
	Authority string `json:"authority"`
	Path      string `json:"path"`
}

func (c Config) Validate() error {
	n := 0
	if len(c.Inline) > 0 {
		n++
	}
	if c.Filename != "" {
		n++
	}
	if c.Remote != nil {
		n++
		if c.Remote.Cluster == "" || c.Remote.Authority == "" || len(c.Remote.Path) == 0 || c.Remote.Path[0] != '/' {
			return errors.New("remote requires cluster, authority and absolute path")
		}
	}
	if n != 1 {
		return errors.New("exactly one of inline, filename, remote is required")
	}
	return nil
}

type snapshot struct {
	values map[string]json.RawMessage
	at     time.Time
}
type Store struct{ current atomic.Pointer[snapshot] }

func (s *Store) Publish(data []byte, now time.Time) error {
	if len(data) > MaxBytes {
		return errors.New("document exceeds limit")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if values == nil {
		return errors.New("document must be an object")
	}
	s.current.Store(&snapshot{values: values, at: now})
	return nil
}

// Lookup returns an owned copy; failed refreshes never extend freshness.
func (s *Store) Lookup(key string, now time.Time, maxAge time.Duration) ([]byte, bool) {
	p := s.current.Load()
	if p == nil || (maxAge > 0 && now.Sub(p.at) > maxAge) {
		return nil, false
	}
	v, ok := p.values[key]
	return bytes.Clone(v), ok
}

// ReadFile opens the path on every refresh, including Kubernetes projected-volume swaps.
func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBytes {
		return nil, errors.New("document exceeds limit")
	}
	return b, nil
}
