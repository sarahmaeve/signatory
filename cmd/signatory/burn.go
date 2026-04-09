package main

import "fmt"

// BurnCmd records a burn against an entity, degrading its trust signals.
type BurnCmd struct {
	Target string `arg:"" help:"Entity to burn."`
	Reason string `help:"Reason for the burn." required:""`
}

func (cmd *BurnCmd) Run(globals *Globals) error {
	fmt.Printf("Burning: %s (reason=%s)\n", cmd.Target, cmd.Reason)
	// TODO: wire up engine, show blast radius, confirm
	return nil
}
