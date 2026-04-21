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

// BurnCmd manages entity burns. Burns are a one-per-entity decision
// (not versioned like postures) because a compromised maintainer
// compromises the entity's identity, not a specific version.
type BurnCmd struct {
	Add  BurnAddCmd  `cmd:"" default:"withargs" help:"Burn an entity, degrading its trust signals."`
	List BurnListCmd `cmd:"" help:"List all active burns."`
}

// BurnAddCmd records a burn against an entity.
//
// Reason may be supplied via --reason (one-line) or --reason-file
// (multi-line, path or "-" for stdin). Exactly one must be non-empty.
// See agent-facing-contract §3.4.
type BurnAddCmd struct {
	Target     string `arg:"" help:"Entity to burn."`
	Reason     string `help:"Reason for the burn (one-line). For multi-line reasons use --reason-file."`
	ReasonFile string `name:"reason-file" help:"Path to a file containing the burn reason (or '-' for stdin)."`
}

func (cmd *BurnAddCmd) Run(globals *Globals) error {
	ctx := context.Background()

	// Resolve --reason / --reason-file early so a missing/malformed
	// source fails before we open the store (§3.4).
	reason, err := readFreeText("reason", cmd.Reason, cmd.ReasonFile)
	if err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("burn add: --reason or --reason-file is required (an empty reason isn't a burn)")
	}
	cmd.Reason = reason

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

	entity, err := ensureEntity(ctx, s, cmd.Target)
	if err != nil {
		return err
	}

	// Check for existing burn so we can warn the user we're overwriting.
	// Use a distinctively-named variable for the GetBurn error so the
	// "did a prior burn exist?" boolean below can't be silently broken
	// by a future refactor that shadows or reassigns `err` (#92).
	existing, getBurnErr := s.GetBurn(ctx, entity.ID)
	if getBurnErr != nil && !errors.Is(getBurnErr, store.ErrNotFound) {
		return fmt.Errorf("check existing burn: %w", getBurnErr)
	}
	// Capture the meaning at the moment it's computed, in a boolean
	// that can't be confused with any later `err`. Used below in the
	// audit detail.
	overwriting := getBurnErr == nil
	if overwriting {
		fmt.Fprintf(os.Stderr, "Warning: %s is already burned (reason: %s, by: %s, at: %s)\n",
			entity.ShortName, existing.Reason, existing.BurnedBy, existing.BurnedAt.Format(time.RFC3339))
		fmt.Fprintln(os.Stderr, "Overwriting with new burn.")
	}

	burn := &profile.Burn{
		EntityID: entity.ID,
		Reason:   cmd.Reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: actor,
	}

	if err := s.SetBurn(ctx, burn); err != nil {
		return err
	}

	// Audit. The `overwriting` boolean was captured immediately after
	// the GetBurn call above; it cannot be affected by intervening code.
	detail, _ := json.Marshal(map[string]interface{}{
		"canonical_uri": entity.CanonicalURI,
		"reason":        cmd.Reason,
		"overwrite":     overwriting,
	})
	if err := auditLog.LogAction(ctx, actor, "burn", entity.ID, string(detail)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: audit log write failed: %v\n", err)
	}

	fmt.Printf("Burned: %s\n", entity.ShortName)
	fmt.Printf("URI:    %s\n", entity.CanonicalURI)
	fmt.Printf("Reason: %s\n", cmd.Reason)
	fmt.Printf("By:     %s\n", burn.BurnedBy)
	fmt.Printf("At:     %s\n", burn.BurnedAt.Format(time.RFC3339))
	return nil
}

// BurnListCmd lists all active burns.
type BurnListCmd struct{}

func (cmd *BurnListCmd) Run(globals *Globals) error {
	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	burns, err := s.ListBurns(ctx)
	if err != nil {
		return err
	}

	if len(burns) == 0 {
		fmt.Println("No active burns.")
		return nil
	}

	// Look up each entity so we can show ShortName and CanonicalURI
	// rather than the opaque UUID.
	for _, b := range burns {
		entity, err := s.GetEntity(ctx, b.EntityID)
		name := b.EntityID
		uri := ""
		if err == nil && entity != nil {
			name = entity.ShortName
			uri = entity.CanonicalURI
		}
		fmt.Printf("%-30s  %-40s  %-12s  %s\n",
			name, uri, b.Source, b.Reason)
	}
	return nil
}
