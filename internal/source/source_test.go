package source

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFreshnessAndOwnership(t *testing.T) {
	var s Store
	now := time.Now()
	if err := s.Publish([]byte(`{"demo":{"v":1}}`), now); err != nil {
		t.Fatal(err)
	}
	b, ok := s.Lookup("demo", now, time.Second)
	if !ok {
		t.Fatal("missing")
	}
	b[0] = 'x'
	if b, _ := s.Lookup("demo", now, time.Second); string(b) != `{"v":1}` {
		t.Fatal("mutable snapshot")
	}
	if err := s.Publish([]byte(`broken`), now.Add(time.Second)); err == nil {
		t.Fatal("accepted malformed document")
	}
	if _, ok := s.Lookup("demo", now.Add(2*time.Second), time.Second); ok {
		t.Fatal("failed refresh extended freshness")
	}
	if _, ok := s.Lookup("other", now, 0); ok {
		t.Fatal("missing key accepted")
	}
}
func TestFileReplacement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(p, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".new", []byte(`{"a":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".new", p); err != nil {
		t.Fatal(err)
	}
	b, err := ReadFile(p)
	if err != nil || string(b) != `{"a":2}` {
		t.Fatalf("replacement: %v", err)
	}
}
func TestExclusiveAndBounded(t *testing.T) {
	for _, c := range []Config{{}, {Filename: "x", Inline: []byte(`{}`)}, {Remote: &Remote{}}} {
		if c.Validate() == nil {
			t.Fatal("accepted invalid source")
		}
	}
	var s Store
	if s.Publish(make([]byte, MaxBytes+1), time.Now()) == nil {
		t.Fatal("accepted oversized data")
	}
}
