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
