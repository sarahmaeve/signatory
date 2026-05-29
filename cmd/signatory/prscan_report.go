package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	rhtml "github.com/sarahmaeve/pr-analyzer/render/html"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// PRScanReportCmd writes the deep-findings SIDECAR that enriches
// pr-analyzer's repo PR overview. It does NOT touch index.html: it emits
// a `pr-scan.js` data file next to it, which pr-analyzer's page loads
// out-of-band (via <script src>, so it works from file://) and folds into
// the matching drill-downs at view time. The overview keeps showing ALL
// of the repo's PRs exactly as pr-analyzer rendered them; the sidecar
// only adds deep findings to the drill-downs of PRs signatory scanned.
//
// pr-scan is expensive and storage-heavy, so not every project scans
// every PR — the sidecar is sparse by design, and a PR with no stored
// scan simply shows pr-analyzer's mechanistic overview, untouched.
//
// signatory writes only structured data (via pr-analyzer's SidecarJS) —
// never HTML — so the deep sections match the overview's look, and a
// restyle in pr-analyzer applies automatically.
type PRScanReportCmd struct {
	Repo string `arg:"" help:"Repository whose pr-analyzer report to enrich: owner/repo (the same target you ran pr-analyzer against)."`
	Out  string `name:"out" type:"path" default:"." help:"Directory holding pr-analyzer's index.html; the pr-scan.js sidecar is written here. Created if missing."`

	Stdout io.Writer `kong:"-"`
}

func (cmd *PRScanReportCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	resolved, err := profile.ResolveTarget(cmd.Repo)
	if err != nil {
		return NewUsageError(fmt.Errorf("resolve %q: %w", cmd.Repo, err))
	}
	if resolved.Scheme != "repo" || resolved.Platform != profile.PlatformGitHub {
		return NewUsageError(fmt.Errorf("pr-scan report target must be a GitHub repo (owner/repo); %q resolved to %s",
			cmd.Repo, resolved.CanonicalURI))
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	enrich, scanned, err := repoEnrichment(ctx, s, resolved.Owner, resolved.ShortName)
	if err != nil {
		return err
	}

	js, err := rhtml.SidecarJS(enrich)
	if err != nil {
		return fmt.Errorf("build sidecar: %w", err)
	}
	if err := os.MkdirAll(cmd.Out, 0o755); err != nil { //nolint:gosec // G301: report dir is intended to be readable
		return fmt.Errorf("create out dir %q: %w", cmd.Out, err)
	}
	sidecarPath := filepath.Join(cmd.Out, rhtml.SidecarFilename)
	if err := os.WriteFile(sidecarPath, js, 0o644); err != nil { //nolint:gosec // G306: sidecar is a world-readable report artifact
		return fmt.Errorf("write sidecar %q: %w", sidecarPath, err)
	}

	_, _ = fmt.Fprintf(stdout,
		"wrote %s — %d scanned PR(s) will enrich %s\n",
		sidecarPath, scanned, filepath.Join(cmd.Out, "index.html"))
	return nil
}

// repoEnrichment builds the drill-down enrichment for every PR of
// owner/repo that signatory has scanned, keyed by PR number. Returns the
// enrichment and the count of PRs that contributed at least one section.
func repoEnrichment(ctx context.Context, s store.Store, owner, repo string) (rhtml.Enrichment, int, error) {
	patches, err := s.ListEntitiesByType(ctx, profile.EntityPatch)
	if err != nil {
		return nil, 0, err
	}
	prefix := profile.CanonicalPatchURI("github", owner, repo, "") // patch:github/owner/repo/
	enrich := rhtml.Enrichment{}
	for _, e := range patches {
		if !strings.HasPrefix(e.CanonicalURI, prefix) {
			continue
		}
		num, err := strconv.Atoi(strings.TrimPrefix(e.CanonicalURI, prefix))
		if err != nil {
			continue // non-numeric tail — not a PR patch we recognize
		}
		ds, err := loadDeepScan(ctx, s, e.ID)
		if err != nil {
			return nil, 0, err
		}
		if secs := ds.sections(); len(secs) > 0 {
			enrich[num] = secs
		}
	}
	return enrich, len(enrich), nil
}

// deepScan is the signatory-side view of one PR's stored pr-scan
// findings, flattened to strings so the section mapper stays pure and
// free of prdefense's types.
type deepScan struct {
	Verdict          string
	Reasons          []string
	ExfilHosts       []string
	InjectionFiles   []string
	AgentConfigPaths []string
	ASTConcernLangs  []string
	RiskyPaths       []string
	AnomalousLangs   []string
	Burned           bool
	BurnVia          string
	BurnReason       string
}

// loadDeepScan decodes a patch entity's stored pr-scan signals plus its
// live effective-burn into a deepScan. The detail signals are decoded
// against local shapes (not prdefense types) so this stays decoupled
// from the scanner's structs.
func loadDeepScan(ctx context.Context, s store.Store, entityID string) (deepScan, error) {
	sigs, err := s.GetLatestSignals(ctx, entityID)
	if err != nil {
		return deepScan{}, err
	}
	var ds deepScan
	for _, sg := range sigs {
		switch sg.Type {
		case "pr_defense_verdict":
			var rec verdictRecord
			if json.Unmarshal(sg.Value, &rec) == nil {
				ds.Verdict = string(rec.Verdict)
				ds.Reasons = rec.Reasons
			}
		case "pr_exfil_host_reference":
			var v struct {
				Hits []struct {
					File string `json:"file"`
					Line int    `json:"line"`
					Host string `json:"host"`
				} `json:"hits"`
			}
			if json.Unmarshal(sg.Value, &v) == nil {
				for _, h := range v.Hits {
					ds.ExfilHosts = append(ds.ExfilHosts, fmt.Sprintf("%s (%s:%d)", h.Host, h.File, h.Line))
				}
			}
		case "pr_content_injection":
			var v struct {
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			}
			if json.Unmarshal(sg.Value, &v) == nil {
				for _, f := range v.Files {
					ds.InjectionFiles = append(ds.InjectionFiles, f.Path)
				}
			}
		case "pr_agent_config_touched":
			var v struct {
				Paths []string `json:"paths"`
			}
			if json.Unmarshal(sg.Value, &v) == nil {
				ds.AgentConfigPaths = v.Paths
			}
		case "pr_risky_path_touched":
			var v struct {
				Paths []string `json:"paths"`
			}
			if json.Unmarshal(sg.Value, &v) == nil {
				ds.RiskyPaths = v.Paths
			}
		case "pr_anomalous_language":
			var v struct {
				Languages []string `json:"languages"`
			}
			if json.Unmarshal(sg.Value, &v) == nil {
				ds.AnomalousLangs = v.Languages
			}
		case "pr_ast_concern":
			var v struct {
				Languages []struct {
					Language string `json:"language"`
				} `json:"languages"`
			}
			if json.Unmarshal(sg.Value, &v) == nil {
				for _, l := range v.Languages {
					ds.ASTConcernLangs = append(ds.ASTConcernLangs, l.Language)
				}
			}
		}
	}
	if burn, ebCtx, berr := s.EffectiveBurn(ctx, entityID); berr == nil {
		ds.Burned = true
		ds.BurnReason = burn.Reason
		ds.BurnVia = burnViaLabel(ebCtx)
	} else if !errors.Is(berr, store.ErrNotFound) {
		return deepScan{}, berr
	}
	return ds, nil
}

// sections renders a scanned PR's deep findings as pr-analyzer drill-down
// enrichment: a section with a verdict pill (and an AUTHOR BURNED danger
// pill when the author is burned) plus a labelled row per finding
// category. Returns nil when there is nothing to surface.
func (d deepScan) sections() []rhtml.Section {
	if d.Verdict == "" && !d.Burned {
		return nil
	}

	var pills []rhtml.Pill
	switch d.Verdict {
	case "block":
		pills = append(pills, rhtml.Pill{Text: "BLOCK", Tier: "danger"})
	case "warn":
		pills = append(pills, rhtml.Pill{Text: "WARN", Tier: "warning"})
	case "clear":
		pills = append(pills, rhtml.Pill{Text: "CLEAR", Tier: "success"})
	}
	if d.Burned {
		pills = append(pills, rhtml.Pill{Text: "AUTHOR BURNED", Tier: "danger"})
	}
	if len(d.RiskyPaths) > 0 {
		pills = append(pills, rhtml.Pill{Text: "SENSITIVE PATH", Tier: "warning"})
	}
	if len(d.AnomalousLangs) > 0 {
		pills = append(pills, rhtml.Pill{Text: "ANOMALOUS LANG", Tier: "warning"})
	}

	var rows []rhtml.Row
	if d.Burned {
		detail := d.BurnReason
		if d.BurnVia != "" {
			detail = fmt.Sprintf("%s (via %s)", d.BurnReason, d.BurnVia)
		}
		rows = append(rows, rhtml.Row{Term: "Author burned", Detail: detail})
	}
	for _, r := range d.Reasons {
		rows = append(rows, rhtml.Row{Term: "Verdict reason", Detail: r})
	}
	if len(d.ExfilHosts) > 0 {
		rows = append(rows, rhtml.Row{Term: "Exfil hosts", Detail: strings.Join(d.ExfilHosts, ", ")})
	}
	if len(d.InjectionFiles) > 0 {
		rows = append(rows, rhtml.Row{Term: "Injected files", Detail: strings.Join(d.InjectionFiles, ", ")})
	}
	if len(d.AgentConfigPaths) > 0 {
		rows = append(rows, rhtml.Row{Term: "Agent-config touched", Detail: strings.Join(d.AgentConfigPaths, ", ")})
	}
	if len(d.ASTConcernLangs) > 0 {
		rows = append(rows, rhtml.Row{Term: "AST concern", Detail: strings.Join(d.ASTConcernLangs, ", ")})
	}
	if len(d.RiskyPaths) > 0 {
		rows = append(rows, rhtml.Row{Term: "Sensitive paths", Detail: strings.Join(d.RiskyPaths, ", ")})
	}
	if len(d.AnomalousLangs) > 0 {
		rows = append(rows, rhtml.Row{Term: "Anomalous languages", Detail: strings.Join(d.AnomalousLangs, ", ")})
	}

	return []rhtml.Section{{Title: "pr-scan deep findings", Pills: pills, Rows: rows}}
}
