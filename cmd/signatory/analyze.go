package main

import "fmt"

// AnalyzeCmd retrieves or collects the trust profile for a target.
type AnalyzeCmd struct {
	Target  string `arg:"" help:"Package name, repo URL, or identity to analyze."`
	Refresh bool   `help:"Collect fresh signals from network sources." default:"false"`
	JSON    bool   `help:"Output as JSON." default:"false"`
}

func (cmd *AnalyzeCmd) Run(globals *Globals) error {
	fmt.Printf("Analyzing: %s (refresh=%v, db=%s)\n", cmd.Target, cmd.Refresh, globals.DB)
	// TODO: wire up engine
	return nil
}
