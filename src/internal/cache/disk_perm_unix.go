//go:build !windows

package cache

import "io/fs"

// privateModeOK requires owner-only permissions on unix so snapshots and
// state stay private to the account that wrote them.
func privateModeOK(m fs.FileMode) bool {
	return m.Perm()&0077 == 0
}

// wantSnapshotMode is the mode persisted snapshots are expected to carry.
const wantSnapshotMode = 0600
