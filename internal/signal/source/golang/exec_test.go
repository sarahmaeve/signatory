package golang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyze_ExecCommandSh_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import "os/exec"

func init() {
	_ = exec.Command("sh", "-c", "echo pwned")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.ExecCalls)
}

func TestAnalyze_ExecCommandContext_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import (
	"context"
	"os/exec"
)

func init() {
	_ = exec.CommandContext(context.Background(), "sh", "-c", "echo pwned")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.ExecCalls)
}

func TestAnalyze_AliasedExecImport_Counts(t *testing.T) {
	t.Parallel()

	// Aliased import: `import e "os/exec"` exposes os/exec as `e`.
	// callSiteOf must resolve through the import map.
	src := `package main

import e "os/exec"

func init() {
	_ = e.Command("sh", "-c", "echo pwned")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.ExecCalls)
}

func TestAnalyze_LocalCommandFunction_NotCounted(t *testing.T) {
	t.Parallel()

	// A locally-defined Command function is not os/exec.Command.
	// The catalog matches on full import path, not on leaf name.
	src := `package main

func Command(args ...string) {}

func init() {
	Command("sh", "-c", "echo nope")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.ExecCalls)
}

func TestAnalyze_StringLiteralContainingExecCommand_NotCounted(t *testing.T) {
	t.Parallel()

	src := `package main

const helpText = "Use exec.Command(name, args) to spawn a process."

func init() {
	_ = helpText
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.ExecCalls)
}

func TestAnalyze_MultipleExecCalls_AllCounted(t *testing.T) {
	t.Parallel()

	src := `package main

import "os/exec"

func init() {
	_ = exec.Command("sh", "-c", "first")
	_ = exec.Command("sh", "-c", "second")
}

func helper() {
	_ = exec.Command("sh", "-c", "third")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 3, feats.ExecCalls)
}

func TestPatternsCatalog_ExecCallSites_NoDuplicates(t *testing.T) {
	t.Parallel()

	seen := make(map[CallSite]struct{}, len(ExecCallSites))
	for _, cs := range ExecCallSites {
		_, dup := seen[cs]
		require.Falsef(t, dup, "duplicate CallSite %+v", cs)
		seen[cs] = struct{}{}
	}
	assert.NotEmpty(t, ExecCallSites)
	for _, cs := range ExecCallSites {
		assert.NotEmpty(t, cs.Pkg)
		assert.NotEmpty(t, cs.Fn)
	}
}
