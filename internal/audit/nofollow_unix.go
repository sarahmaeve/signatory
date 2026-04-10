//go:build unix

package audit

import "syscall"

// nofollowFlag is the platform-specific O_NOFOLLOW open flag. On Unix it
// is the real syscall constant, which causes open(2) to fail with ELOOP
// if the final path component is a symlink. This is the atomic,
// TOCTOU-free defense against the issue #81 attack vector — an attacker
// planting a symlink at the audit log path to redirect writes elsewhere.
//
// Intermediate-component symlinks (e.g., ~/.signatory/ itself being a
// symlink to an attacker-controlled directory) are NOT covered by
// O_NOFOLLOW; defending against those would require openat2 with
// RESOLVE_NO_SYMLINKS (Linux 5.6+) or manual Lstat walks of the parent
// path. v0.1 accepts that gap as out-of-scope.
const nofollowFlag = syscall.O_NOFOLLOW
