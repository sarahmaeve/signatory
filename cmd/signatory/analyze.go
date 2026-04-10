package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalyzeCmd retrieves or collects the trust profile for a target.
//
// Target resolution: the user-supplied target is parsed via
// profile.NormalizeGitHubRepoInput so that e.g. "alecthomas/kong",
// "github.com/alecthomas/kong", and "https://github.com/alecthomas/kong"
// all collapse to the same canonical URI and therefore the same
// entity. This prevents duplicate-entity fragmentation (#53).
type AnalyzeCmd struct {
	Target  string `arg:"" help:"Package name, repo URL, or identity to analyze."`
	Refresh bool   `help:"Collect fresh signals from network sources." default:"false"`
	JSON    bool   `help:"Output as JSON." default:"false"`
}

func (cmd *AnalyzeCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := context.Background()
	auditLog := globals.NewAuditLogger(s)
	actor := identity.Current()

	// Normalize user input to a canonical URI. This is the one place
	// where free-form input crosses into stable internal identifiers —
	// everything downstream uses the canonical URI as the lookup key.
	canonicalURI, owner, repoName, err := profile.NormalizeGitHubRepoInput(cmd.Target)
	if err != nil {
		return fmt.Errorf("parse target %q: %w", cmd.Target, err)
	}

	// Look up an existing entity by canonical URI. A matching entity
	// means the user has analyzed this target before — we reuse its
	// UUID ID so FK references stay stable.
	entity, err := s.FindEntityByURI(ctx, canonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		entity = nil
	} else if err != nil {
		return fmt.Errorf("lookup entity: %w", err)
	}

	// Decide what to do based on cache state and --refresh.
	if !cmd.Refresh {
		if entity == nil {
			fmt.Printf("No cached data for: %s\n", cmd.Target)
			fmt.Printf("Resolved to: %s\n", canonicalURI)
			fmt.Println("Run with --refresh to collect signals from GitHub.")
			return nil
		}
		existing, err := s.GetLatestSignals(ctx, entity.ID)
		if err != nil {
			return fmt.Errorf("read cached signals: %w", err)
		}
		if len(existing) > 0 {
			return cmd.displayProfile(ctx, s, entity)
		}
		fmt.Printf("No cached signals for: %s\n", cmd.Target)
		fmt.Println("Run with --refresh to collect signals from GitHub.")
		return nil
	}

	// --- Refresh path: collect fresh signals. ---

	// Create the entity if it doesn't exist yet. UUID ID, canonical URI,
	// short_name and URL derived from the parsed input.
	created := false
	if entity == nil {
		entity = &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: canonicalURI,
			Type:         profile.EntityProject,
			ShortName:    owner + "/" + repoName,
			URL:          "https://github.com/" + owner + "/" + repoName,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		if err := s.PutEntity(ctx, entity); err != nil {
			return fmt.Errorf("create entity: %w", err)
		}
		created = true
	}

	fmt.Printf("Collecting signals for: %s\n", entity.CanonicalURI)

	var allSignals []profile.Signal
	for _, collector := range globals.Collectors {
		result, err := collector.Collect(ctx, entity)
		if err != nil {
			return fmt.Errorf("collect signals (%s): %w", collector.Name(), err)
		}
		allSignals = append(allSignals, result.Signals()...)
		fmt.Printf("[%s] %s\n", collector.Name(), result.Summary())
	}

	if err := s.AppendSignals(ctx, allSignals); err != nil {
		return fmt.Errorf("store signals: %w", err)
	}

	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}

	// Audit the analysis. Failure is non-fatal — the signals are
	// already in the store; a missing audit line is a secondary
	// observability concern, not a correctness failure.
	detail, _ := json.Marshal(map[string]interface{}{
		"target":            cmd.Target,
		"canonical_uri":     entity.CanonicalURI,
		"signals_collected": len(allSignals),
		"created_entity":    created,
	})
	if err := auditLog.LogAction(ctx, actor, "analyze", entity.ID, string(detail)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: audit log write failed: %v\n", err)
	}

	fmt.Println()
	return cmd.displayProfile(ctx, s, entity)
}

// displayProfile reads the current-state view for an entity and
// renders it to stdout. Uses GetLatestSignals so superseded signals
// are filtered out; uses GetPostures to show the latest posture plus
// a hint when multiple versions have recorded decisions.
func (cmd *AnalyzeCmd) displayProfile(ctx context.Context, s store.Store, entity *profile.Entity) error {
	signals, err := s.GetLatestSignals(ctx, entity.ID)
	if err != nil {
		return fmt.Errorf("read signals: %w", err)
	}

	postures, err := s.GetPostures(ctx, entity.ID)
	if err != nil {
		return fmt.Errorf("read postures: %w", err)
	}

	burn, err := s.GetBurn(ctx, entity.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get burn: %w", err)
	}

	p := &profile.Profile{
		Entity:   *entity,
		Signals:  signals,
		Postures: postures,
		Burn:     burn,
	}
	if len(postures) > 0 {
		// Postures are returned newest-first; highlight the latest as
		// the "current" posture for backward-compat display.
		latest := postures[0]
		p.Posture = &latest
	}

	if cmd.JSON {
		data, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	return displayHuman(p)
}

// displayHuman prints a human-readable entity profile.
func displayHuman(p *profile.Profile) error {
	fmt.Printf("Entity:    %s\n", p.Entity.ShortName)
	fmt.Printf("URI:       %s\n", p.Entity.CanonicalURI)
	fmt.Printf("Type:      %s\n", p.Entity.Type)
	if p.Entity.Description != "" {
		fmt.Printf("Note:      %s\n", p.Entity.Description)
	}
	if p.Entity.Ecosystem != "" {
		fmt.Printf("Ecosystem: %s\n", p.Entity.Ecosystem)
	}
	fmt.Println()

	// Posture: show latest + hint about other versions.
	if len(p.Postures) > 0 {
		latest := p.Postures[0]
		if latest.Version != "" {
			fmt.Printf("Posture:   %s (version %s)\n", latest.Tier, latest.Version)
		} else {
			fmt.Printf("Posture:   %s\n", latest.Tier)
		}
		fmt.Printf("Rationale: %s\n", latest.Rationale)
		fmt.Printf("Set by:    %s\n", latest.SetBy)
		if len(p.Postures) > 1 {
			fmt.Printf("           (%d other version%s recorded — `signatory posture get %s --all` to see all)\n",
				len(p.Postures)-1, pluralS(len(p.Postures)-1), p.Entity.CanonicalURI)
		}
		fmt.Println()
	}

	if p.Burn != nil {
		fmt.Printf("*** BURNED: %s (by %s, %s) ***\n",
			p.Burn.Reason, p.Burn.BurnedBy, p.Burn.BurnedAt.Format(time.RFC3339))
		fmt.Println()
	}

	// Group signals for display.
	groups := map[profile.SignalGroup][]profile.Signal{}
	for _, s := range p.Signals {
		groups[s.Group] = append(groups[s.Group], s)
	}

	groupOrder := []struct {
		group profile.SignalGroup
		label string
	}{
		{profile.SignalGroupVitality, "Vitality"},
		{profile.SignalGroupGovernance, "Governance"},
		{profile.SignalGroupPublication, "Publication Integrity"},
		{profile.SignalGroupHygiene, "Hygiene"},
		{profile.SignalGroupCriticality, "Criticality"},
		{profile.SignalGroupPosture, "Posture"},
	}

	absenceCount := 0
	for _, g := range groupOrder {
		sigs, ok := groups[g.group]
		if !ok {
			continue
		}
		fmt.Printf("=== %s ===\n", g.label)
		for _, s := range sigs {
			var val map[string]interface{}
			_ = json.Unmarshal(s.Value, &val)

			if strings.HasPrefix(s.Type, "absence:") {
				absenceCount++
				retryable := ""
				if r, ok := val["retryable"].(bool); ok && r {
					retryable = " (retryable)"
				}
				reason := ""
				if r, ok := val["reason"].(string); ok {
					reason = r
				}
				fmt.Printf("  %-20s [ABSENT]  %s%s\n",
					strings.TrimPrefix(s.Type, "absence:"), reason, retryable)
			} else {
				fmt.Printf("  %-20s [%s]  ", s.Type, s.ForgeryResistance)
				printCompactValue(val)
				fmt.Println()
			}
		}
		fmt.Println()
	}

	fmt.Printf("Total signals: %d (%d absent)\n", len(p.Signals), absenceCount)
	return nil
}

func printCompactValue(val map[string]interface{}) {
	first := true
	for k, v := range val {
		if !first {
			fmt.Print(", ")
		}
		fmt.Printf("%s=%v", k, v)
		first = false
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
