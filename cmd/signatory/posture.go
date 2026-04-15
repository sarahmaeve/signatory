package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

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
	Get PostureGetCmd `cmd:"" default:"withargs" help:"View the posture for an entity."`
	Set PostureSetCmd `cmd:"" help:"Set the posture tier for an entity."`
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
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := context.Background()

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
			p := p
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
type PostureSetCmd struct {
	Target    string `arg:"" help:"Entity to set posture for."`
	Tier      string `help:"Posture tier." enum:"vetted-frozen,trusted-for-now,unexamined,unknown-provenance" required:""`
	Rationale string `help:"Rationale for the posture decision." required:""`
	Version   string `help:"Specific version being attested (strongly recommended)." optional:""`
}

func (cmd *PostureSetCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := context.Background()
	auditLog := globals.NewAuditLogger(s)
	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
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
	detail, _ := json.Marshal(map[string]interface{}{
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
	// First try the target as-is — it might already be a canonical URI.
	if entity, err := s.FindEntityByURI(ctx, target); err == nil {
		return entity, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("lookup entity: %w", err)
	}

	// Fall back to GitHub-style input normalization. A parse
	// failure here is semantically the same as "the entity doesn't
	// exist": the input was neither a canonical URI we have nor a
	// GitHub shape we can normalize. Report it as ErrNotFound so
	// callers handle both absence conditions uniformly.
	normalized, _, _, perr := profile.NormalizeGitHubRepoInput(target)
	if perr != nil {
		return nil, store.ErrNotFound
	}
	entity, err := s.FindEntityByURI(ctx, normalized)
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

	// Create a stub entity. Try to parse as a GitHub repo first so we
	// get a proper canonical URI; otherwise, treat the target as a
	// canonical URI itself (e.g. `pkg:npm/express`).
	canonicalURI, owner, repoName, perr := profile.NormalizeGitHubRepoInput(target)
	if perr == nil {
		// Looks like a GitHub repo.
		entity := &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: canonicalURI,
			Type:         profile.EntityProject,
			ShortName:    owner + "/" + repoName,
			URL:          "https://github.com/" + owner + "/" + repoName,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		if err := s.PutEntity(ctx, entity); err != nil {
			return nil, fmt.Errorf("create entity: %w", err)
		}
		return entity, nil
	}

	// Treat the target as a canonical URI (e.g. purl) — the caller
	// probably knows what they're doing and wants the entity created
	// under the URI they supplied. Validate before persisting so we
	// fail closed on garbage input rather than wrapping arbitrary
	// text as if it were a canonical identifier (#78).
	if err := profile.ValidateCanonicalURI(target); err != nil {
		return nil, fmt.Errorf("cannot resolve %q: not a parseable GitHub repo and not a valid canonical URI (expected forms: pkg:<ecosystem>/<name>, repo:<platform>/<owner>/<name>, identity:<platform>/<user>, org:<platform>/<name>, patch:<platform>/<owner>/<repo>/<id>): %w", target, err)
	}
	entity := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: target,
		Type:         profile.EntityPackage,
		ShortName:    target,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := s.PutEntity(ctx, entity); err != nil {
		return nil, fmt.Errorf("create entity: %w", err)
	}
	return entity, nil
}
