package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sarahmaeve/signatory/internal/ecosystem/resolver"
	"github.com/sarahmaeve/signatory/internal/profile"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/synthesis"
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
// Network behavior: this command is offline by default. Language and
// ecosystem auto-detection from a remote repo is available via the
// --network-precheck flag, which probes the GitHub API for the
// target's primary language and ecosystem markers. Without it, pass
// --language and --ecosystem explicitly.
type HandoffCmd struct {
	Role   string `arg:"" enum:"security,provenance,synthesist" help:"Analyst role: security, provenance, or synthesist."`
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
	Language   string `help:"Language flavor for security role (python|go). Auto-detected with --network-precheck; falls back to python." default:"" enum:"python,go,"`
	Intake     string `help:"INTAKE_QUESTION body; the user's specific question for this engagement (one-line)."`
	IntakeFile string `name:"intake-file" help:"Path to a file containing the INTAKE_QUESTION (or '-' for stdin). Use this for multi-line briefs that would otherwise need heredoc gymnastics."`
	Template   string `help:"Explicit template name (e.g., handoffs/security-review-v1.md). Bypasses --role/--language inference."`

	TemplateDir  []string `name:"template-dir" help:"Additional template search directory (repeatable, highest priority)."`
	FilestoreDir []string `name:"filestore-dir" help:"Additional filestore output directory (repeatable). Unused unless --output is a bare filename."`
	ConfigFile   string   `name:"config" help:"Path to signatory.config.toml. If unset, discovered from --project-dir." type:"existingfile"`
	ProjectDir   string   `name:"project-dir" help:"Project root used to locate ./templates/, ./filestore/, and signatory.config.toml." default:"." type:"path"`

	NetworkPrecheck bool   `name:"network-precheck" help:"Fill unset --language and --ecosystem by calling the GitHub API (requires a github.com target). Offline by default; this is the opt-in that authorizes network calls."`
	CloneDir        string `name:"clone-dir" help:"Shallow-clone the target URL into CLONE_DIR/<repo-name>/ and use that path for TARGET_PATH. Uses 'git clone --depth=1'. Skipped if the destination already exists. Requires target to be a URL." type:"path"`

	Output string `short:"o" help:"Write rendered handoff to this file instead of stdout."`
	Force  bool   `help:"Overwrite --output if it exists."`
	Quiet  bool   `help:"Suppress the stderr report (template source, unfilled placeholders)." short:"q"`
	JSON   bool   `name:"json" help:"Emit the rendered handoff as a JSON-escaped string (with surrounding quotes) instead of raw text. Use when piping into another tool that embeds the handoff as a JSON value — removes the need for downstream 'jq -Rs' wrapping and avoids control-character errors when the handoff body contains stored analysis text with newlines."`

	// PrecheckSource overrides the GitHub-backed ecosystem.Source used
	// by --network-precheck. nil → fall through to
	// ghclient.NewClient(GITHUB_TOKEN) at call time. Exists as a
	// struct-field seam (not a package-level var) so parallel tests
	// can't race each other's injection, and so the dependency graph
	// is visible from HandoffCmd's type definition.
	//
	// kong:"-" excludes the field from the CLI surface — it's a
	// programmatic seam, not a user-tunable flag.
	PrecheckSource ecosystem.Source `kong:"-"`

	// RunGitClone overrides the git subprocess invocation for
	// --clone-dir. nil → fall through to defaultGitClone (exec.CommandContext
	// with safeGitEnv). Same rationale as PrecheckSource: parallel-safe
	// injection, visible dependency.
	RunGitClone func(ctx context.Context, url, dest string) error `kong:"-"`

	// EcosystemRegistry overrides the ecosystem resolver registry
	// that --network-precheck consults for pkg:<eco>/<name> targets.
	// nil → fall through to resolver.Default (the process-wide
	// registry shipping npm + go resolvers at init). Tests build a
	// fresh registry with resolver.NewRegistry() and register stubs
	// so they don't hit live registries. Same rationale as
	// PrecheckSource / RunGitClone: parallel-safe injection,
	// dependency visible from the struct definition.
	EcosystemRegistry *resolver.Registry `kong:"-"`
}

// Run executes the handoff render. Errors from template resolution,
// substitution, or write are surfaced directly; the stderr report
// (template source, unfilled placeholders, embedded-fallback notice)
// is informational and goes out independently of Success/failure.
func (cmd *HandoffCmd) Run(globals *Globals) error {
	// Reconcile --intake / --intake-file (§3.4 agent-facing-contract).
	// Intake is optional — the template falls back to an embedded
	// default — so an empty result is fine; the readFreeText helper
	// only cares about conflict and malformed inputs.
	intake, err := readFreeText("intake", cmd.Intake, cmd.IntakeFile)
	if err != nil {
		return err
	}
	cmd.Intake = intake

	// Resolve the target to canonical form. For any input that
	// ResolveTarget understands (GitHub shorthand, github.com URL,
	// `repo:` canonical URI, etc.) pre-populate --name and --url
	// from the resolved metadata, and rewrite cmd.Target itself to
	// the HTTPS clone URL so downstream classifier-driven code
	// (HandoffSubstitutions, applyClone) sees a form it handles
	// natively.
	//
	// Non-URI / non-shorthand inputs — local filesystem paths like
	// ~/code/thefuck, /Users/me/code/proj, ./relative — cleanly
	// fail ResolveTarget; the rest of Run() then handles them via
	// TargetPath as before. ResolveTarget failure is not a CLI
	// error here; it's a signal that the target isn't a
	// remote-repo form.
	if resolved, err := profile.ResolveTarget(cmd.Target); err == nil {
		if cmd.Name == "" {
			cmd.Name = resolved.ShortName
		}
		if resolved.CloneURL != "" {
			if cmd.URL == "" {
				cmd.URL = resolved.CloneURL
			}
			// Feed the HTTPS URL form to downstream steps. This
			// turns every accepted form — owner/repo,
			// github.com/owner/repo, repo:github/owner/name — into
			// the same effective target for TARGET_URL population
			// and --clone-dir processing, closing the
			// "--clone-dir requires a URL target" gap surfaced
			// during the v0.1 dogfood.
			cmd.Target = resolved.CloneURL
		}
	}

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

	// Language stays as whatever precheck detected (or "" if
	// undetected / no precheck). inferTemplateName maps "" to the
	// generic security template — no hardcoded Python default.

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
	defer rc.Close() //nolint:errcheck // template reader close; errors here are not actionable after the read below
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

	// Synthesist role needs the evidence rollup assembled from the
	// store — the synthesis-v1 template embeds it as the body of a
	// fenced JSON block under {EVIDENCE_JSON}. This is the M6c wiring
	// that turns the synthesist from a store-browsing agent into a
	// self-contained one (agent-facing-contract §3.5). Other roles
	// don't need store access; leaving the map untouched keeps the
	// security/provenance handoff paths offline.
	if cmd.Role == "synthesist" {
		evidenceJSON, err := cmd.assembleSynthesisEvidence(context.Background(), globals)
		if err != nil {
			return err
		}
		subs["EVIDENCE_JSON"] = evidenceJSON
	}

	rendered, unfilled := config.RenderTemplate(raw, subs)

	// --json wraps the rendered bytes as a JSON string literal
	// (with surrounding quotes and all control chars escaped).
	// Downstream shell pipelines that embed the handoff into a
	// larger JSON payload can then use $(signatory handoff
	// --json ...) directly instead of piping through `jq -Rs`.
	// Raw bytes contain literal newlines + other control chars
	// from stored analysis text, so the escape is load-bearing.
	emitted := rendered
	if cmd.JSON {
		encoded, err := json.Marshal(string(rendered))
		if err != nil {
			return fmt.Errorf("json-encode handoff body: %w", err)
		}
		emitted = encoded
	}

	if err := writeHandoff(cmd.Output, cmd.Force, emitted); err != nil {
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

// assembleSynthesisEvidence opens the store, resolves cmd.Target to
// its canonical URI, and composes the structured evidence rollup
// that the synthesist-v1 template embeds under {EVIDENCE_JSON}.
// Returns the pretty-printed JSON body as a string for direct
// substitution into the template.
//
// Failure modes:
//
//   - Target doesn't parse as a recognized form → usage error, no
//     store read.
//   - Target has no entity in the store or zero non-synthesis
//     analyses → refuse to emit a synthesist handoff. Dispatching a
//     synthesist against empty evidence produces either a no-op or a
//     fabricated synthesis; catching the empty case here fails fast
//     with a message that tells the operator the right next step
//     (run /analyze on the target first).
//
// Pretty-prints with two-space indent. The handoff body travels via
// WebFetch to the synthesist agent; compact JSON saves bytes but
// hurts token-level attention across a multi-kilobyte evidence
// block.
func (cmd *HandoffCmd) assembleSynthesisEvidence(ctx context.Context, globals *Globals) (string, error) {
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return "", fmt.Errorf("open store for synthesis evidence: %w", err)
	}
	defer s.Close() //nolint:errcheck // store close on function exit; errors not actionable

	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return "", NewUsageError(fmt.Errorf(
			"synthesist handoff: cannot resolve target %q: %w",
			cmd.Target, err))
	}

	assembler := synthesis.New(s)
	evidence, err := assembler.Assemble(ctx, resolved.CanonicalURI)
	if err != nil {
		if errors.Is(err, synthesis.ErrEntityNotFound) {
			return "", fmt.Errorf(
				"synthesist handoff: no entity matches %q in the store. "+
					"Run /analyze on this target first so the security and "+
					"provenance analysts can populate the evidence the "+
					"synthesist will consume",
				resolved.CanonicalURI)
		}
		return "", fmt.Errorf("assemble synthesis evidence: %w", err)
	}

	if len(evidence.Analyses) == 0 {
		return "", fmt.Errorf(
			"synthesist handoff: entity %q has no non-synthesis analyses to synthesize. "+
				"Run /analyze on this target so the security and provenance "+
				"analysts deposit conclusions first",
			resolved.CanonicalURI)
	}

	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal synthesis evidence: %w", err)
	}
	return string(raw), nil
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
	// Pkg-target pre-resolution: if the caller passed a pkg:<eco>/
	// URI (or an npmjs.com URL that ResolveTarget canonicalized to
	// one), consult the ecosystem resolver registry to find its
	// declared source repository. Precheck is GitHub-only; this
	// bridge lets users paste a pkg URI without hand-translating
	// to the source repo first. Works for any registered ecosystem
	// (npm, go, future pypi/cargo) — the registry dispatches.
	//
	// Security note: for most ecosystems (npm, Go via vanity URLs)
	// the declared source is self-reported by the publisher and
	// not cryptographically bound. A malicious package could
	// declare a famous GitHub URL as its "source." The precheck
	// report below discloses DeclaredSource.SelfReported verbatim
	// so a human skimming the output can sanity-check before
	// trusting the downstream analysis.
	var pkgDisclosure string
	if resolved, rerr := profile.ResolveTarget(cmd.Target); rerr == nil &&
		resolved.Scheme == "pkg" && resolved.Ecosystem != "" {
		// Strip the pkg:<eco>/ prefix to get the ecosystem-internal
		// name. resolved.ShortName drops the scope for scoped npm
		// packages ("@types/node" → "node"), so we reconstruct the
		// full name from the canonical URI. Trailing @version (if
		// present) is stripped — resolvers operate per-package, not
		// per-version.
		nameWithVersion := strings.TrimPrefix(resolved.CanonicalURI, "pkg:"+resolved.Ecosystem+"/")
		name := nameWithVersion
		if atIdx := strings.LastIndex(name, "@"); atIdx > 0 {
			// Only strip @version when the @ is AFTER a character
			// (scoped names start with @ at position 0).
			name = name[:atIdx]
		}

		registry := cmd.EcosystemRegistry
		if registry == nil {
			registry = resolver.Default
		}
		source, err := registry.Resolve(ctx, resolved.Ecosystem, name)
		if err != nil {
			if errors.Is(err, resolver.ErrNoResolver) {
				return "", fmt.Errorf("no source resolver registered for ecosystem %q; pass the source URL explicitly (supported: %v)", resolved.Ecosystem, registry.Ecosystems())
			}
			return "", fmt.Errorf("resolving %s package %q source: %w", resolved.Ecosystem, name, err)
		}
		if source.URL == "" {
			return "", fmt.Errorf("%s package %q declares no resolvable source repository; pass the source URL explicitly", resolved.Ecosystem, name)
		}
		if !looksLikeGitHubURL(source.URL) {
			return "", fmt.Errorf("%s package %q declares source %q; only github.com sources are supported by --network-precheck", resolved.Ecosystem, name, source.URL)
		}
		verifiedNote := "self-reported in registry metadata, not cryptographically verified"
		if !source.SelfReported && source.VerifiedBy != "" {
			verifiedNote = "verified via " + source.VerifiedBy
		}
		pkgDisclosure = fmt.Sprintf("# precheck: %s package %q declares source %s (%s)\n",
			resolved.Ecosystem, name, source.URL, verifiedNote)

		// Rewrite the target so downstream steps (substitution,
		// clone, ecosystem detector) see the github URL. Mirror the
		// Run-level pre-population: fill cmd.URL if empty so the
		// TARGET_URL substitution picks up the resolved source.
		cmd.Target = source.URL
		if cmd.URL == "" {
			cmd.URL = source.URL
		}
	}

	// Reject non-GitHub URLs early. NormalizeGitHubRepoInput is
	// lenient — it accepts any URL with 2+ path segments and
	// would happily parse gitlab.com/foo/bar as owner="gitlab.com"
	// name="foo". The precheck only works with GitHub's API, so
	// we guard URL-form targets with the stricter host check.
	// Bare owner/repo shorthand (no scheme) is allowed through —
	// NormalizeGitHubRepoInput assumes GitHub for those.
	if strings.Contains(cmd.Target, "://") && !looksLikeGitHubURL(cmd.Target) {
		return "", fmt.Errorf("--network-precheck requires a github.com URL; got %q", cmd.Target)
	}

	// Accept both full GitHub URLs and owner/repo shorthand. The
	// NormalizeGitHubRepoInput function handles all forms:
	//   https://github.com/owner/repo
	//   github.com/owner/repo
	//   owner/repo
	//   git@github.com:owner/repo
	// If it can extract owner+name, the target is GitHub-resolvable.
	_, owner, name, err := profile.NormalizeGitHubRepoInput(cmd.Target)
	if err != nil {
		return "", fmt.Errorf("target %q is not a recognizable GitHub reference (expected https://github.com/owner/repo or owner/repo): %w", cmd.Target, err)
	}

	source := cmd.PrecheckSource
	if source == nil {
		// Production path: construct a real github client. Reading the
		// env here (rather than inside the swap-able factory) means
		// tests that don't inject PrecheckSource exercise this exact
		// code, and env-var behavior is discoverable from go-to-definition
		// on applyNetworkPrecheck.
		source = ghclient.NewClient(os.Getenv("GITHUB_TOKEN"))
	}
	detector := ecosystem.NewDetector(source)

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
	// languageToFlavor maps the top ten GitHub languages to stable
	// slugs. For anything else detected (Haskell, Kotlin, etc.) the
	// slug is "" and inferTemplateName routes to the generic security
	// template — which is language-agnostic and correct, unlike the
	// old Python fallback.
	langApplied := ""
	langWarning := ""
	if cmd.Language == "" {
		switch flavor := languageToFlavor(result.Language); {
		case flavor != "":
			cmd.Language = flavor
			langApplied = flavor
		case result.Language != "":
			// Detected a language, but no flavor for it. The generic
			// template handles this correctly; the warning is
			// informational so the user knows what happened.
			langWarning = fmt.Sprintf("# precheck warning: detected language %q has no flavor mapping; using generic security template (consider passing --template for a language-specific review)\n", result.Language)
		}
	}

	return pkgDisclosure + formatPrecheckReport(owner, name, result, ecoApplied, langApplied) + langWarning, nil
}

// defaultGitClone is the production git-clone implementation that
// applyClone falls back to when HandoffCmd.RunGitClone is nil. It is
// a regular package-level function (not a var) to remove any
// accidental-mutation surface — tests inject via HandoffCmd.RunGitClone
// field assignment, not via swap-the-global.
//
// Creates an exec.CommandContext so a cancelled context propagates to
// the git subprocess.
//
// Security: the subprocess runs with a minimal, hardened environment
// derived from os.Environ() with dangerous git-specific vars stripped.
// Without this, a hostile parent environment (GIT_TERMINAL_PROMPT,
// GIT_SSH_COMMAND, GIT_PROXY_COMMAND, GIT_CONFIG_COUNT, etc.) could
// redirect git's network transport, invoke attacker-controlled helpers,
// or smuggle config that overrides the URL we validated. We strip rather
// than whitelist so the process still inherits PATH, HOME, and
// TLS-related vars that legitimate git operations need.
func defaultGitClone(ctx context.Context, url, dest string) error {
	// G204 rationale: CommandContext doesn't invoke a shell, so
	// shell-metacharacter injection in url/dest is not a threat.
	// url is validated by safeGitCloneURL (rejects ?, #, userinfo,
	// NUL) and constrained upstream by looksLikeGitHubURL /
	// ClassifyTarget. dest is validated by the containment check in
	// applyClone (filepath.Rel against an EvalSymlinks-resolved
	// parent) and by safeCloneRepoName on the derived repo name.
	// Env inheritance is stripped to a vetted set by safeGitEnv so
	// GIT_SSH_COMMAND / GIT_PROXY_COMMAND / GIT_CONFIG_* can't
	// redirect the clone.
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", url, dest) //nolint:gosec // G204: url validated by safeGitCloneURL, dest validated by applyClone containment check, env sanitized by safeGitEnv
	cmd.Env = safeGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// safeGitEnv returns a copy of os.Environ() with dangerous git-specific
// environment variables removed. These vars can redirect git's transport,
// invoke arbitrary helper programs, or override config options in ways
// that bypass the URL validation we performed before calling git clone.
//
// Stripped variables and their threat vectors:
//
//   - GIT_TERMINAL_PROMPT: if "1", git prompts for credentials on stdin —
//     useful interactively but a hang risk in a non-terminal context and a
//     signal that the environment may be adversarially configured.
//   - GIT_SSH_COMMAND / GIT_SSH: override the SSH binary/wrapper used for
//     ssh:// transports; an attacker-controlled value achieves RCE.
//   - GIT_PROXY_COMMAND: overrides the proxy command for non-standard
//     transports; same RCE risk as GIT_SSH_COMMAND.
//   - GIT_EXEC_PATH: overrides the directory git searches for sub-commands
//     (git-clone, git-fetch, etc.); an attacker can inject malicious binaries.
//   - GIT_CONFIG_COUNT / GIT_CONFIG_KEY_* / GIT_CONFIG_VALUE_*: the bulk
//     config-injection interface added in git 2.31; can override any config
//     option including core.sshCommand, http.proxy, etc.
//   - GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM: redirect the config file paths
//     that git reads, allowing wholesale config replacement.
//   - GIT_DIR / GIT_WORK_TREE: redefine what git considers its repository and
//     work tree; with GIT_DIR set to an existing .git elsewhere on the system,
//     a clone can be made to operate on the wrong repository.
//   - GIT_ASKPASS: program to invoke for credential prompts; overrides the
//     OS credential helper and can capture credentials mid-operation.
//
// We do NOT strip PATH, HOME, USER, SSH_AUTH_SOCK, SSL_CERT_FILE,
// REQUESTS_CA_BUNDLE, or XDG_* because legitimate git operations depend on them.
func safeGitEnv() []string {
	// Variables whose mere presence (regardless of value) is dangerous.
	deny := map[string]bool{
		"GIT_TERMINAL_PROMPT": true,
		"GIT_SSH_COMMAND":     true,
		"GIT_SSH":             true,
		"GIT_PROXY_COMMAND":   true,
		"GIT_EXEC_PATH":       true,
		"GIT_CONFIG_GLOBAL":   true,
		"GIT_CONFIG_SYSTEM":   true,
		"GIT_DIR":             true,
		"GIT_WORK_TREE":       true,
		"GIT_ASKPASS":         true,
		"SSH_ASKPASS":         true,
		"SSH_ASKPASS_REQUIRE": true,
	}
	raw := os.Environ()
	safe := make([]string, 0, len(raw))
	for _, kv := range raw {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		// Strip GIT_CONFIG_COUNT and GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_*
		// (bulk injection interface). The prefix check covers all numbered
		// variants without enumerating them.
		if deny[key] ||
			strings.HasPrefix(key, "GIT_CONFIG_COUNT") ||
			strings.HasPrefix(key, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		safe = append(safe, kv)
	}
	// Force-disable terminal prompts regardless of whether the var was
	// already present. This guarantees non-interactive behavior even if
	// the original env had a value we didn't strip.
	safe = append(safe, "GIT_TERMINAL_PROMPT=0")
	return safe
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

	// Create the parent if it doesn't exist. This matches the user
	// model of `git clone URL DEST` (which creates DEST) and
	// `signatory init DIR` (which creates DIR). Insisting that the
	// user pre-create the parent was a UX paper-cut surfaced during
	// dogfood — the friction had no defensive value because the
	// writability probe below catches real permission problems.
	//
	// MkdirAll is idempotent: an existing directory is fine, a
	// missing one is created with 0755, an existing FILE at the path
	// errors (which we want — the IsDir check below also catches that
	// case for race conditions).
	// 0o750: owner rwx, group rx, others none. Matches the init-time
	// scaffold dir perms in internal/config/init.go.
	if err := os.MkdirAll(absParent, 0o750); err != nil {
		return "", "", fmt.Errorf("create clone-dir parent %q: %w", absParent, err)
	}
	info, err := os.Stat(absParent)
	if err != nil {
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
	_ = probe.Close()           // probe was just created; close errors don't affect the writability check
	_ = os.Remove(probe.Name()) // best-effort cleanup of the probe file

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

	clone := cmd.RunGitClone
	if clone == nil {
		clone = defaultGitClone
	}
	if err := clone(cloneCtx, cmd.Target, destClean); err != nil {
		return "", "", err
	}

	report = fmt.Sprintf("# clone: %s → %s\n", cmd.Target, destClean)
	return destClean, report, nil
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
// language flavor slug we use in template filenames. Recognized
// languages return a stable slug; unrecognized languages return ""
// so the caller falls back to the generic security template.
//
// The top ten languages by GitHub usage are mapped here so that
// --network-precheck can route to the right template without
// manual --language overrides. Not all flavors have dedicated
// templates today — inferTemplateName handles the fallback.
func languageToFlavor(primaryLanguage string) string {
	switch strings.ToLower(primaryLanguage) {
	case "go":
		return "go"
	case "python":
		return "python"
	case "rust":
		return "rust"
	case "javascript":
		return "javascript"
	case "typescript":
		return "typescript"
	case "java":
		return "java"
	case "c#":
		return "csharp"
	case "c++":
		return "cpp"
	case "c":
		return "c"
	case "php":
		return "php"
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
// specific template file under handoffs/. Languages with dedicated
// templates (go, python, rust) get their own file; everything else
// falls back to the generic security template. Callers with
// non-standard variants should pass --template.
func inferTemplateName(role, language string) string {
	switch role {
	case "security":
		return inferSecurityTemplate(language)
	case "provenance":
		// Language doesn't fork provenance — the template covers
		// PyPI, npm, crates.io, and Go modules in one file.
		return "handoffs/provenance-review-v1.md"
	case "synthesist":
		// The synthesist reads the signatory store, not source —
		// no language/ecosystem fork needed.
		return "handoffs/synthesis-v1.md"
	default:
		// Enum validation ensures we never reach this; keep the
		// fallthrough explicit to catch programmer error in tests.
		return ""
	}
}

// inferSecurityTemplate returns the security-review template path
// for the given language flavor. Languages with dedicated templates
// get their own file; everything else gets the generic template.
//
// Adding a new language-specific template:
//  1. Create handoffs/security-review-<flavor>-v1.md
//  2. Add the flavor to this switch
//  3. Add the GitHub language string to languageToFlavor
func inferSecurityTemplate(language string) string {
	switch language {
	case "go":
		return "handoffs/security-review-go-v1.md"
	case "python":
		return "handoffs/security-review-v1.md"
	case "rust":
		return "handoffs/security-review-rust-v1.md"
	default:
		// Covers "" (undetected), and recognized flavors like
		// "javascript", "java", etc. that don't have dedicated
		// templates yet.
		return "handoffs/security-review-generic-v1.md"
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
	// G304 rationale: output is the user's explicit --output argument
	// (kong's type:"path"); writing to that file is writeHandoff's
	// purpose. G302 rationale: handoff templates are not secrets and
	// users pipe them into agents / inspect them in editors running
	// as different uids; 0o644 is the right default.
	f, err := os.OpenFile(output, flag, 0o644) //nolint:gosec // G304,G302: caller-supplied path, user-facing content, not secrets
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; pass --force to overwrite", output)
		}
		return fmt.Errorf("open %s: %w", output, err)
	}
	// Defer is a safety net against early returns below. The primary
	// close is explicit after the write so flush errors surface as a
	// real error to the caller — a silent close error on a write path
	// can mean data wasn't actually persisted.
	defer f.Close() //nolint:errcheck // safety net; the primary close is below
	if _, err := f.Write(rendered); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", output, err)
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
