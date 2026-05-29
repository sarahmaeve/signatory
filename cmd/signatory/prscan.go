package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/prdefense"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/signal/source"
)

// ErrPRDefenseBlocked is returned by PRScanCmd.Run when the scan verdict
// is "block". It drives a non-zero exit (exitCodeFor → 1) so the command
// works as a CI gate. main skips the stderr echo for it — the human
// report is already on stdout.
var ErrPRDefenseBlocked = errors.New("pr-scan: pull request blocked by defense scan")

// prSignalTTL bounds the freshness of pr-scan findings. They are pinned
// to an immutable head commit, so a generous TTL is correct — re-scans
// of the same head append identical findings under a new timestamp.
const prSignalTTL = 90 * 24 * time.Hour

// prBlobCap bounds per-blob reads from the object DB. Matches the
// content-injection scan cap — larger files would be truncated by that
// scanner anyway, and the bound is an abuse defense on a hostile PR.
const prBlobCap = 2 * 1024 * 1024

// PRScanCmd deep-scans a single pull request's changed files for
// supply-chain attacks (injected prompt-injection, exfil hosts,
// persistence-write / weaponized AST) before merge. Standalone — NOT
// part of general `analyze` collection.
type PRScanCmd struct {
	Target string `arg:"" help:"Pull request: owner/repo#N, https://github.com/owner/repo/pull/N, or patch:github/owner/repo/N."`
	Path   string `help:"Directory to hold the PR clone. Defaults to filestore/clones/pr-<repo>-<N>." type:"path"`
	JSON   bool   `help:"Output the report as JSON." default:"false"`

	// Test seams (not flags).
	Client      *ghclient.Client                                                                                      `kong:"-"`
	RunGit      func(ctx context.Context, workdir string, args ...string) error                                       `kong:"-"`
	NewProvider func(ctx context.Context, clonePath, headSHA string) (prdefense.ContentProvider, func() error, error) `kong:"-"`
	Stdout      io.Writer                                                                                             `kong:"-"`
	Stderr      io.Writer                                                                                             `kong:"-"`
}

func (cmd *PRScanCmd) io(globals *Globals) (context.Context, io.Writer, io.Writer) {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, stdout, stderr
}

func (cmd *PRScanCmd) Run(globals *Globals) error {
	ctx, stdout, stderr := cmd.io(globals)

	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return err
	}
	if resolved.Scheme != "patch" {
		return NewUsageError(fmt.Errorf(
			"pr-scan target must be a pull request (owner/repo#N or a /pull/N URL); %q resolved to %s",
			cmd.Target, resolved.CanonicalURI))
	}
	if resolved.Platform != profile.PlatformGitHub {
		return NewUsageError(fmt.Errorf("pr-scan supports GitHub only in v0.1; got platform %q", resolved.Platform))
	}
	n, err := strconv.Atoi(resolved.PatchID)
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid pull-request number %q", resolved.PatchID)
	}
	owner := resolved.Owner
	repo := strings.TrimSuffix(resolved.ShortName, "#"+resolved.PatchID)

	// 1. PR metadata: changed files + the head commit SHA to pin the scan.
	client := cmd.Client
	if client == nil {
		client = ghclient.NewClient(os.Getenv("GITHUB_TOKEN"))
	}
	pr, err := client.FetchPullRequest(ctx, owner, repo, n)
	if err != nil {
		return fmt.Errorf("fetch PR %s/%s#%d: %w", owner, repo, n, err)
	}
	if pr.HeadSHA == "" {
		return fmt.Errorf("PR %s/%s#%d: GitHub did not report a head commit SHA; cannot pin the scan", owner, repo, n)
	}

	// 2. Content provider: clone the repo, fetch the PR head ref (objects
	// only, no checkout), and read changed-file blobs by SHA. Overridable
	// for tests.
	clonePath := cmd.Path
	if clonePath == "" {
		clonePath = filepath.Join("filestore", "clones", fmt.Sprintf("pr-%s-%d", repo, n))
	}
	absPath, err := filepath.Abs(clonePath)
	if err != nil {
		return fmt.Errorf("resolve clone path %q: %w", clonePath, err)
	}
	newProvider := cmd.NewProvider
	if newProvider == nil {
		cloneURL := resolved.CloneURL
		newProvider = func(ctx context.Context, cp, headSHA string) (prdefense.ContentProvider, func() error, error) {
			if err := ensureCloneAtPath(ctx, stderr, cmd.RunGit, cp, cloneURL); err != nil {
				return nil, nil, err
			}
			if err := fetchPullRef(ctx, stderr, cmd.RunGit, cp, n); err != nil {
				return nil, nil, err
			}
			return newBlobProvider(ctx, cp, headSHA)
		}
	}
	provider, cleanup, err := newProvider(ctx, absPath, pr.HeadSHA)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup() //nolint:errcheck // best-effort resource release
	}

	// 3. Scan the changelist.
	changed := make([]prdefense.ChangedFile, len(pr.Files))
	for i, f := range pr.Files {
		changed[i] = prdefense.ChangedFile{Path: f.Path, Status: f.Status}
	}
	rep, err := prdefense.Scan(ctx, provider, pr.HeadSHA, changed)
	if err != nil {
		return err
	}

	// 4. Persist: mint the patch: entity and append findings + verdict.
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable
	entity, _, err := s.EnsureEntityByCanonicalURI(ctx, resolved.CanonicalURI, resolved.ShortName)
	if err != nil {
		return fmt.Errorf("mint patch entity %s: %w", resolved.CanonicalURI, err)
	}
	if err := s.AppendSignals(ctx, prScanSignals(entity.ID, rep, time.Now().UTC())); err != nil {
		return fmt.Errorf("persist pr-scan signals: %w", err)
	}

	// 5. Render.
	if cmd.JSON {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal report: %w", err)
		}
		_, _ = fmt.Fprintln(stdout, string(data))
	} else {
		renderPRReport(stdout, owner, repo, n, rep)
	}

	// 6. Exit code: block fails the gate.
	if rep.Verdict == prdefense.VerdictBlock {
		return ErrPRDefenseBlocked
	}
	return nil
}

// prScanSignals builds the store signals for one scan: each finding type
// only when it has content, and the verdict always.
func prScanSignals(entityID string, rep prdefense.Report, now time.Time) []profile.Signal {
	var r signal.CollectionResult
	const src = "pr-scan"

	if len(rep.ContentInjection) > 0 {
		r.RecordSignal(entityID, "pr_content_injection", src, now, prSignalTTL,
			map[string]any{"files": rep.ContentInjection})
	}
	if len(rep.ExfilHits) > 0 {
		r.RecordSignal(entityID, "pr_exfil_host_reference", src, now, prSignalTTL,
			map[string]any{"hits": rep.ExfilHits})
	}
	if len(rep.AgentConfigPaths) > 0 {
		r.RecordSignal(entityID, "pr_agent_config_touched", src, now, prSignalTTL,
			map[string]any{"paths": rep.AgentConfigPaths})
	}
	if len(rep.ASTConcerns) > 0 {
		r.RecordSignal(entityID, "pr_ast_concern", src, now, prSignalTTL,
			map[string]any{"languages": rep.ASTConcerns})
	}
	r.RecordSignal(entityID, "pr_defense_verdict", src, now, prSignalTTL,
		map[string]any{
			"verdict":  rep.Verdict,
			"reasons":  rep.Reasons,
			"head_sha": rep.HeadSHA,
			"scanned":  rep.Scanned,
			"skipped":  rep.Skipped,
		})
	return r.Signals()
}

func renderPRReport(w io.Writer, owner, repo string, n int, rep prdefense.Report) {
	_, _ = fmt.Fprintf(w, "PR Defense Scan — %s/%s#%d (head %s)\n", owner, repo, n, shortSHA(rep.HeadSHA))
	_, _ = fmt.Fprintf(w, "Verdict: %s\n", strings.ToUpper(string(rep.Verdict)))
	for _, reason := range rep.Reasons {
		_, _ = fmt.Fprintf(w, "  - %s\n", reason)
	}
	for _, h := range rep.ExfilHits {
		_, _ = fmt.Fprintf(w, "  exfil host    %s:%d  %s\n", h.File, h.Line, h.Host)
	}
	for _, fi := range rep.ContentInjection {
		tag := ""
		if fi.IsAgentConfig {
			tag = " (agent-config file)"
		}
		prims := make([]string, len(fi.Result.Findings))
		for i, f := range fi.Result.Findings {
			prims[i] = string(f.Primitive)
		}
		_, _ = fmt.Fprintf(w, "  injection     %s  %s%s\n", fi.Path, strings.Join(prims, ", "), tag)
	}
	for _, c := range rep.ASTConcerns {
		_, _ = fmt.Fprintf(w, "  AST concern   %s  %s\n", c.Language, strings.Join(c.Concern.ConcerningFeatures, ", "))
	}
	_, _ = fmt.Fprintf(w, "Scanned %d file(s), skipped %d.\n", rep.Scanned, len(rep.Skipped))
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// blobProvider reads a PR's changed-file content from a clone's object
// database at a fixed head SHA — no working-tree checkout, so it is
// immune to symlink / path-traversal tricks in a hostile PR, and reads
// are size-capped. It satisfies prdefense.ContentProvider.
type blobProvider struct {
	bs   *source.BlobStreamer
	tree map[string]string // changed path → blob SHA at head
}

func newBlobProvider(ctx context.Context, clonePath, headSHA string) (prdefense.ContentProvider, func() error, error) {
	bs, err := source.NewBlobStreamer(ctx, clonePath, source.WithMaxBlobSize(prBlobCap))
	if err != nil {
		return nil, nil, fmt.Errorf("open blob streamer: %w", err)
	}
	blobs, err := bs.ListTreeBlobs(ctx, headSHA)
	if err != nil {
		_ = bs.Close()
		return nil, nil, fmt.Errorf("list tree at head %s: %w", headSHA, err)
	}
	tree := make(map[string]string, len(blobs))
	for _, b := range blobs {
		tree[b.Path] = b.SHA
	}
	return &blobProvider{bs: bs, tree: tree}, bs.Close, nil
}

func (p *blobProvider) ReadFile(ctx context.Context, path string) ([]byte, prdefense.SkipReason) {
	sha, ok := p.tree[path]
	if !ok {
		return nil, prdefense.SkipMissing
	}
	content, err := p.bs.ReadBlob(ctx, sha)
	if err != nil {
		if errors.Is(err, source.ErrBlobSizeExceedsCap) {
			return nil, prdefense.SkipTooLarge
		}
		return nil, prdefense.SkipMissing
	}
	return content, prdefense.SkipNone
}
