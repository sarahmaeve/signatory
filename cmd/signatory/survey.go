package main

import "fmt"

// SurveyCmd assesses the trust posture of a project's dependency tree.
//
// Status: not implemented in v0.1. Survey is the dashboard entry point
// described in design/ROADMAP.md (v0.1 must-do #1) and will parse a
// dependency manifest, list deps, show posture tiers, flag burned
// entities, and highlight unexamined dependencies. Until that lands,
// Run returns an error so LLM agents and shell callers see a non-zero
// exit instead of a misleadingly successful "Surveying..." line.
type SurveyCmd struct {
	Manifest string `help:"Path to dependency manifest (default: auto-detect)." short:"m" type:"existingfile" optional:""`
	Refresh  bool   `help:"Collect fresh signals from network sources." default:"false"`
	JSON     bool   `help:"Output as JSON." default:"false"`
}

func (cmd *SurveyCmd) Run(globals *Globals) error {
	return fmt.Errorf("survey: not implemented in v0.1, see design/ROADMAP.md")
}
