// Tests for the four "compound" probes that read multiple seams
// at once: node-extra-ca-certs (delegates to certs.Check),
// mcp-binary-matches (.mcp.json + executable + stat),
// signatory-db (open + .mcp.json env consistency), and
// pipeline-service (port probe).
package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/certs"
)

// ---- node-extra-ca-certs ---------------------------------------------------

func TestProbeNodeExtraCACerts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		check      certs.CheckResult
		wantStatus Status
		wantInMsg  string
	}{
		{
			name:       "ok",
			check:      certs.CheckResult{OK: true, Message: "NODE_EXTRA_CA_CERTS=/foo (valid)"},
			wantStatus: StatusOK,
			wantInMsg:  "NODE_EXTRA_CA_CERTS",
		},
		{
			name:       "fail with fix",
			check:      certs.CheckResult{OK: false, Message: "not set", Fix: "run `signatory certs init`"},
			wantStatus: StatusFail,
			wantInMsg:  "not set",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				CertsCheck: func() certs.CheckResult { return tc.check },
			})
			got := probeNodeExtraCACerts(r)

			if got.Name != "node-extra-ca-certs" {
				t.Errorf("Name = %q, want node-extra-ca-certs", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("Message %q does not contain %q", got.Message, tc.wantInMsg)
			}
			if tc.wantStatus == StatusFail && got.Fix == "" {
				t.Errorf("fail without Fix: %+v", got)
			}
		})
	}
}

// ---- mcp-binary-matches ----------------------------------------------------

// mcpFixture builds a minimal valid .mcp.json + the supporting
// seams so each test case can override one piece (the command,
// the running executable, or the stat result) without re-stating
// the whole environment.
type mcpFixture struct {
	cwd          string
	mcpJSON      string
	mcpJSONErr   error
	homeEnv      string
	executable   string
	executableEr error
	stat         func(string) (os.FileInfo, error)
}

func (f mcpFixture) opts() Options {
	cwd := f.cwd
	if cwd == "" {
		cwd = "/work/proj"
	}
	return Options{
		Getwd: func() (string, error) { return cwd, nil },
		ReadFile: func(p string) ([]byte, error) {
			if p != filepath.Join(cwd, ".mcp.json") {
				return nil, os.ErrNotExist
			}
			if f.mcpJSONErr != nil {
				return nil, f.mcpJSONErr
			}
			return []byte(f.mcpJSON), nil
		},
		Getenv: func(k string) string {
			if k == "HOME" {
				if f.homeEnv != "" {
					return f.homeEnv
				}
				return "/home/test"
			}
			return ""
		},
		Executable: func() (string, error) {
			if f.executableEr != nil {
				return "", f.executableEr
			}
			return f.executable, nil
		},
		Stat: f.stat,
	}
}

func TestProbeMCPBinaryMatches(t *testing.T) {
	t.Parallel()

	const realBinary = "/home/test/go/bin/signatory"

	t.Run("matches", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"${HOME}/go/bin/signatory","args":["mcp"]}}}`,
			executable: realBinary,
			stat:       statTable(map[string]fakeFileInfo{realBinary: {name: "signatory", dir: false}}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusOK {
			t.Fatalf("Status = %q, want ok (msg=%q)", got.Status, got.Message)
		}
	})

	t.Run("declared path missing", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"${HOME}/go/bin/signatory"}}}`,
			executable: "/usr/local/bin/signatory",
			stat:       statTable(map[string]fakeFileInfo{}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusFail {
			t.Fatalf("Status = %q, want fail", got.Status)
		}
		if !strings.Contains(got.Message, "does not exist") {
			t.Errorf("Message %q lacks 'does not exist'", got.Message)
		}
	})

	t.Run("declared path differs from running binary", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"${HOME}/go/bin/signatory"}}}`,
			executable: "/opt/homebrew/bin/signatory",
			stat:       statTable(map[string]fakeFileInfo{realBinary: {name: "signatory"}}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusFail {
			t.Fatalf("Status = %q, want fail (msg=%q)", got.Status, got.Message)
		}
		if !strings.Contains(got.Message, realBinary) || !strings.Contains(got.Message, "/opt/homebrew/bin/signatory") {
			t.Errorf("Message should name both paths: %q", got.Message)
		}
	})

	t.Run("no mcp.json present", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSONErr: os.ErrNotExist,
			executable: "/anywhere/signatory",
			stat:       statTable(map[string]fakeFileInfo{}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusWarn {
			t.Fatalf("Status = %q, want warn (msg=%q)", got.Status, got.Message)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSON:    `{not valid json`,
			executable: "/anywhere/signatory",
			stat:       statTable(map[string]fakeFileInfo{}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusFail {
			t.Fatalf("Status = %q, want fail", got.Status)
		}
	})

	t.Run("missing signatory entry", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSON:    `{"mcpServers":{"other":{"command":"/x"}}}`,
			executable: "/anywhere/signatory",
			stat:       statTable(map[string]fakeFileInfo{}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusFail {
			t.Fatalf("Status = %q, want fail", got.Status)
		}
	})

	t.Run("empty command", func(t *testing.T) {
		t.Parallel()
		fx := mcpFixture{
			mcpJSON:    `{"mcpServers":{"signatory":{"command":""}}}`,
			executable: "/anywhere/signatory",
			stat:       statTable(map[string]fakeFileInfo{}),
		}
		got := probeMCPBinaryMatches(resolveOptions(fx.opts()))
		if got.Status != StatusFail {
			t.Fatalf("Status = %q, want fail", got.Status)
		}
	})
}

// ---- signatory-db ----------------------------------------------------------

func TestProbeSignatoryDB_OpenStore(t *testing.T) {
	t.Parallel()

	t.Run("opens cleanly", func(t *testing.T) {
		t.Parallel()
		opts := Options{
			DBPath:    "/tmp/sig.db",
			OpenStore: func(_ context.Context, _ string) error { return nil },
			Getwd:     func() (string, error) { return "/work", nil },
			ReadFile:  func(_ string) ([]byte, error) { return nil, os.ErrNotExist },
		}
		got := probeSignatoryDB(resolveOptions(opts))
		if got.Status != StatusOK {
			t.Fatalf("Status = %q, want ok (msg=%q)", got.Status, got.Message)
		}
		if !strings.Contains(got.Message, "/tmp/sig.db") {
			t.Errorf("Message should name DB path: %q", got.Message)
		}
	})

	t.Run("open fails", func(t *testing.T) {
		t.Parallel()
		opts := Options{
			DBPath:    "/tmp/sig.db",
			OpenStore: func(_ context.Context, _ string) error { return errStub },
			Getwd:     func() (string, error) { return "/work", nil },
			ReadFile:  func(_ string) ([]byte, error) { return nil, os.ErrNotExist },
		}
		got := probeSignatoryDB(resolveOptions(opts))
		if got.Status != StatusFail {
			t.Fatalf("Status = %q, want fail (msg=%q)", got.Status, got.Message)
		}
		if got.Fix == "" {
			t.Errorf("fail without Fix: %+v", got)
		}
	})

	t.Run("nil seam skips open check", func(t *testing.T) {
		t.Parallel()
		opts := Options{
			DBPath:   "/tmp/sig.db",
			Getwd:    func() (string, error) { return "/work", nil },
			ReadFile: func(_ string) ([]byte, error) { return nil, os.ErrNotExist },
		}
		got := probeSignatoryDB(resolveOptions(opts))
		if got.Status != StatusOK {
			t.Fatalf("Status = %q, want ok (seam-skipped path); msg=%q", got.Status, got.Message)
		}
	})
}

func TestProbeSignatoryDB_EnvDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shellEnv   map[string]string
		mcpJSON    string
		wantStatus Status
		wantInMsg  string
	}{
		{
			name:       "both unset",
			shellEnv:   map[string]string{},
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"/x","env":{}}}}`,
			wantStatus: StatusOK,
			wantInMsg:  "no SIGNATORY_DB pin",
		},
		{
			name:       "consistent",
			shellEnv:   map[string]string{"SIGNATORY_DB": "/db/x.db"},
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"/x","env":{"SIGNATORY_DB":"/db/x.db"}}}}`,
			wantStatus: StatusOK,
			wantInMsg:  "consistent",
		},
		{
			name:       "shell-only",
			shellEnv:   map[string]string{"SIGNATORY_DB": "/db/x.db"},
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"/x","env":{}}}}`,
			wantStatus: StatusWarn,
			wantInMsg:  "drift",
		},
		{
			name:       "mcp-only",
			shellEnv:   map[string]string{},
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"/x","env":{"SIGNATORY_DB":"/db/x.db"}}}}`,
			wantStatus: StatusWarn,
			wantInMsg:  "drift",
		},
		{
			name:       "different paths",
			shellEnv:   map[string]string{"SIGNATORY_DB": "/db/a.db"},
			mcpJSON:    `{"mcpServers":{"signatory":{"command":"/x","env":{"SIGNATORY_DB":"/db/b.db"}}}}`,
			wantStatus: StatusWarn,
			wantInMsg:  "drift",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cwd := "/work"
			opts := Options{
				DBPath:    "/db/x.db",
				OpenStore: func(_ context.Context, _ string) error { return nil },
				Getenv: func(k string) string {
					if v, ok := tc.shellEnv[k]; ok {
						return v
					}
					return ""
				},
				Getwd: func() (string, error) { return cwd, nil },
				ReadFile: func(p string) ([]byte, error) {
					if p == filepath.Join(cwd, ".mcp.json") {
						return []byte(tc.mcpJSON), nil
					}
					return nil, os.ErrNotExist
				},
			}
			got := probeSignatoryDB(resolveOptions(opts))
			if got.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (msg=%q)", got.Status, tc.wantStatus, got.Message)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("Message %q lacks %q", got.Message, tc.wantInMsg)
			}
			if got.Status == StatusWarn && got.Fix == "" {
				t.Errorf("warn without Fix: %+v", got)
			}
		})
	}
}

// ---- pipeline-service ------------------------------------------------------

func TestProbePipelineService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		listening bool
		wantInMsg string
	}{
		{name: "listening", listening: true, wantInMsg: "listening on 127.0.0.1:21517"},
		{name: "not running", listening: false, wantInMsg: "not running"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := Options{
				ProbePort: func(_ int, _ time.Duration) bool { return tc.listening },
			}
			got := probePipelineService(resolveOptions(opts))
			// Always StatusOK by design (service is on-demand).
			if got.Status != StatusOK {
				t.Errorf("Status = %q, want ok (pipeline-service is informational)", got.Status)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("Message %q lacks %q", got.Message, tc.wantInMsg)
			}
		})
	}
}

// Smoke that defaults wire the seams correctly. We don't probe for
// real OS state — just verify resolveOptions doesn't leave any nil
// pointer that production callers would deref.
func TestResolveOptions_DefaultsArePopulated(t *testing.T) {
	t.Parallel()
	r := resolveOptions(Options{})
	if r.getenv == nil || r.lookPath == nil || r.goVersion == nil ||
		r.stat == nil || r.userHomeDir == nil || r.getwd == nil ||
		r.readFile == nil || r.executable == nil || r.probePort == nil ||
		r.certsCheck == nil {
		t.Fatalf("resolveOptions left a nil seam: %+v", r)
	}
	// openStore is intentionally nil-default; the probe handles
	// that branch.
	if r.openStore != nil {
		t.Errorf("openStore should default to nil, got non-nil")
	}
	if r.pipelinePort != 21517 {
		t.Errorf("pipelinePort default = %d, want 21517", r.pipelinePort)
	}
}

// errSentinel asserts errStub is wired through testing as expected,
// guarding against silent rename of the package-level sentinel.
func TestErrStub_IsRealError(t *testing.T) {
	t.Parallel()
	if !errors.Is(errStub, errStub) {
		t.Fatal("errStub does not satisfy errors.Is on itself — sentinel broken")
	}
}
