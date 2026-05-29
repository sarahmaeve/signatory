package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sarahmaeve/signatory/internal/prdefense"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/signal/source"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ErrPRDefenseBlocked is returned when the scan verdict is "block". It
// drives a non-zero exit (exitCodeFor → 1) so the command works as a CI
// gate. main skips the stderr echo for it — the report is on stdout.
var ErrPRDefenseBlocked = errors.New("pr-scan: pull request blocked by defense scan")

const (
	// prScanSource is the Source stamped on every pr-scan signal.
	prScanSource = "pr-scan"
	// prSignalTTL bounds finding freshness. Findings are pinned to an
	// immutable head commit, so a generous TTL is correct.
	prSignalTTL = 90 * 24 * time.Hour
	// prBlobCap bounds per-blob reads from the object DB (matches the
	// content-injection scan cap).
	prBlobCap = 2 * 1024 * 1024
	// verdictSignalType is the always-emitted rollup signal; its value is
	// a verdictRecord and it is the cache / summary anchor.
	verdictSignalType = "pr_defense_verdict"
)

// verdictRecord is the JSON value stored as the pr_defense_verdict signal
// and parsed back for cache decisions and summaries. It carries the PR
// AUTHOR so a future workflow can attribute (and burn) the user entity
// behind a malicious PR.
type verdictRecord struct {
	Verdict           prdefense.Verdict       `json:"verdict"`
	Reasons           []string                `json:"reasons,omitempty"`
	HeadSHA           string                  `json:"head_sha"`
	Scanned           int                     `json:"scanned"`
	Skipped           []prdefense.SkippedFile `json:"skipped,omitempty"`
	Author            string                  `json:"author,omitempty"`
	AuthorAssociation string                  `json:"author_association,omitempty"`
}

// PRScanCmd is the `pr-scan` command group. `pr-scan <owner/repo#N>`
// checks a PR (the default `check` verb); `pr-scan summary` lists prior
// captures. It is a group rather than a leaf because kong can't give a
// command both its own Run and sibling subcommands — same shape as
// burn / posture / serve.
type PRScanCmd struct {
	Check   PRScanCheckCmd   `cmd:"" default:"withargs" help:"Deep-scan a pull request's changed files for injected prompt-injection, exfil hosts, and persistence writes (owner/repo#N)."`
	Summary PRScanSummaryCmd `cmd:"" help:"List previously-captured PR scans, or show one in detail."`
}

// PRScanCheckCmd scans a single pull request's changed files. It is
// cache-aware: a prior capture whose head SHA matches the PR's current
// head is reported from the store without re-cloning; --refresh forces a
// fresh scan and re-store.
type PRScanCheckCmd struct {
	Target  string `arg:"" help:"Pull request: owner/repo#N, https://github.com/owner/repo/pull/N, or patch:github/owner/repo/N."`
	Path    string `help:"Directory to hold the PR clone. Defaults to filestore/clones/pr-<repo>-<N>." type:"path"`
	JSON    bool   `help:"Output the report as JSON." default:"false"`
	Refresh bool   `help:"Force a fresh scan and re-store even if an unchanged capture exists." default:"false"`

	// Test seams (not flags).
	Client      *ghclient.Client                                                                                      `kong:"-"`
	RunGit      func(ctx context.Context, workdir string, args ...string) error                                       `kong:"-"`
	NewProvider func(ctx context.Context, clonePath, headSHA string) (prdefense.ContentProvider, func() error, error) `kong:"-"`
	Stdout      io.Writer                                                                                             `kong:"-"`
	Stderr      io.Writer                                                                                             `kong:"-"`
}

func (cmd *PRScanCheckCmd) io(globals *Globals) (context.Context, io.Writer, io.Writer) {
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

func (cmd *PRScanCheckCmd) Run(globals *Globals) error {
	ctx, stdout, stderr := cmd.io(globals)

	resolved, owner, repo, n, err := resolvePRTarget(cmd.Target)
	if err != nil {
		return err
	}

	// PR metadata: changed files + the CURRENT head SHA. Fetched on every
	// run (cheap) — the head SHA is how we detect whether the PR changed
	// since a prior capture.
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

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	// Cache: a prior capture at the same head SHA is reported without a
	// re-scan, unless --refresh forces one.
	if !cmd.Refresh {
		rec, at, ok, err := loadVerdict(ctx, s, resolved.CanonicalURI)
		if err != nil {
			return err
		}
		if ok && rec.HeadSHA == pr.HeadSHA {
			return cmd.renderCached(stdout, owner, repo, n, rec, at)
		}
		if ok {
			_, _ = fmt.Fprintf(stderr,
				"PR changed since last scan (was head %s @ %s); re-scanning.\n",
				shortSHA(rec.HeadSHA), at.UTC().Format(time.RFC3339))
		}
	}

	rep, err := cmd.scan(ctx, stderr, resolved, pr, n)
	if err != nil {
		return err
	}

	// Persist: mint the patch: entity and append findings + verdict
	// (carrying the author for later attribution).
	now := time.Now().UTC()
	entity, _, err := s.EnsureEntityByCanonicalURI(ctx, resolved.CanonicalURI, resolved.ShortName)
	if err != nil {
		return fmt.Errorf("mint patch entity %s: %w", resolved.CanonicalURI, err)
	}
	if err := s.AppendSignals(ctx, prScanSignals(entity.ID, rep, pr.Author, pr.AuthorAssociation, now)); err != nil {
		return fmt.Errorf("persist pr-scan signals: %w", err)
	}
	if err := linkAuthorIdentity(ctx, s, entity.ID, pr, now); err != nil {
		return err
	}

	if cmd.JSON {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal report: %w", err)
		}
		_, _ = fmt.Fprintln(stdout, string(data))
	} else {
		renderPRReport(stdout, owner, repo, n, rep)
	}

	if rep.Verdict == prdefense.VerdictBlock {
		return ErrPRDefenseBlocked
	}
	return nil
}

// scan clones the repo, fetches the PR head (objects only), and runs the
// content scanners over the changed-file blobs.
func (cmd *PRScanCheckCmd) scan(ctx context.Context, stderr io.Writer, resolved *profile.ResolvedTarget, pr ghclient.PullRequest, n int) (prdefense.Report, error) {
	clonePath := cmd.Path
	if clonePath == "" {
		clonePath = filepath.Join("filestore", "clones", fmt.Sprintf("pr-%s-%d", shortName(resolved), n))
	}
	absPath, err := filepath.Abs(clonePath)
	if err != nil {
		return prdefense.Report{}, fmt.Errorf("resolve clone path %q: %w", clonePath, err)
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
		return prdefense.Report{}, err
	}
	if cleanup != nil {
		defer cleanup() //nolint:errcheck // best-effort resource release
	}

	changed := make([]prdefense.ChangedFile, len(pr.Files))
	for i, f := range pr.Files {
		changed[i] = prdefense.ChangedFile{Path: f.Path, Status: f.Status}
	}
	return prdefense.Scan(ctx, provider, pr.HeadSHA, changed)
}

// cachedReport is the --json shape for a cache hit: a prdefense.Report
// (so consumers parse it uniformly) plus a cached marker and the prior
// scan timestamp.
type cachedReport struct {
	prdefense.Report
	Cached              bool      `json:"cached"`
	PreviouslyScannedAt time.Time `json:"previously_scanned_at"`
}

func (cmd *PRScanCheckCmd) renderCached(w io.Writer, owner, repo string, n int, rec verdictRecord, at time.Time) error {
	if cmd.JSON {
		out := cachedReport{
			Report: prdefense.Report{
				HeadSHA: rec.HeadSHA, Verdict: rec.Verdict, Reasons: rec.Reasons,
				Scanned: rec.Scanned, Skipped: rec.Skipped,
			},
			Cached:              true,
			PreviouslyScannedAt: at.UTC(),
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal cached report: %w", err)
		}
		_, _ = fmt.Fprintln(w, string(data))
	} else {
		_, _ = fmt.Fprintf(w, "PR Defense Scan — %s/%s#%d (head %s)\n", owner, repo, n, shortSHA(rec.HeadSHA))
		_, _ = fmt.Fprintf(w, "Verdict: %s  (previously scanned %s; head unchanged — pass --refresh to re-scan)\n",
			strings.ToUpper(string(rec.Verdict)), at.UTC().Format(time.RFC3339))
		for _, reason := range rec.Reasons {
			_, _ = fmt.Fprintf(w, "  - %s\n", reason)
		}
	}
	if rec.Verdict == prdefense.VerdictBlock {
		return ErrPRDefenseBlocked
	}
	return nil
}

// PRScanSummaryCmd lists previously-captured PR scans, or shows one in
// detail. It reads only the store — no network, no git.
type PRScanSummaryCmd struct {
	Target string    `arg:"" optional:"" help:"owner/repo#N to show one capture in detail; omit to list all captures."`
	JSON   bool      `help:"Output as JSON." default:"false"`
	Stdout io.Writer `kong:"-"`
}

func (cmd *PRScanSummaryCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	if strings.TrimSpace(cmd.Target) != "" {
		return cmd.showOne(ctx, s, stdout)
	}
	return cmd.listAll(ctx, s, stdout)
}

// capture is one row in the summary listing.
type capture struct {
	Ref               string            `json:"ref"`
	Verdict           prdefense.Verdict `json:"verdict"`
	Author            string            `json:"author,omitempty"`
	AuthorAssociation string            `json:"author_association,omitempty"`
	HeadSHA           string            `json:"head_sha"`
	ScannedAt         time.Time         `json:"scanned_at"`
}

func (cmd *PRScanSummaryCmd) listAll(ctx context.Context, s store.Store, w io.Writer) error {
	patches, err := s.ListEntitiesByType(ctx, profile.EntityPatch)
	if err != nil {
		return err
	}
	var caps []capture
	for _, e := range patches {
		sigs, err := s.GetLatestSignals(ctx, e.ID)
		if err != nil {
			return err
		}
		for _, sg := range sigs {
			if sg.Type != verdictSignalType {
				continue
			}
			var rec verdictRecord
			if err := json.Unmarshal(sg.Value, &rec); err != nil {
				return fmt.Errorf("decode verdict for %s: %w", e.CanonicalURI, err)
			}
			caps = append(caps, capture{
				Ref:               refFromPatchURI(e.CanonicalURI),
				Verdict:           rec.Verdict,
				Author:            rec.Author,
				AuthorAssociation: rec.AuthorAssociation,
				HeadSHA:           rec.HeadSHA,
				ScannedAt:         sg.CollectedAt.UTC(),
			})
		}
	}
	// Surface blocks first, then warns, then clears; newest first within.
	sort.SliceStable(caps, func(i, j int) bool {
		if ri, rj := verdictRank(caps[i].Verdict), verdictRank(caps[j].Verdict); ri != rj {
			return ri < rj
		}
		return caps[i].ScannedAt.After(caps[j].ScannedAt)
	})

	if cmd.JSON {
		data, err := json.MarshalIndent(caps, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, string(data))
		return nil
	}

	if len(caps) == 0 {
		_, _ = fmt.Fprintln(w, "No pr-scan captures recorded.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VERDICT\tPR\tAUTHOR\tHEAD\tSCANNED")
	for _, c := range caps {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			strings.ToUpper(string(c.Verdict)), c.Ref, authorLabel(c.Author, c.AuthorAssociation),
			shortSHA(c.HeadSHA), c.ScannedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func (cmd *PRScanSummaryCmd) showOne(ctx context.Context, s store.Store, w io.Writer) error {
	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return NewUsageError(fmt.Errorf("resolve target %q: %w", cmd.Target, err))
	}
	if resolved.Scheme != "patch" {
		return NewUsageError(fmt.Errorf("pr-scan summary target must be a pull request (owner/repo#N); %q resolved to %s",
			cmd.Target, resolved.CanonicalURI))
	}
	rec, at, ok, err := loadVerdict(ctx, s, resolved.CanonicalURI)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintf(w, "No pr-scan record for %s\n", resolved.CanonicalURI)
		return nil
	}
	if cmd.JSON {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, string(data))
		return nil
	}
	_, _ = fmt.Fprintf(w, "PR:      %s\n", refFromPatchURI(resolved.CanonicalURI))
	_, _ = fmt.Fprintf(w, "Verdict: %s\n", strings.ToUpper(string(rec.Verdict)))
	_, _ = fmt.Fprintf(w, "Author:  %s\n", authorLabel(rec.Author, rec.AuthorAssociation))
	_, _ = fmt.Fprintf(w, "Head:    %s\n", rec.HeadSHA)
	_, _ = fmt.Fprintf(w, "Scanned: %s\n", at.UTC().Format(time.RFC3339))
	for _, reason := range rec.Reasons {
		_, _ = fmt.Fprintf(w, "  - %s\n", reason)
	}
	return nil
}

// --- shared helpers ---

// resolvePRTarget parses a pr-scan target into its patch URI plus
// owner / repo / PR-number.
func resolvePRTarget(target string) (resolved *profile.ResolvedTarget, owner, repo string, n int, err error) {
	resolved, err = profile.ResolveTarget(target)
	if err != nil {
		return nil, "", "", 0, err
	}
	if resolved.Scheme != "patch" {
		return nil, "", "", 0, NewUsageError(fmt.Errorf(
			"pr-scan target must be a pull request (owner/repo#N or a /pull/N URL); %q resolved to %s",
			target, resolved.CanonicalURI))
	}
	if resolved.Platform != profile.PlatformGitHub {
		return nil, "", "", 0, NewUsageError(fmt.Errorf("pr-scan supports GitHub only in v0.1; got platform %q", resolved.Platform))
	}
	n, err = strconv.Atoi(resolved.PatchID)
	if err != nil || n <= 0 {
		return nil, "", "", 0, fmt.Errorf("invalid pull-request number %q", resolved.PatchID)
	}
	return resolved, resolved.Owner, strings.TrimSuffix(resolved.ShortName, "#"+resolved.PatchID), n, nil
}

// loadVerdict returns the latest stored verdict record for a patch URI,
// its collection timestamp, and whether one exists. A missing entity is
// (false, nil), not an error.
func loadVerdict(ctx context.Context, s store.Store, patchURI string) (verdictRecord, time.Time, bool, error) {
	ent, err := s.FindEntityByURI(ctx, patchURI)
	if errors.Is(err, store.ErrNotFound) {
		return verdictRecord{}, time.Time{}, false, nil
	}
	if err != nil {
		return verdictRecord{}, time.Time{}, false, err
	}
	sigs, err := s.GetLatestSignals(ctx, ent.ID)
	if err != nil {
		return verdictRecord{}, time.Time{}, false, err
	}
	for _, sg := range sigs {
		if sg.Type != verdictSignalType {
			continue
		}
		var rec verdictRecord
		if err := json.Unmarshal(sg.Value, &rec); err != nil {
			return verdictRecord{}, time.Time{}, false, fmt.Errorf("decode verdict for %s: %w", patchURI, err)
		}
		return rec, sg.CollectedAt, true, nil
	}
	return verdictRecord{}, time.Time{}, false, nil
}

// linkAuthorIdentity mints (or reuses) the identity: entity for a PR's
// human author and links the patch to it via a pr_author signal — the
// per-user home for Engineer Profile data and the join key for a future
// contributor-burn cascade. The mint is idempotent on the canonical URI,
// so an author who also owns repos resolves to the same identity entity.
// Bot / GitHub-App authors (which don't resolve to a real user account)
// are skipped entirely — no identity minted, no link emitted.
func linkAuthorIdentity(ctx context.Context, s store.Store, patchEntityID string, pr ghclient.PullRequest, now time.Time) error {
	if pr.Author == "" || isBotAuthor(pr.Author, pr.AuthorType) {
		return nil
	}
	identURI := profile.CanonicalIdentityURI("github", pr.Author)
	if _, _, err := s.EnsureEntityByCanonicalURI(ctx, identURI, pr.Author); err != nil {
		return fmt.Errorf("mint author identity %s: %w", identURI, err)
	}
	var r signal.CollectionResult
	r.RecordSignal(patchEntityID, "pr_author", prScanSource, now, prSignalTTL, map[string]any{
		"login":              pr.Author,
		"author_association": pr.AuthorAssociation,
		"identity":           identURI,
	})
	return s.AppendSignals(ctx, r.Signals())
}

// isBotAuthor reports whether a PR author is a bot / GitHub-App identity
// rather than a human user account. Authoritative on user.type == "Bot";
// the [bot] login suffix is a backstop for connectors that don't surface
// the type.
func isBotAuthor(login, authorType string) bool {
	return strings.EqualFold(authorType, "Bot") ||
		strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// prScanSignals builds the store signals for one scan: each finding type
// only when non-empty, and the verdict (carrying the author) always.
func prScanSignals(entityID string, rep prdefense.Report, author, authorAssociation string, now time.Time) []profile.Signal {
	var r signal.CollectionResult

	if len(rep.ContentInjection) > 0 {
		r.RecordSignal(entityID, "pr_content_injection", prScanSource, now, prSignalTTL,
			map[string]any{"files": rep.ContentInjection})
	}
	if len(rep.ExfilHits) > 0 {
		r.RecordSignal(entityID, "pr_exfil_host_reference", prScanSource, now, prSignalTTL,
			map[string]any{"hits": rep.ExfilHits})
	}
	if len(rep.AgentConfigPaths) > 0 {
		r.RecordSignal(entityID, "pr_agent_config_touched", prScanSource, now, prSignalTTL,
			map[string]any{"paths": rep.AgentConfigPaths})
	}
	if len(rep.ASTConcerns) > 0 {
		r.RecordSignal(entityID, "pr_ast_concern", prScanSource, now, prSignalTTL,
			map[string]any{"languages": rep.ASTConcerns})
	}
	r.RecordSignal(entityID, verdictSignalType, prScanSource, now, prSignalTTL, verdictRecord{
		Verdict:           rep.Verdict,
		Reasons:           rep.Reasons,
		HeadSHA:           rep.HeadSHA,
		Scanned:           rep.Scanned,
		Skipped:           rep.Skipped,
		Author:            author,
		AuthorAssociation: authorAssociation,
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

func authorLabel(author, association string) string {
	switch {
	case author == "" && association == "":
		return "(unknown)"
	case association == "":
		return author
	case author == "":
		return "(" + association + ")"
	default:
		return author + " (" + association + ")"
	}
}

// refFromPatchURI turns patch:github/owner/repo/N into owner/repo#N for
// display, falling back to the raw URI if it isn't the expected shape.
func refFromPatchURI(uri string) string {
	body, ok := strings.CutPrefix(uri, "patch:github/")
	if !ok {
		return uri
	}
	parts := strings.Split(body, "/")
	if len(parts) != 3 {
		return uri
	}
	return parts[0] + "/" + parts[1] + "#" + parts[2]
}

func verdictRank(v prdefense.Verdict) int {
	switch v {
	case prdefense.VerdictBlock:
		return 0
	case prdefense.VerdictWarn:
		return 1
	default:
		return 2
	}
}

func shortName(resolved *profile.ResolvedTarget) string {
	// ShortName is "repo#N"; the clone dir wants just the repo segment.
	if i := strings.IndexByte(resolved.ShortName, '#'); i > 0 {
		return resolved.ShortName[:i]
	}
	return resolved.ShortName
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
