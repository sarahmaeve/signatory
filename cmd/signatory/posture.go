package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// PostureCmd manages dependency posture tiers.
//
// Posture decisions are version-scoped: `posture set kong --version=v1.15.0`
// records an opinion about v1.15.0 specifically, and v1.16.0 is
// automatically "unexamined" until the user reviews it. This is the
// core shift from v1 — vetting a version no longer leaks forward.
type PostureCmd struct {
	Get    PostureGetCmd    `cmd:"" default:"withargs" help:"View the posture for an entity."`
	Set    PostureSetCmd    `cmd:"" help:"Set the posture tier for an entity."`
	Unset  PostureUnsetCmd  `cmd:"" help:"Withdraw (soft-delete) a previously-set posture. Use when a recorded decision turns out to be wrong."`
	Accept PostureAcceptCmd `cmd:"" help:"Promote a synthesist's proposed posture into a recorded posture row. Reads the proposal from a synthesis output id and writes a posture with optional tier/version/rationale overrides."`
}

// PostureGetCmd views the posture for an entity.
//
// Default behavior: return the most recently set posture across all
// versions, plus a one-line hint if other versions also have recorded
// decisions. With --version, return the exact row for that version.
// With --all, print every version in reverse-chronological order.
type PostureGetCmd struct {
	Target  string `arg:"" help:"Entity to view posture for."`
	Version string `help:"Show posture for a specific version." optional:""`
	All     bool   `help:"Show all recorded postures (one per version)." default:"false"`
}

func (cmd *PostureGetCmd) Run(globals *Globals) error {
	ctx := context.Background()

	// Plan-A canonicalization: a posture for `pkg:npm/X@V` lives at
	// the `pkg:npm/X` entity with the posture row's `version` column
	// = "V". Strip any @V suffix from the target and fold it into
	// cmd.Version so the downstream GetPosture lookup routes to the
	// canonical storage form. The dogfood bug (2026-04-21) was
	// precisely this: a versioned-URI query landed at the versioned
	// entity, which has no posture rows, instead of the unversioned
	// entity where posture accept wrote the row.
	base, version, err := normalizeTargetForPosture(cmd.Target, cmd.Version)
	if err != nil {
		return NewUsageError(err)
	}
	cmd.Target = base
	cmd.Version = version

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	entity, err := resolveEntity(ctx, s, cmd.Target)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Printf("No posture recorded for: %s\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}

	// --version=X: exact lookup.
	if cmd.Version != "" {
		p, err := s.GetPosture(ctx, entity.ID, cmd.Version)
		if errors.Is(err, store.ErrNotFound) {
			fmt.Printf("No posture recorded for %s @ %s\n", cmd.Target, cmd.Version)
			return nil
		}
		if err != nil {
			return err
		}
		printPosture(entity, p)
		return nil
	}

	// --all or default: fetch everything and let the flag decide.
	postures, err := s.GetPostures(ctx, entity.ID)
	if err != nil {
		return err
	}
	if len(postures) == 0 {
		fmt.Printf("No posture recorded for: %s\n", cmd.Target)
		return nil
	}

	if cmd.All {
		for i, p := range postures {
			if i > 0 {
				fmt.Println()
				fmt.Println("---")
				fmt.Println()
			}
			// Go 1.22+ gives each iteration its own `p`, so taking
			// &p is safe without the old `p := p` shadow.
			printPosture(entity, &p)
		}
		return nil
	}

	// Default: latest + hint.
	latest := postures[0]
	printPosture(entity, &latest)
	if len(postures) > 1 {
		fmt.Println()
		fmt.Printf("(%d other version%s recorded — rerun with --all to see all)\n",
			len(postures)-1, pluralS(len(postures)-1))
	}
	return nil
}

// printPosture formats a single posture to stdout.
func printPosture(entity *profile.Entity, p *profile.Posture) {
	fmt.Printf("Entity:    %s\n", entity.ShortName)
	fmt.Printf("URI:       %s\n", entity.CanonicalURI)
	fmt.Printf("Tier:      %s\n", p.Tier)
	if p.Version != "" {
		fmt.Printf("Version:   %s\n", p.Version)
	} else {
		fmt.Printf("Version:   (unversioned)\n")
	}
	fmt.Printf("Rationale: %s\n", p.Rationale)
	fmt.Printf("Set by:    %s\n", p.SetBy)
	fmt.Printf("Set at:    %s\n", p.SetAt.Format(time.RFC3339))
}

// PostureSetCmd records a posture decision.
//
// Version is optional but strongly recommended — a posture without a
// version applies to "the entity as a whole" (the v1-style semantics),
// which is almost never what you want. The CLI warns when --version
// is omitted.
//
// Rationale may be supplied via --rationale (one-line) or
// --rationale-file (multi-line, path or "-" for stdin). Exactly one
// must be non-empty. See agent-facing-contract §3.4.
type PostureSetCmd struct {
	Target        string `arg:"" help:"Entity to set posture for."`
	Tier          string `help:"Posture tier (vetted-frozen, trusted-for-now, unexamined, unknown-provenance, or rejected)." enum:"vetted-frozen,trusted-for-now,unexamined,unknown-provenance,rejected" required:""`
	Rationale     string `help:"Rationale for the posture decision (one-line). For multi-line rationales use --rationale-file."`
	RationaleFile string `name:"rationale-file" help:"Path to a file containing the rationale (or '-' for stdin). Use this for multi-line synthesis output that would otherwise need heredoc gymnastics."`
	Version       string `help:"Specific version being attested (strongly recommended; inherited from URI @version when target carries one)." optional:""`
	DryRun        bool   `name:"dry-run" help:"Print what would change without writing to the store."`
}

func (cmd *PostureSetCmd) Run(globals *Globals) error {
	ctx := context.Background()

	// Plan-A canonicalization: a posture for `pkg:npm/X@V` lives at
	// the `pkg:npm/X` entity with the row's version column = "V".
	// Strip any @V suffix from the target before entity lookup so
	// the write routes to the canonical unversioned entity. Merges
	// with --version flag; the two must agree or this is a usage
	// error. See design/m6-synthesis-contract.md and the 2026-04-21
	// dogfood where version-suffix queries missed postures stored at
	// the unversioned form.
	base, version, err := normalizeTargetForPosture(cmd.Target, cmd.Version)
	if err != nil {
		return NewUsageError(err)
	}
	cmd.Target = base
	cmd.Version = version

	// Shape-check the version before any side effect. The same
	// validator runs at synthesis-ingest (see ProposedPosture.validate);
	// applying it here closes the manual-input door — whether the
	// operator typed `--version` directly or pasted a command an agent
	// emitted, malformed version strings (pasted URIs, multi-line
	// blobs, over-length pastes) are rejected before ensureEntity or
	// SetPosture run. Usage-error wrapping so the CLI exits EX_USAGE.
	if err := exchange.ValidateVersionScopeShape(cmd.Version); err != nil {
		return NewUsageError(fmt.Errorf("posture set --version: %w", err))
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	auditLog := globals.NewAuditLogger(s)
	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
	}

	// Reconcile --rationale / --rationale-file. Exactly one must be
	// non-empty; we preserve kong's "required" semantic in Run-level
	// validation rather than via a kong tag so either flag form can
	// satisfy it (§3.4, agent-facing-contract.md).
	rationale, err := readFreeText("rationale", cmd.Rationale, cmd.RationaleFile)
	if err != nil {
		return NewUsageError(err)
	}
	if rationale == "" {
		return NewUsageError(fmt.Errorf("posture set: --rationale or --rationale-file is required (an empty rationale isn't a decision)"))
	}
	cmd.Rationale = rationale

	if cmd.DryRun {
		// Resolve without touching the store so dry-run is
		// side-effect-free: ensureEntity would otherwise INSERT a
		// stub row before the dry-run message printed.
		resolved, rerr := profile.ResolveTarget(cmd.Target)
		if rerr != nil {
			return NewUsageError(rerr)
		}
		fmt.Printf("[dry-run] Would set posture for %s", resolved.ShortName)
		if cmd.Version != "" {
			fmt.Printf(" @ %s", cmd.Version)
		}
		fmt.Printf(": %s\n", cmd.Tier)
		fmt.Printf("[dry-run] URI:       %s\n", resolved.CanonicalURI)
		fmt.Printf("[dry-run] Rationale: %s\n", firstLine(cmd.Rationale))
		return nil
	}

	entity, err := ensureEntity(ctx, s, cmd.Target)
	if err != nil {
		return err
	}

	posture := &profile.Posture{
		EntityID:  entity.ID,
		Tier:      profile.PostureTier(cmd.Tier),
		Version:   cmd.Version,
		Rationale: cmd.Rationale,
		SetBy:     actor,
		SetAt:     time.Now().UTC(),
	}

	if err := s.SetPosture(ctx, posture); err != nil {
		return err
	}

	// Audit.
	detail, _ := json.Marshal(map[string]any{
		"canonical_uri": entity.CanonicalURI,
		"version":       cmd.Version,
		"tier":          cmd.Tier,
		"rationale":     cmd.Rationale,
	})
	if err := auditLog.LogAction(ctx, actor, "set_posture", entity.ID, string(detail)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: audit log write failed: %v\n", err)
	}

	if cmd.Version != "" {
		fmt.Printf("Posture set for %s @ %s: %s\n", entity.ShortName, cmd.Version, cmd.Tier)
	} else {
		fmt.Printf("Posture set for %s (unversioned): %s\n", entity.ShortName, cmd.Tier)
		fmt.Fprintln(os.Stderr, "warning: no --version specified — this posture applies to the entity as a whole, "+
			"not a specific version. Consider re-running with --version for version-specific trust decisions.")
	}
	return nil
}

// normalizeTargetForPosture reconciles a target string + optional
// --version flag into the Plan-A canonical storage form: an
// unversioned target and a version string. If the target carries a
// `@V` suffix (pkg URIs only per v0.1 grammar), the suffix is
// stripped and folded into the returned version. If a --version flag
// was also passed, the two must agree or a usage error is returned.
//
// Under Plan A, postures for `pkg:npm/X@V` live at the `pkg:npm/X`
// entity with the posture row's `version` column set to "V". Every
// posture-family command (set/get/unset/accept) funnels through this
// helper so the two input forms (`X@V` and `X --version V`) resolve
// to the same storage row. Non-pkg URIs pass through unchanged in
// the target position.
//
// Returns a usage error (not wrapped) when --version and the URI
// suffix disagree — callers should wrap with NewUsageError so the
// exit code reflects the caller's mistake.
func normalizeTargetForPosture(target, flagVersion string) (baseTarget, version string, err error) {
	base, uriVersion := profile.SplitURIVersion(target)
	if uriVersion == "" {
		return target, flagVersion, nil
	}
	switch {
	case flagVersion == "":
		return base, uriVersion, nil
	case flagVersion == uriVersion:
		return base, flagVersion, nil
	default:
		return "", "", fmt.Errorf(
			"--version %q conflicts with target URI version %q; pass a single version or remove one of the two",
			flagVersion, uriVersion)
	}
}

// --- Shared helpers used by posture, burn, and analyze. ---

// resolveEntity looks up an entity by user-supplied target (a
// canonical URI string, or a GitHub repo in any parseable form).
// Returns (nil, store.ErrNotFound) when no entity matches — the
// caller uses errors.Is to decide whether absence is an error or
// a "create it" situation.
//
// Two absence conditions map to ErrNotFound:
//
//  1. The target parses as a canonical URI (or is one already) but
//     no stored entity has it.
//  2. The target doesn't parse as a canonical URI AND doesn't
//     parse as a GitHub-repo shape. We treat this as absence
//     rather than a distinct parse error because the caller's
//     interest is "does this entity exist?" — the parse failure is
//     an implementation detail of how we try to resolve it.
func resolveEntity(ctx context.Context, s store.Store, target string) (*profile.Entity, error) {
	// Route through ResolveTarget so every accepted form (shorthand,
	// URL, canonical URI, versioned pkg URI) maps to its canonical
	// URI uniformly. Before M1 this function tried the target as-is
	// then fell back to GitHubRepoInput normalization; that split-
	// pipeline path couldn't see the @version suffix on pkg URIs.
	resolved, rerr := profile.ResolveTarget(target)
	if rerr != nil {
		// Target isn't any form we recognize. Caller's "does this
		// entity exist?" question resolves to "no" regardless of
		// the underlying parse failure.
		return nil, store.ErrNotFound
	}
	entity, err := s.FindEntityByURI(ctx, resolved.CanonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup entity: %w", err)
	}
	return entity, nil
}

// ensureEntity resolves or creates an entity for the given target.
// Used by posture set and burn add, which should work even on
// entities that have never been analyzed.
//
// If resolveEntity returns ErrNotFound we proceed to the create
// path; any other error propagates. This is the canonical pattern
// for consuming ErrNotFound as "absence is a valid operating state."
func ensureEntity(ctx context.Context, s store.Store, target string) (*profile.Entity, error) {
	existing, err := resolveEntity(ctx, s, target)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// Entity not found. ResolveTarget already proved the target is
	// a form we recognize (resolveEntity returned ErrNotFound, not a
	// parse error) — call it again for the canonical metadata we
	// need to build the stub row.
	resolved, rerr := profile.ResolveTarget(target)
	if rerr != nil {
		return nil, fmt.Errorf("cannot resolve %q: not a recognized target form (expected GitHub shorthand / URL or one of pkg:<ecosystem>/<name>[@<version>], repo:<platform>/<owner>/<name>, identity:<platform>/<user>, org:<platform>/<name>, patch:<platform>/<owner>/<repo>/<id>): %w", target, rerr)
	}

	entity := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: resolved.CanonicalURI,
		Type:         entityTypeForScheme(resolved.Scheme),
		ShortName:    resolved.ShortName,
		// Ecosystem MUST be stamped here for pkg: targets — the
		// downstream `signatory analyze --refresh` resolver guards
		// (resolveNpmRepo / resolvePyPIRepo) gate on this field
		// and silently skip when it's empty. Pre-fix omission
		// caused the 2026-04-28 idna refresh meltdown: entities
		// landed with ecosystem='', the refresh skipped repo
		// resolution, and Layer-1 collection emitted zero signals.
		// resolved.Ecosystem is empty for non-pkg schemes (repo:,
		// identity:, org:) — same as before for those.
		Ecosystem: resolved.Ecosystem,
		URL:       resolved.CloneURL, // empty for non-repo schemes
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.PutEntity(ctx, entity); err != nil {
		return nil, fmt.Errorf("create entity: %w", err)
	}
	return entity, nil
}

// entityTypeForScheme maps a canonical-URI scheme to the EntityType
// that should be stored on the entity row. Previously ensureEntity
// hardcoded EntityProject for GitHub URLs and EntityPackage for
// everything else — identity: and org: URIs would have landed under
// EntityPackage, which is wrong but rarely surfaced because those
// schemes came in through different code paths. Routing through
// ResolveTarget makes the mapping explicit.
func entityTypeForScheme(scheme string) profile.EntityType {
	switch scheme {
	case "repo":
		return profile.EntityProject
	case "pkg":
		return profile.EntityPackage
	case "identity":
		return profile.EntityIdentity
	case "org":
		return profile.EntityOrg
	case "patch":
		return profile.EntityPatch
	default:
		// Unknown schemes shouldn't reach here — ResolveTarget
		// constrains the set — but EntityPackage is the least-
		// surprising fallback (purl-style identifier).
		return profile.EntityPackage
	}
}

// PostureUnsetCmd withdraws a previously-set posture for an entity.
// The row stays in the DB with withdrawal metadata filled in; reads
// default-filter it out. A subsequent `posture set` on the same
// (entity, version) reactivates the row.
//
// Like PostureSetCmd, accepts the URI-embedded @version form. If the
// URI carries @V and --version is also set they must agree.
//
// A --reason is optional; supplying one is strongly encouraged so
// future readers of the audit log understand why a decision was
// withdrawn ("author compromised" vs "reassessment pending" are very
// different narratives).
type PostureUnsetCmd struct {
	Target     string `arg:"" help:"Entity whose posture should be withdrawn."`
	Version    string `help:"Specific version whose posture to withdraw (inherited from URI @V when target carries one)." optional:""`
	Reason     string `help:"Reason for withdrawing the posture (one-line). For multi-line reasons use --reason-file."`
	ReasonFile string `name:"reason-file" help:"Path to a file containing the withdrawal reason (or '-' for stdin)."`
	DryRun     bool   `name:"dry-run" help:"Print what would change without writing to the store."`
}

func (cmd *PostureUnsetCmd) Run(globals *Globals) error {
	ctx := context.Background()

	// Plan-A canonicalization: a posture for `pkg:npm/X@V` lives at
	// the `pkg:npm/X` entity with the row's version column = "V".
	// Strip any @V suffix so the WithdrawPosture lookup routes to the
	// canonical storage form. See PostureSetCmd for the sibling fix.
	base, version, err := normalizeTargetForPosture(cmd.Target, cmd.Version)
	if err != nil {
		return NewUsageError(err)
	}
	cmd.Target = base
	cmd.Version = version

	reason, err := readFreeText("reason", cmd.Reason, cmd.ReasonFile)
	if err != nil {
		return NewUsageError(err)
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	auditLog := globals.NewAuditLogger(s)
	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
	}

	entity, err := resolveEntity(ctx, s, cmd.Target)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no entity found for %q; nothing to unset", cmd.Target)
	}
	if err != nil {
		return err
	}

	if cmd.DryRun {
		fmt.Printf("[dry-run] Would withdraw posture for %s", entity.ShortName)
		if cmd.Version != "" {
			fmt.Printf(" @ %s", cmd.Version)
		}
		if reason != "" {
			fmt.Printf(" (reason: %s)", firstLine(reason))
		}
		fmt.Println()
		return nil
	}

	now := time.Now().UTC()
	if err := s.WithdrawPosture(ctx, entity.ID, cmd.Version, actor, reason, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no active posture to withdraw for %s (version %q); it may already be withdrawn or never have been set", entity.ShortName, cmd.Version)
		}
		return err
	}

	detail, _ := json.Marshal(map[string]any{
		"canonical_uri": entity.CanonicalURI,
		"version":       cmd.Version,
		"reason":        reason,
	})
	if err := auditLog.LogAction(ctx, actor, "unset_posture", entity.ID, string(detail)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: audit log write failed: %v\n", err)
	}

	if cmd.Version != "" {
		fmt.Printf("Posture withdrawn for %s @ %s\n", entity.ShortName, cmd.Version)
	} else {
		fmt.Printf("Posture withdrawn for %s (unversioned)\n", entity.ShortName)
	}
	return nil
}

// firstLine returns the first line of s, for compact dry-run output
// that doesn't blow up the terminal with a multi-line rationale.
func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i] + "…"
		}
	}
	return s
}

// PostureAcceptCmd promotes a synthesist's proposed_posture into a
// recorded Posture row. Reads the proposal from a synthesis output
// id (produced by /analyze or a manual synthesist run) and writes a
// posture row whose tier/version/rationale come from the proposal
// by default, with optional per-field overrides.
//
// Deviations — places where the user diverged from the synthesist's
// proposal — are captured in the audit log detail as `proposed_*`
// fields. Presence of a `proposed_tier` / `proposed_version_scope`
// / `proposed_rationale_summary` field in the audit blob IS the
// deviation signal. Absence means "accepted verbatim."
//
// Safety:
//
//   - Non-TTY invocations must pass --yes. The confirmation prompt
//     is interactive-only; without --yes in a script, the command
//     errors rather than silently blocking on stdin.
//   - --dry-run prints the resolved proposal + deviations without
//     touching the store or audit log. Use before the real accept.
//
// See design/m6-synthesis-contract.md §6 (M6d).
type PostureAcceptCmd struct {
	OutputID string `arg:"" help:"Synthesis output UUID to accept. Find it in signatory show-analyses or /analyze pipeline output."`

	// Overrides. Each empty → pull from the synthesist's proposal.
	// Non-empty → override and record the original proposal in the
	// audit detail under the matching proposed_* field.
	Tier          string `help:"Override the proposed tier." enum:"vetted-frozen,trusted-for-now,unexamined,unknown-provenance,rejected," default:""`
	Version       string `help:"Override the proposed version_scope."`
	Rationale     string `help:"Override the proposed rationale (one-line). For multi-line use --rationale-file."`
	RationaleFile string `name:"rationale-file" help:"Path to a file containing the override rationale (or '-' for stdin)."`

	Yes    bool `help:"Skip the confirmation prompt. Required in non-TTY environments so scripts can't accidentally block on stdin."`
	DryRun bool `name:"dry-run" help:"Print the resolved proposal and any deviations without writing to the store or audit log."`

	// IsTTY overrides the default stdin-is-terminal check. nil →
	// fall through to isStdinTTY. Exists so unit tests can
	// deterministically simulate non-TTY regardless of how `go
	// test` is invoked — without this hook, a test run from a
	// terminal inherits a TTY stdin and the non-TTY error path
	// can't be exercised. kong:"-" excludes the field from the
	// CLI surface; it's a programmatic test seam only.
	IsTTY func() bool `kong:"-"`
}

func (cmd *PostureAcceptCmd) Run(globals *Globals) error {
	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	// Load the proposal first — fails fast with a clear error if the
	// output id doesn't exist or isn't a synthesis (GetSynthesisProposal
	// returns ErrNotFound for both cases; the user-facing message has
	// to cover both).
	proposal, err := s.GetSynthesisProposal(ctx, cmd.OutputID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf(
				"no synthesis proposal found for output id %q: the id may not exist, or the output may not be a synthesis. "+
					"Run `signatory show-analyses <target>` to see available analyses",
				cmd.OutputID)
		}
		return fmt.Errorf("load synthesis proposal: %w", err)
	}

	// Reconcile --rationale / --rationale-file before proceeding.
	// Empty rationale override means "use the proposal's
	// rationale_summary"; non-empty means deviate.
	overrideRationale, err := readFreeText("rationale", cmd.Rationale, cmd.RationaleFile)
	if err != nil {
		return NewUsageError(err)
	}

	// Resolve the final posture fields. Each override defaults to
	// the proposal when empty.
	finalTier := proposal.Tier
	if cmd.Tier != "" {
		finalTier = cmd.Tier
	}
	finalVersion := proposal.VersionScope
	if cmd.Version != "" {
		finalVersion = cmd.Version
	}
	finalRationale := proposal.RationaleSummary
	if overrideRationale != "" {
		finalRationale = overrideRationale
	}

	// Look up the synthesis's entity so we know where to write the
	// posture row. The entity is the one this synthesis was indexed
	// under (respects the M2 collected_from hop: a synthesis indexed
	// under pkg:npm/X with collected_from=repo:github/Y produces a
	// posture on pkg:npm/X, matching agent-facing-contract §3.2).
	//
	// Walk the analyst_output.entity_id FK directly instead of
	// reconstructing the entity URI and looking it up by string.
	// String re-derivation was the 2026-04-22 dogfood bug: for a
	// synthesis ingested with target `pkg:golang/...@v1.11.1`, the
	// entity lives at that exact versioned URI (current
	// ensureEntityForTarget preserves @V), but the prior accept-path
	// ran SplitURIVersion before FindEntityByURI and looked up a
	// stripped form that wasn't in the store. Going through the FK
	// is schema-tight: there's exactly one entity per output row and
	// no shape to mistranslate.
	//
	// Upstream entity-model cleanup (ingest normalizing @V off the
	// entity URI so repeat analyses across versions share a row, per
	// the Plan-A canonicalization doc in profile/uri.go) is tracked
	// separately. The FK walk is correct under either model.
	entity, err := s.GetOutputEntity(ctx, cmd.OutputID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf(
				"no entity found for synthesis output %q — the analyst_output row has no entity_id or the entity was deleted; "+
					"re-ingest the synthesis or run `signatory show-analyses` to verify the output id",
				cmd.OutputID)
		}
		return fmt.Errorf("load synthesis entity for output %q: %w", cmd.OutputID, err)
	}

	if cmd.DryRun {
		fmt.Printf("[dry-run] Would accept synthesis %s for %s\n", cmd.OutputID, entity.ShortName)
		cmd.printProposalSummary(os.Stdout, entity, proposal, finalTier, finalVersion, finalRationale, overrideRationale)
		return nil
	}

	// Non-TTY invocations require --yes. The TTY check is injectable
	// so tests can simulate non-TTY deterministically; production
	// leaves cmd.IsTTY nil and falls through to isStdinTTY.
	ttyCheck := cmd.IsTTY
	if ttyCheck == nil {
		ttyCheck = isStdinTTY
	}
	if !cmd.Yes {
		if !ttyCheck() {
			return NewUsageError(fmt.Errorf(
				"posture accept refuses to prompt in a non-TTY environment; pass --yes to confirm up-front"))
		}
		fmt.Printf("Accept synthesis %s for %s?\n", cmd.OutputID, entity.ShortName)
		cmd.printProposalSummary(os.Stdout, entity, proposal, finalTier, finalVersion, finalRationale, overrideRationale)
		fmt.Print("Proceed? [y/N]: ")
		var response string
		if _, scanErr := fmt.Fscanln(os.Stdin, &response); scanErr != nil {
			// Scanln returns an error on empty input (user just
			// hit enter). Treat that as "no."
			response = ""
		}
		if strings.ToLower(strings.TrimSpace(response)) != "y" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
	}

	posture := &profile.Posture{
		EntityID:  entity.ID,
		Tier:      profile.PostureTier(finalTier),
		Version:   finalVersion,
		Rationale: finalRationale,
		SetBy:     actor,
		SetAt:     time.Now().UTC(),
	}
	if err := s.SetPosture(ctx, posture); err != nil {
		return err
	}

	// Audit detail with deviation signals. Each proposed_* field is
	// present only when the user overrode — its presence IS the
	// "user disagreed on this field" signal.
	detail := map[string]any{
		"canonical_uri":              entity.CanonicalURI,
		"version":                    finalVersion,
		"tier":                       finalTier,
		"rationale":                  finalRationale,
		"accepted_from_synthesis_id": cmd.OutputID,
	}
	if cmd.Tier != "" && cmd.Tier != proposal.Tier {
		detail["proposed_tier"] = proposal.Tier
	}
	if cmd.Version != "" && cmd.Version != proposal.VersionScope {
		detail["proposed_version_scope"] = proposal.VersionScope
	}
	if overrideRationale != "" && overrideRationale != proposal.RationaleSummary {
		detail["proposed_rationale_summary"] = proposal.RationaleSummary
	}
	detailJSON, _ := json.Marshal(detail)
	auditLog := globals.NewAuditLogger(s)
	if err := auditLog.LogAction(ctx, actor, "accept_posture", entity.ID, string(detailJSON)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: audit log write failed: %v\n", err)
	}

	if finalVersion != "" {
		fmt.Printf("Posture accepted for %s @ %s: %s\n", entity.ShortName, finalVersion, finalTier)
	} else {
		fmt.Printf("Posture accepted for %s (unversioned): %s\n", entity.ShortName, finalTier)
	}
	return nil
}

// printProposalSummary formats the resolved proposal + any
// overrides to w. Shared between --dry-run output and the TTY
// confirmation prompt so both surfaces show the same information.
func (cmd *PostureAcceptCmd) printProposalSummary(
	w io.Writer,
	entity *profile.Entity,
	proposal *exchange.ProposedPosture,
	finalTier, finalVersion, finalRationale, overrideRationale string,
) {
	fmt.Fprintf(w, "  URI:       %s\n", entity.CanonicalURI)
	fmt.Fprintf(w, "  Tier:      %s", finalTier)
	if cmd.Tier != "" && cmd.Tier != proposal.Tier {
		fmt.Fprintf(w, " (overridden from %s)", proposal.Tier)
	}
	fmt.Fprintln(w)
	if finalVersion != "" {
		fmt.Fprintf(w, "  Version:   %s", finalVersion)
		if cmd.Version != "" && cmd.Version != proposal.VersionScope {
			from := proposal.VersionScope
			if from == "" {
				from = "(unversioned)"
			}
			fmt.Fprintf(w, " (overridden from %s)", from)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  Rationale: %s", firstLine(finalRationale))
	if overrideRationale != "" && overrideRationale != proposal.RationaleSummary {
		fmt.Fprint(w, " (overridden)")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Source:    synthesis output %s\n", cmd.OutputID)
}

// isStdinTTY reports whether stdin is attached to a terminal. Used
// by PostureAcceptCmd to decide whether to prompt interactively or
// require --yes. Pure stdlib (os.Stat of the file descriptor mode)
// to avoid adding an isatty dependency.
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
