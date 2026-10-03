//go:build windows

package cache

import "io/fs"

// privateModeOK is always true on Windows: NTFS reports ACL-derived mode
// bits (readable as 0666/0444) so unix owner-only bits cannot be checked.
// Privacy is enforced by the per-user state directory's ACL.
func privateModeOK(fs.FileMode) bool {
	return true
}

// wantSnapshotMode matches what Go reports for a writable file on NTFS.
const wantSnapshotMode = 0666
