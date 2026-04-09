package main

import "fmt"

// CompareCmd compares trust profiles of two packages or repos side by side.
type CompareCmd struct {
	TargetA string `arg:"" help:"First package or repo."`
	TargetB string `arg:"" help:"Second package or repo."`
	JSON    bool   `help:"Output as JSON." default:"false"`
}

func (cmd *CompareCmd) Run(globals *Globals) error {
	fmt.Printf("Comparing: %s vs %s\n", cmd.TargetA, cmd.TargetB)
	// TODO: wire up engine
	return nil
}
