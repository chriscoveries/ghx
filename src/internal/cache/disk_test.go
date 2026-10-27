//go:build !windows

package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskRestartAndInvalidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c := NewWithByteLimit(2, 100)
	if err := c.EnableDisk(path); err != nil {
		t.Fatal(err)
	}
	key := "0123456789012345678901234567890123456789012345678901234567890123"
	c.Set(&Entry{Key: key, Stdout: []byte(`{"number":21}`), CachedAt: time.Now(), TTL: time.Minute})
	d := NewWithByteLimit(2, 100)
	if err := d.EnableDisk(path); err != nil {
		t.Fatal(err)
	}
	if e := d.Get(key); e == nil || string(e.Stdout) != `{"number":21}` {
		t.Fatal("restart lost captured bytes")
	}
	d.Flush()
	next := NewWithByteLimit(2, 100)
	if err := next.EnableDisk(path); err != nil || next.Size() != 0 {
		t.Fatal("flush resurrected snapshot", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != wantSnapshotMode {
		t.Fatal("private snapshot required")
	}
}
func TestDiskFailureRemovesStaleSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c := New(2)
	if err := c.EnableDisk(path); err != nil {
		t.Fatal(err)
	}
	c.Flush()
	c.diskPath = filepath.Join(path, "cannot-create.json")
	c.Flush()
	if c.Usage().DiskErrors != 1 {
		t.Fatal("disk failure invisible")
	}
}

// Reuse the retained public repository response, with explicit invalidation
// and restart controls; the empty invalidation must fence in-flight fills too.
func TestRepoInvalidationFencesEmptyAndPersistsDeletion(t *testing.T) {
	body, err := os.ReadFile("../../../testdata/resource/repository.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache.json")
	c := New(10)
	if err := c.EnableDisk(path); err != nil {
		t.Fatal(err)
	}
	version := c.Version()
	if n := c.InvalidateRepo("github.com", "brunoborges/ghx"); n != 0 || c.Version() == version {
		t.Fatal("empty repo invalidation did not advance the fence")
	}
	key := "0123456789012345678901234567890123456789012345678901234567890123"
	entry := &Entry{Key: key, Stdout: body, Host: "github.com", Repo: "brunoborges/ghx", CachedAt: time.Now(), TTL: time.Hour}
	otherKey := "1123456789012345678901234567890123456789012345678901234567890123"
	otherBody, err := os.ReadFile("../../../testdata/api-throttle/issue140.http")
	if err != nil {
		t.Fatal(err)
	}
	other := &Entry{Key: otherKey, Stdout: otherBody, Host: "github.com", Repo: "chriscoveries/cifast", CachedAt: time.Now(), TTL: time.Hour}
	if c.SetVersion(entry, version) || c.Get(key) != nil {
		t.Fatal("empty invalidation accepted a stale fill")
	}
	c.Set(entry)
	c.Set(other)
	if c.Get(key) == nil {
		t.Fatal("fixture was not retained before invalidation")
	}
	version = c.Version()
	if n := c.InvalidateRepo("github.com", "brunoborges/ghx"); n != 1 || c.Version() == version || c.Get(key) != nil {
		t.Fatal("repo invalidation failed to remove and fence the retained entry")
	}
	reloaded := New(10)
	if err := reloaded.EnableDisk(path); err != nil {
		t.Fatal(err)
	}
	if reloaded.Peek(key) != nil || reloaded.Get(otherKey) == nil {
		t.Fatal("disk reload resurrected invalidated bytes or lost the unrelated entry")
	}
}
