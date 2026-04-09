package main

import "fmt"

// SurveyCmd assesses the trust posture of a project's dependency tree.
type SurveyCmd struct {
	Manifest string `help:"Path to dependency manifest (default: auto-detect)." short:"m" type:"existingfile" optional:""`
	Refresh  bool   `help:"Collect fresh signals from network sources." default:"false"`
	JSON     bool   `help:"Output as JSON." default:"false"`
}

func (cmd *SurveyCmd) Run(globals *Globals) error {
	manifest := cmd.Manifest
	if manifest == "" {
		manifest = "(auto-detect)"
	}
	fmt.Printf("Surveying dependencies (manifest=%s, refresh=%v)\n", manifest, cmd.Refresh)
	// TODO: wire up engine
	return nil
}
