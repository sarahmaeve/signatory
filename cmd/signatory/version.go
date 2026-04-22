package main

import (
	"fmt"
	"runtime"
)

// VersionCmd prints version, commit, build timestamp, and Go runtime
// version. All four come from package-level vars (version, commit,
// buildDate) stamped at install time by the Makefile's ldflags, plus
// runtime.Version() which is always accurate.
//
// The multi-line KV format makes drift spottable at a glance: a user
// or agent seeing `built: 2026-04-21T12:05:00Z` when the source is at
// 17:17 can immediately diagnose a stale binary without having to
// cross-reference git.
type VersionCmd struct{}

func (cmd *VersionCmd) Run(globals *Globals) error {
	fmt.Printf("signatory %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", buildDate)
	fmt.Printf("  go:     %s\n", runtime.Version())
	return nil
}
