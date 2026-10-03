//go:build !windows

package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// EnableDisk loads a private, bounded snapshot after the daemon holds its socket
// lock. Only response bytes and hashed keys are saved; never auth environments.
func (c *Cache) EnableDisk(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cache_file must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	entries := []*Entry{}
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		info, e := f.Stat()
		limit := c.maxBytes*2 + int64(c.maxSize)*8192 + 1024
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > limit {
			return fmt.Errorf("unsafe or oversized disk cache")
		}
		if err = json.NewDecoder(io.LimitReader(f, limit+1)).Decode(&entries); err != nil {
			return fmt.Errorf("invalid disk cache: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if len(entries) > c.maxSize {
		entries = entries[:c.maxSize]
	}
	// Snapshot order is MRU first. Preserve retained expired entries for ETags
	// and immutable projections, bounded to one day across process restarts.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e != nil && len(e.Key) == 64 && time.Since(e.CachedAt) < 24*time.Hour {
			c.set(e)
		}
	}
	c.diskPath = path
	return nil
}
func (c *Cache) persistLocked() {
	if c.diskPath == "" {
		return
	}
	entries := make([]*Entry, 0, c.order.Len())
	for e := c.order.Front(); e != nil; e = e.Next() {
		entries = append(entries, e.Value.(*storedEntry).entry)
	}
	f, err := os.CreateTemp(filepath.Dir(c.diskPath), ".cache-*")
	if err != nil {
		c.diskErrors++
		return
	}
	name := f.Name()
	defer os.Remove(name)
	err = json.NewEncoder(f).Encode(entries)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, c.diskPath)
	}
	if err != nil {
		c.diskErrors++
		os.Remove(c.diskPath)
	}
}
