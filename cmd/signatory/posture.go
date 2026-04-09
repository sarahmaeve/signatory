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

// PostureCmd manages dependency posture tiers.
type PostureCmd struct {
	Get PostureGetCmd `cmd:"" default:"withargs" help:"View the posture for an entity."`
	Set PostureSetCmd `cmd:"" help:"Set the posture tier for an entity."`
}

// PostureGetCmd views the current posture for an entity.
type PostureGetCmd struct {
	Target string `arg:"" help:"Entity to view posture for."`
}

func (cmd *PostureGetCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	posture, err := s.GetPosture(context.Background(), cmd.Target)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Printf("No posture recorded for: %s\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Printf("Entity:    %s\n", posture.EntityID)
	fmt.Printf("Tier:      %s\n", posture.Tier)
	if posture.Version != "" {
		fmt.Printf("Version:   %s\n", posture.Version)
	}
	fmt.Printf("Rationale: %s\n", posture.Rationale)
	fmt.Printf("Set by:    %s\n", posture.SetBy)
	fmt.Printf("Set at:    %s\n", posture.SetAt.Format(time.RFC3339))
	return nil
}

// PostureSetCmd records a posture decision.
type PostureSetCmd struct {
	Target    string `arg:"" help:"Entity to set posture for."`
	Tier      string `help:"Posture tier." enum:"vetted-frozen,trusted-for-now,unexamined,unknown-provenance" required:""`
	Rationale string `help:"Rationale for the posture decision." required:""`
	Version   string `help:"Specific version being attested." optional:""`
}

func (cmd *PostureSetCmd) Run(globals *Globals) error {
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

	posture := &profile.Posture{
		EntityID:  cmd.Target,
		Tier:      profile.PostureTier(cmd.Tier),
		Version:   cmd.Version,
		Rationale: cmd.Rationale,
		SetBy:     currentUser(),
		SetAt:     time.Now().UTC(),
	}

	if err := s.SetPosture(ctx, posture); err != nil {
		return err
	}

	fmt.Printf("Posture set for %s: %s\n", cmd.Target, cmd.Tier)
	return nil
}

func currentUser() string {
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return "unknown"
}
