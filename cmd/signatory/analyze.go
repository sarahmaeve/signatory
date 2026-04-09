package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalyzeCmd retrieves or collects the trust profile for a target.
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
	entityID := cmd.Target

	// Check for cached data.
	existing, err := s.GetSignals(ctx, entityID)
	if err != nil {
		return err
	}

	if len(existing) > 0 && !cmd.Refresh {
		return cmd.displayProfile(ctx, s, entityID)
	}

	if len(existing) == 0 && !cmd.Refresh {
		fmt.Printf("No cached data for: %s\n", cmd.Target)
		fmt.Println("Run with --refresh to collect signals from GitHub.")
		return nil
	}

	// Collect fresh signals.
	fmt.Printf("Collecting signals for: %s\n", cmd.Target)

	entity := &profile.Entity{
		ID:        entityID,
		Type:      profile.EntityProject,
		Name:      cmd.Target,
		URL:       cmd.Target,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	// Upsert entity.
	if _, err := s.GetEntity(ctx, entityID); errors.Is(err, store.ErrNotFound) {
		if err := s.PutEntity(ctx, entity); err != nil {
			return fmt.Errorf("store entity: %w", err)
		}
	}

	// Use injected collectors.
	var allSignals []profile.Signal
	for _, collector := range globals.Collectors {
		signals, err := collector.Collect(ctx, entity)
		if err != nil {
			return fmt.Errorf("collect signals (%s): %w", collector.Name(), err)
		}
		allSignals = append(allSignals, signals...)
	}

	if err := s.PutSignals(ctx, allSignals); err != nil {
		return fmt.Errorf("store signals: %w", err)
	}

	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}

	fmt.Printf("Collected %d signals.\n\n", len(allSignals))
	return cmd.displayProfile(ctx, s, entityID)
}

func (cmd *AnalyzeCmd) displayProfile(ctx context.Context, s *store.SQLite, entityID string) error {
	entity, err := s.GetEntity(ctx, entityID)
	if err != nil {
		return err
	}

	signals, err := s.GetSignals(ctx, entityID)
	if err != nil {
		return err
	}

	posture, _ := s.GetPosture(ctx, entityID)
	burn, _ := s.GetBurn(ctx, entityID)

	p := &profile.Profile{
		Entity:  *entity,
		Signals: signals,
		Posture: posture,
		Burn:    burn,
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

func displayHuman(p *profile.Profile) error {
	fmt.Printf("Entity:    %s\n", p.Entity.ID)
	fmt.Printf("Type:      %s\n", p.Entity.Type)
	if p.Entity.Ecosystem != "" {
		fmt.Printf("Ecosystem: %s\n", p.Entity.Ecosystem)
	}
	fmt.Println()

	if p.Posture != nil {
		fmt.Printf("Posture:   %s\n", p.Posture.Tier)
		fmt.Printf("Rationale: %s\n", p.Posture.Rationale)
		fmt.Println()
	}

	if p.Burn != nil {
		fmt.Printf("*** BURNED: %s (by %s, %s) ***\n",
			p.Burn.Reason, p.Burn.BurnedBy, p.Burn.BurnedAt.Format(time.RFC3339))
		fmt.Println()
	}

	// Group signals.
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
			json.Unmarshal(s.Value, &val)

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
