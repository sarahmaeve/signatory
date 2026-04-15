package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/config"
	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/profile"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
)

// HandoffCmd renders a handoff prompt by loading a template from the
// resolver-ordered search path and substituting the caller's
// `{PLACEHOLDER}` values. Output defaults to stdout; pass --output to
// write a file.
//
// Typical invocations:
//
//	signatory handoff security https://github.com/nvbn/thefuck
//	signatory handoff provenance /Users/me/code/thefuck --ecosystem=pypi
//	signatory handoff security ./atuin --language=go
//
// The <role> positional picks between the shipped roles (security,
// provenance). For security, --language=python|go picks which
// pattern-catalog variant to use. --template overrides the inference
// for advanced callers who maintain their own template variants.
//
// All `{PLACEHOLDER}` tokens not filled by flags are left literal in
// the output; the command reports which ones remained on stderr so
// the user can fix them before pasting into an agent prompt. The
// exception is TARGET_NAME — if it can't be inferred from the target
// and the user didn't pass --name, the command errors rather than
// emit a broken handoff.
//
// Network behavior: this command is offline. Auto-detecting language
// or ecosystem from a remote repo would require network access and
// isn't implemented yet — pass --language and --ecosystem explicitly.
// A future --network-precheck flag will probe remote registries.
type HandoffCmd struct {
	Role   string `arg:"" enum:"security,provenance" help:"Analyst role: security or provenance."`
	Target string `arg:"" help:"Target repository URL or local path (e.g., https://github.com/foo/bar or /Users/me/code/foo)."`

	Name       string `help:"Override TARGET_NAME (default: inferred from target)."`
	URL        string `help:"Override TARGET_URL (default: target when target is a URL)."`
	Path       string `help:"Override TARGET_PATH (default: target when target is a local path)."`
	TargetRole string `name:"target-role" default:"" help:"Dependency role for TARGET_ROLE (runtime|validation|build-only|development)." enum:"runtime,validation,build-only,development,"`
	Ecosystem  string `default:"" help:"ECOSYSTEM value for provenance role (pypi|npm|crates|go). Auto-detected with --network-precheck." enum:"pypi,npm,crates,go,"`
	// Language default is "" — kong would otherwise make it
	// impossible to distinguish "user passed --language=python" from
	// "user omitted it and kong filled in the default", and the
	// --network-precheck path needs that distinction to know whether
	// to apply a detected language. Resolved to "python" in Run if
	// still empty after precheck.
	Language string `help:"Language flavor for security role (python|go). Auto-detected with --network-precheck; falls back to python." default:"" enum:"python,go,"`
	Intake   string `help:"INTAKE_QUESTION body; the user's specific question for this engagement."`
	Template string `help:"Explicit template name (e.g., handoffs/security-review-v1.md). Bypasses --role/--language inference."`

	TemplateDir  []string `name:"template-dir" help:"Additional template search directory (repeatable, highest priority)."`
	FilestoreDir []string `name:"filestore-dir" help:"Additional filestore output directory (repeatable). Unused unless --output is a bare filename."`
	ConfigFile   string   `name:"config" help:"Path to signatory.config.toml. If unset, discovered from --project-dir." type:"existingfile"`
	ProjectDir   string   `name:"project-dir" help:"Project root used to locate ./templates/, ./filestore/, and signatory.config.toml." default:"." type:"path"`

	NetworkPrecheck bool   `name:"network-precheck" help:"Fill unset --language and --ecosystem by calling the GitHub API (requires a github.com target). Offline by default; this is the opt-in that authorizes network calls."`
	CloneDir        string `name:"clone-dir" help:"Shallow-clone the target URL into CLONE_DIR/<repo-name>/ and use that path for TARGET_PATH. Uses 'git clone --depth=1'. Skipped if the destination already exists. Requires target to be a URL." type:"path"`

	Output string `short:"o" help:"Write rendered handoff to this file instead of stdout."`
	Force  bool   `help:"Overwrite --output if it exists."`
	Quiet  bool   `help:"Suppress the stderr report (template source, unfilled placeholders)." short:"q"`
}

// Run executes the handoff render. Errors from template resolution,
// substitution, or write are surfaced directly; the stderr report
// (template source, unfilled placeholders, embedded-fallback notice)
// is informational and goes out independently of Success/failure.
func (cmd *HandoffCmd) Run(globals *Globals) error {
	// Network precheck runs early: it may fill --language and
	// --ecosystem, which both influence later steps (template name
	// inference, provenance-role validation).
	var precheckReport string
	if cmd.NetworkPrecheck {
		report, err := cmd.applyNetworkPrecheck(context.Background())
		if err != nil {
			return fmt.Errorf("network-precheck: %w", err)
		}
		precheckReport = report
	}

	// Clone step: shallow-clone the target URL if --clone-dir was passed.
	// Runs AFTER precheck (precheck may confirm the target is a GitHub URL)
	// but BEFORE template resolution (so TARGET_PATH is available).
	var cloneReport string
	if cmd.CloneDir != "" {
		clonedPath, report, err := cmd.applyClone(context.Background())
		if err != nil {
			return fmt.Errorf("clone-dir: %w", err)
		}
		cloneReport = report
		// --path wins if the user set it explicitly; --clone-dir is
		// the "auto-fill" path. We check cmd.Path here — kong leaves
		// it empty when the user didn't pass the flag.
		if cmd.Path == "" {
			cmd.Path = clonedPath
		} else if !cmd.Quiet {
			// User passed both flags; note the override. Gated by
			// --quiet because that flag's contract is "no stderr
			// output" for automation callers who've made an
			// intentional choice.
			fmt.Fprintf(os.Stderr, "# clone-dir: cloned to %s but --path=%s wins\n", clonedPath, cmd.Path)
		}
	}

	// Language default: "python" unless auto-detected otherwise.
	// Done AFTER network-precheck so detection can set "go" first.
	if cmd.Language == "" {
		cmd.Language = "python"
	}

	resolver, err := cmd.buildResolver()
	if err != nil {
		return err
	}

	templateName := cmd.Template
	if templateName == "" {
		templateName = inferTemplateName(cmd.Role, cmd.Language)
	}

	rc, source, embedded, err := resolver.OpenTemplate(templateName)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read template %s: %w", source, err)
	}

	subs, err := config.HandoffSubstitutions(cmd.Target, config.HandoffOverrides{
		Name:      cmd.Name,
		URL:       cmd.URL,
		Path:      cmd.Path,
		Role:      cmd.TargetRole,
		Ecosystem: cmd.Ecosystem,
		Intake:    cmd.Intake,
	})
	if err != nil {
		return err
	}

	// Surface ecosystem-required roles before render time so the user
	// doesn't silently get a handoff with `{ECOSYSTEM}` literal.
	if cmd.Role == "provenance" && subs["ECOSYSTEM"] == "" {
		return fmt.Errorf("provenance role requires --ecosystem (one of: pypi, npm, crates, go)")
	}

	rendered, unfilled := config.RenderTemplate(raw, subs)

	if err := writeHandoff(cmd.Output, cmd.Force, rendered); err != nil {
		return err
	}

	if !cmd.Quiet {
		reportToStderr(source, embedded, unfilled)
		if precheckReport != "" {
			fmt.Fprint(os.Stderr, precheckReport)
		}
		if cloneReport != "" {
			fmt.Fprint(os.Stderr, cloneReport)
		}
	}
	return nil
}

// applyNetworkPrecheck resolves the target to a GitHub owner/name,
// calls the ecosystem detector, and fills cmd.Language / cmd.Ecosystem
// for any field the user left empty. Returns a short stderr-report
// string describing what was detected (or "" when nothing is worth
// reporting, e.g. the user had already set everything).
//
// Behavior:
//   - Target must be a github.com URL (scheme + host checked against
//     whitelisted forms). Other hosts error immediately — we have no
//     detection code for them yet.
//   - Token comes from GITHUB_TOKEN if set; unauthenticated otherwise
//     (60/hour quota, but two calls per run fits comfortably).
//   - A detected ecosystem like "go" overrides an empty --ecosystem;
//     it never clobbers a user-provided value.
//   - A detected language of "Go" maps to --language=go; anything
//     else leaves --language empty so the later default ("python")
//     takes effect. We'll extend this as we add more language
//     variants of the security template.
func (cmd *HandoffCmd) applyNetworkPrecheck(ctx context.Context) (string, error) {
	if !looksLikeGitHubURL(cmd.Target) {
		return "", fmt.Errorf("target %q is not a github.com URL; only GitHub is supported for --network-precheck in v1", cmd.Target)
	}
	_, owner, name, err := profile.NormalizeGitHubRepoInput(cmd.Target)
	if err != nil {
		return "", fmt.Errorf("parse target: %w", err)
	}

	detector := ecosystem.NewDetector(newPrecheckSource(os.Getenv("GITHUB_TOKEN")))

	result, err := detector.Detect(ctx, owner, name)
	if err != nil {
		return "", err
	}

	// Ecosystem: fill only if user didn't pass --ecosystem.
	ecoApplied := ""
	if cmd.Ecosystem == "" && result.Primary != ecosystem.EcosystemUnknown {
		cmd.Ecosystem = string(result.Primary)
		ecoApplied = string(result.Primary)
	}

	// Language flavor: fill only if user didn't pass --language.
	// We currently only ship two flavors (python, go); anything
	// else leaves --language empty to fall through to "python."
	langApplied := ""
	if cmd.Language == "" && languageToFlavor(result.Language) == "go" {
		cmd.Language = "go"
		langApplied = "go"
	}

	return formatPrecheckReport(owner, name, result, ecoApplied, langApplied), nil
}

// runGitClone is the function applyClone uses to execute `git clone
// --depth=1 <url> <dest>`. It is a package-level var — rather than
// inlined in applyClone — so tests can swap it for a fake that
// records the arguments without actually spawning git. This mirrors
// the newPrecheckSource seam pattern used for the network precheck.
//
// The production implementation creates an exec.CommandContext so a
// cancelled context propagates to the git subprocess.
var runGitClone = func(ctx context.Context, url, dest string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", url, dest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applyClone shallow-clones the target URL into CloneDir/<repo-name>/
// and returns (clonedPath, stderrReport, error). It is analogous to
// applyNetworkPrecheck: it encapsulates one pre-render side-effect and
// returns a human-readable report string the caller emits to stderr.
//
// Invariants enforced here:
//   - Target must classify as TargetURL (ClassifyTarget).
//   - CloneDir parent must exist and be writable.
//   - Derived dest must be strictly inside CloneDir (no symlink/.. escapes).
//   - If dest already exists as a directory, reuse it without cloning.
//   - Clone uses a 2-minute context timeout; a cancelled ctx propagates.
func (cmd *HandoffCmd) applyClone(ctx context.Context) (clonedPath, report string, err error) {
	// --clone-dir only makes sense when the target is a URL. If it's a
	// local path the classification already filled TARGET_PATH, and the
	// flag is documented as a no-op for local paths — but we still guard
	// explicitly so a confused invocation fails loudly rather than silently
	// doing nothing.
	if config.ClassifyTarget(cmd.Target) != config.TargetURL {
		return "", "", fmt.Errorf("--clone-dir requires a URL target; %q is not a URL", cmd.Target)
	}

	// Validate the clone URL before doing anything else. This rejects
	// query strings (?upload-pack=evil), fragments, embedded credentials,
	// and null bytes — all forms that can be misinterpreted by git.
	if err := safeGitCloneURL(cmd.Target); err != nil {
		return "", "", fmt.Errorf("unsafe clone URL: %w", err)
	}

	repoName := config.InferNameFromURL(cmd.Target)
	// Validate the inferred name before using it as a directory component.
	// InferNameFromURL uses url.Parse which percent-decodes the path, so a
	// URL with %00 in the repo segment would produce a null byte in the name.
	if err := safeCloneRepoName(repoName); err != nil {
		return "", "", fmt.Errorf("cannot derive safe clone directory name from target %q: %w", cmd.Target, err)
	}

	// Resolve the parent dir to an absolute, symlink-free path before the
	// containment check. filepath.Abs handles relative paths but does NOT
	// resolve symlinks; filepath.EvalSymlinks does. Using the resolved path
	// prevents an attacker-controlled symlink at clone-dir from redirecting
	// the parent to an arbitrary location.
	absParent, err := filepath.Abs(cmd.CloneDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve clone-dir %q: %w", cmd.CloneDir, err)
	}

	// Writability check: stat the parent and verify it is an existing,
	// writable directory. We probe with a temp file rather than relying
	// on the mode bits because ACLs and mount options can override them.
	info, err := os.Stat(absParent)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("clone-dir parent %q does not exist", absParent)
		}
		return "", "", fmt.Errorf("stat clone-dir parent %q: %w", absParent, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("clone-dir %q is not a directory", absParent)
	}
	// Probe writability with a temp file rather than inspecting mode
	// bits — ACLs and mount options can make a 0755 dir unwritable.
	probe, err := os.CreateTemp(absParent, ".signatory-clone-probe-*")
	if err != nil {
		return "", "", fmt.Errorf("clone-dir %q is not writable: %w", absParent, err)
	}
	probe.Close()
	os.Remove(probe.Name())

	// Resolve all symlinks in the parent so the containment check below
	// compares real filesystem paths. Without this, a symlink at
	// --clone-dir itself could redirect the parent to an arbitrary path
	// and cause the containment check to compare unrelated absolute paths.
	parent, err := filepath.EvalSymlinks(absParent)
	if err != nil {
		return "", "", fmt.Errorf("resolve symlinks in clone-dir %q: %w", absParent, err)
	}

	// Build dest and verify it is strictly under parent (belt-and-suspenders
	// against any edge case where the inferred repo name contains unexpected
	// path components). We do NOT call EvalSymlinks on dest here — it likely
	// doesn't exist yet. The name-validation above (safeCloneRepoName) already
	// rejected names containing path separators, "..", or null bytes; the Rel
	// check below catches any remaining escapes.
	dest := filepath.Join(parent, repoName)
	destClean := filepath.Clean(dest)
	parentClean := filepath.Clean(parent)
	// filepath.Rel returns an error or a path starting with ".." when dest
	// is outside parent.
	rel, err := filepath.Rel(parentClean, destClean)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", "", fmt.Errorf("derived clone path %q escapes clone-dir %q; refusing to clone", destClean, parentClean)
	}

	// Skip-if-exists: reuse the directory without pulling or fetching.
	// Intentional design: the analyst may have frozen state, applied
	// patches, or be on a slow network. Silently updating would be
	// surprising and potentially unsafe (new commits since last analysis
	// would silently change the reviewed surface).
	//
	// The reuse note is returned through the report channel (gated by
	// --quiet upstream), not written directly to stderr — consistent
	// with the rest of the precheck/clone reporting.
	if fi, err := os.Stat(destClean); err == nil && fi.IsDir() {
		return destClean, fmt.Sprintf("# clone: %s already exists, reusing\n", destClean), nil
	}

	// Shallow clone with a 2-minute timeout. Use a child context so the
	// timeout doesn't bleed into the rest of the Run() pipeline.
	cloneCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := runGitClone(cloneCtx, cmd.Target, destClean); err != nil {
		return "", "", err
	}

	report = fmt.Sprintf("# clone: %s → %s\n", cmd.Target, destClean)
	return destClean, report, nil
}

// newPrecheckSource is the factory applyNetworkPrecheck uses to
// construct an ecosystem.Source backed by GitHub. Exposed as a
// package-level var specifically so tests can swap it out for a
// fake — the real ghclient.Client has an unexported baseURL and
// cannot otherwise be pointed at an httptest.Server from this
// package.
//
// The production implementation wraps ghclient.NewClient(token).
// Tests assign their own function here (and restore it via
// t.Cleanup) to exercise the end-to-end precheck pipeline without
// hitting the live GitHub API.
var newPrecheckSource = func(token string) ecosystem.Source {
	return ghclient.NewClient(token)
}

// looksLikeGitHubURL returns true when the target string starts with
// an http(s) scheme whose host is github.com. Bare "owner/repo"
// inputs are rejected here because the classifier in
// internal/config treats them as TargetUnknown — we want to force
// URL-form explicitness when the user opts into network calls.
func looksLikeGitHubURL(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "https://github.com/") ||
		strings.HasPrefix(lower, "http://github.com/")
}

// safeGitCloneURL validates that raw is safe to pass as a `git clone` URL
// argument. It parses the URL and rejects any form that could be
// misinterpreted by git or that carries unexpected data:
//
//   - Query strings (?upload-pack=evil) — git's URL parser may interpret
//     query-encoded protocol options differently from the scheme, leading
//     to unexpected behavior or remote code execution via git helpers.
//   - URL fragments (#...) — meaningless to git and a sign of injection.
//   - Userinfo (@user:pass) — credentials should never be embedded in
//     URLs passed to git; they belong in netrc or the credential store.
//   - Null bytes in any component — always a path-injection signal.
//
// safeGitCloneURL is a belt-and-suspenders check: it does NOT replace
// looksLikeGitHubURL or the ClassifyTarget guard; it fires on the same
// URL that will reach `git clone` as its argv[1].
func safeGitCloneURL(raw string) error {
	if strings.ContainsRune(raw, 0) {
		return fmt.Errorf("clone URL contains a null byte")
	}
	// Check for query string and fragment on the raw string before parsing.
	// url.Parse sets RawQuery="" for a bare "?" suffix and Fragment="" for a
	// bare "#" suffix, so checking parsed fields misses those forms.
	// Checking the raw string catches both.
	if strings.ContainsRune(raw, '?') {
		return fmt.Errorf("clone URL must not contain a query string; pass a bare repo URL")
	}
	if strings.ContainsRune(raw, '#') {
		return fmt.Errorf("clone URL must not contain a fragment (#...); pass a bare repo URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("clone URL is not parseable: %w", err)
	}
	if u.User != nil {
		return fmt.Errorf("clone URL must not embed credentials; use git's credential store instead")
	}
	return nil
}

// safeCloneRepoName validates that name — derived from the URL by
// InferNameFromURL — is safe to use as a single directory component
// under the clone parent. It rejects:
//
//   - Empty strings (InferNameFromURL couldn't derive a name).
//   - Names containing path separators (would escape the parent dir).
//   - Names that are "." or ".." (classic traversal).
//   - Names containing null bytes (path-injection on most OSes).
//
// This is a second line of defense behind InferNameFromURL, which uses
// url.Parse and takes the last path segment. InferNameFromURL can still
// produce unsafe names from percent-encoded bytes in crafted URLs.
func safeCloneRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("inferred repo name is empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("inferred repo name %q is a reserved path component", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("inferred repo name %q contains a null byte", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("inferred repo name %q contains a path separator", name)
	}
	return nil
}

// languageToFlavor maps GitHub's primary-language string to the
// language flavor slug we use in template filenames. Unknown
// languages return "" so the caller can fall back to the default.
//
// Kept as a small switch rather than a map so the default case is
// explicit: adding a new template flavor is a one-line change here
// plus the corresponding handoffs/security-review-<flavor>-v1.md file.
func languageToFlavor(primaryLanguage string) string {
	switch strings.ToLower(primaryLanguage) {
	case "go":
		return "go"
	case "python":
		return "python"
	default:
		return ""
	}
}

// formatPrecheckReport renders the "what the network call found"
// block that lands on stderr. It's informational — it tells the user
// what was detected and which flags it filled in. When nothing was
// applied (e.g., user passed --ecosystem and --language explicitly)
// the report still shows the detection result for transparency.
func formatPrecheckReport(owner, name string, result *ecosystem.DetectionResult, ecoApplied, langApplied string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# precheck(%s/%s): ", owner, name)
	parts := []string{}
	if result.Primary != ecosystem.EcosystemUnknown {
		parts = append(parts, fmt.Sprintf("ecosystem=%s", result.Primary))
	} else {
		parts = append(parts, "ecosystem=unknown")
	}
	if result.Language != "" {
		parts = append(parts, fmt.Sprintf("language=%s", result.Language))
	}
	fmt.Fprintln(&b, strings.Join(parts, " "))

	applied := []string{}
	if ecoApplied != "" {
		applied = append(applied, fmt.Sprintf("--ecosystem=%s", ecoApplied))
	}
	if langApplied != "" {
		applied = append(applied, fmt.Sprintf("--language=%s", langApplied))
	}
	if len(applied) > 0 {
		fmt.Fprintf(&b, "# precheck applied: %s\n", strings.Join(applied, " "))
	}
	return b.String()
}

// buildResolver wires the CLI flags, optional config file, and
// embedded fallback into a single Resolver for this command. The
// config file is loaded explicitly when --config is passed; otherwise
// we look for signatory.config.toml in --project-dir via
// DiscoverAndLoad (absence is OK).
func (cmd *HandoffCmd) buildResolver() (*config.Resolver, error) {
	var cfg *config.Config
	var err error
	switch {
	case cmd.ConfigFile != "":
		cfg, err = config.LoadConfig(cmd.ConfigFile)
	default:
		cfg, err = config.DiscoverAndLoad(cmd.ProjectDir)
	}
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return &config.Resolver{
		CLITemplateDirs:  cmd.TemplateDir,
		CLIFilestoreDirs: cmd.FilestoreDir,
		Config:           cfg,
		EmbeddedFS:       signatory.EmbeddedTemplates,
		EmbeddedPrefix:   "templates",
		BaseDir:          cmd.ProjectDir,
	}, nil
}

// inferTemplateName maps the role/language CLI arguments to a
// specific template file under handoffs/. The mapping is small and
// explicit to keep behavior predictable; callers with non-standard
// variants should pass --template.
func inferTemplateName(role, language string) string {
	switch role {
	case "security":
		if language == "go" {
			return "handoffs/security-review-go-v1.md"
		}
		return "handoffs/security-review-v1.md"
	case "provenance":
		// Language doesn't fork provenance — the template covers
		// PyPI, npm, crates.io, and Go modules in one file.
		return "handoffs/provenance-review-v1.md"
	default:
		// Enum validation ensures we never reach this; keep the
		// fallthrough explicit to catch programmer error in tests.
		return ""
	}
}

// writeHandoff sends rendered to either stdout (when output is
// empty) or to the named file (respecting --force for overwrite
// safety). Nothing is written if rendered is zero-length — callers
// should treat that as an internal error, but it shouldn't happen
// because RenderTemplate always returns at least the original bytes.
func writeHandoff(output string, force bool, rendered []byte) error {
	if output == "" {
		if _, err := os.Stdout.Write(rendered); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flag |= os.O_EXCL
	}
	f, err := os.OpenFile(output, flag, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; pass --force to overwrite", output)
		}
		return fmt.Errorf("open %s: %w", output, err)
	}
	defer f.Close()
	if _, err := f.Write(rendered); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	return nil
}

// reportToStderr prints the informational post-run report: which
// template was used, whether the embedded fallback was in play, and
// which `{PLACEHOLDER}` tokens remained unfilled. The report is
// stderr-only so `signatory handoff … > file.md` captures only the
// template content.
func reportToStderr(source string, embedded bool, unfilled []string) {
	fmt.Fprintf(os.Stderr, "# template: %s\n", source)
	if embedded {
		fmt.Fprintln(os.Stderr, "# note: read from embedded fallback; run `signatory init` to materialize ./templates/")
	}
	if len(unfilled) > 0 {
		fmt.Fprintf(os.Stderr, "# unfilled placeholders (pass flags to substitute): %v\n", unfilled)
	}
}
