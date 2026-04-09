package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// BurnCmd manages entity burns.
type BurnCmd struct {
	Add  BurnAddCmd  `cmd:"" default:"withargs" help:"Burn an entity, degrading its trust signals."`
	List BurnListCmd `cmd:"" help:"List all active burns."`
}

// BurnAddCmd records a burn against an entity.
type BurnAddCmd struct {
	Target string `arg:"" help:"Entity to burn."`
	Reason string `help:"Reason for the burn." required:""`
}

func (cmd *BurnAddCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := context.Background()

	// Ensure the entity exists (create a stub if not).
	_, err = s.GetEntity(ctx, cmd.Target)
	if errors.Is(err, store.ErrNotFound) {
		entity := &profile.Entity{
			ID:        cmd.Target,
			Type:      profile.EntityPackage,
			Name:      cmd.Target,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		if err := s.PutEntity(ctx, entity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	// Check for existing burn.
	existing, err := s.GetBurn(ctx, cmd.Target)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("check existing burn: %w", err)
	}
	if err == nil {
		fmt.Fprintf(os.Stderr, "Warning: %s is already burned (reason: %s, by: %s, at: %s)\n",
			cmd.Target, existing.Reason, existing.BurnedBy, existing.BurnedAt.Format(time.RFC3339))
		fmt.Fprintf(os.Stderr, "Overwriting with new burn.\n")
	}

	burn := &profile.Burn{
		EntityID: cmd.Target,
		Reason:   cmd.Reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: currentUser(),
	}

	if err := s.SetBurn(ctx, burn); err != nil {
		return err
	}

	fmt.Printf("Burned: %s\n", cmd.Target)
	fmt.Printf("Reason: %s\n", cmd.Reason)
	fmt.Printf("By:     %s\n", burn.BurnedBy)
	fmt.Printf("At:     %s\n", burn.BurnedAt.Format(time.RFC3339))
	return nil
}

// BurnListCmd lists all active burns.
type BurnListCmd struct{}

func (cmd *BurnListCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	burns, err := s.ListBurns(context.Background())
	if err != nil {
		return err
	}

	if len(burns) == 0 {
		fmt.Println("No active burns.")
		return nil
	}

	for _, b := range burns {
		fmt.Printf("%-30s  %-12s  %-10s  %s\n",
			b.EntityID, b.Source, b.BurnedBy, b.Reason)
	}
	return nil
}
