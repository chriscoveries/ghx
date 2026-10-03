//go:build windows

package cache

import "fmt"

// Fleet snapshots require Unix ownership and private file permissions.
func (c *Cache) EnableDisk(path string) error {
	return fmt.Errorf("cache_file requires Unix private file permissions")
}
func (c *Cache) persistLocked() {}
