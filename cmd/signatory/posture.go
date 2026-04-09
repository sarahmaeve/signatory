package main

import "fmt"

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
	fmt.Printf("Viewing posture for: %s\n", cmd.Target)
	// TODO: wire up engine
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
	fmt.Printf("Setting posture for: %s (tier=%s, version=%s, rationale=%s)\n",
		cmd.Target, cmd.Tier, cmd.Version, cmd.Rationale)
	// TODO: wire up engine
	return nil
}
