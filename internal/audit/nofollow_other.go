//go:build !unix

package audit

// nofollowFlag is 0 on non-Unix builds because Go's syscall package does
// not expose O_NOFOLLOW on those platforms. The defensive Lstat check in
// appendFile still rejects symlinks on these platforms — it has a TOCTOU
// window (a symlink could be created between Lstat and OpenFile) but is
// better than no protection at all. On Unix the same Lstat check runs as
// belt-and-suspenders alongside the atomic O_NOFOLLOW flag.
const nofollowFlag = 0
