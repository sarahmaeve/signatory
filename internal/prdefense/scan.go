// Package prdefense runs signatory's content-level deep scanners over
// the files a single pull request changes — a pre-merge supply-chain
// gate. It does not touch git or the network: it consumes a changelist
// (paths + statuses) and a ContentProvider that yields each changed
// file's bytes at the PR head, then composes the existing scanners
// (content-injection, exfil-host, agent-config taxonomy, source-AST
// concern) scoped to just those files and derives a block/warn/clear
// verdict.
package prdefense

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"

	"github.com/sarahmaeve/pr-analyzer/codeshape"

	"github.com/sarahmaeve/signatory/internal/agentconfig"
	"github.com/sarahmaeve/signatory/internal/contentinjection"
	"github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
	"github.com/sarahmaeve/signatory/internal/signal/source"
	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
	"github.com/sarahmaeve/signatory/internal/signal/source/node"
	"github.com/sarahmaeve/signatory/internal/signal/source/python"
	"github.com/sarahmaeve/signatory/internal/signal/source/rust"
)

// SkipReason explains why a changed file was not content-scanned.
type SkipReason string

const (
	SkipNone     SkipReason = ""
	SkipRemoved  SkipReason = "removed"   // status removed — nothing at head
	SkipBinary   SkipReason = "binary"    // NUL byte in the sniff window
	SkipTooLarge SkipReason = "oversized" // exceeded the provider's byte cap
	SkipMissing  SkipReason = "missing"   // not present at the head commit
)

// ChangedFile is one entry of a PR's changelist.
type ChangedFile struct {
	Path   string
	Status string // GitHub file status: added/modified/removed/renamed/...
}

// ContentProvider yields a changed file's bytes at the PR head. The
// production implementation reads blobs by SHA from a git object DB
// (no working-tree checkout); tests inject an in-memory fake. A non-
// empty SkipReason means the bytes were intentionally not returned
// (oversized, missing); content is nil in that case.
type ContentProvider interface {
	ReadFile(ctx context.Context, path string) (content []byte, skip SkipReason)
}

// Verdict is the pre-merge recommendation.
type Verdict string

const (
	VerdictClear Verdict = "clear"
	VerdictWarn  Verdict = "warn"
	VerdictBlock Verdict = "block"
)

// FileInjection is a content-injection finding for one changed file.
type FileInjection struct {
	Path          string                      `json:"path"`
	IsAgentConfig bool                        `json:"is_agent_config"`
	Result        contentinjection.ScanResult `json:"result"`
}

// LanguageConcern is the AST-concern result for one language bucket.
type LanguageConcern struct {
	Language string              `json:"language"`
	Concern  source.ConcernValue `json:"concern"`
}

// SkippedFile records a changed file that was not content-scanned.
type SkippedFile struct {
	Path   string     `json:"path"`
	Reason SkipReason `json:"reason"`
}

// Report is the outcome of scanning a PR's changelist.
type Report struct {
	HeadSHA            string            `json:"head_sha"`
	Verdict            Verdict           `json:"verdict"`
	Reasons            []string          `json:"reasons,omitempty"`
	ContentInjection   []FileInjection   `json:"content_injection,omitempty"`
	ExfilHits          []exfilwatch.Hit  `json:"exfil_hits,omitempty"`
	AgentConfigPaths   []string          `json:"agent_config_paths,omitempty"`
	ASTConcerns        []LanguageConcern `json:"ast_concerns,omitempty"`
	RiskyPathHits      []string          `json:"risky_path_hits,omitempty"`     // changed paths matching an org-configured risky prefix
	AnomalousLanguages []string          `json:"anomalous_languages,omitempty"` // programming languages in the PR outside the org's preferred/allowed set
	Scanned            int               `json:"scanned"`
	Skipped            []SkippedFile     `json:"skipped,omitempty"`
}

// Option configures a Scan. Options carry org policy (from a shared
// pr-analyzer.yaml) into the otherwise self-contained scan.
type Option func(*scanConfig)

type scanConfig struct {
	riskyPaths []string
	langPolicy codeshape.LanguageConfig
}

// WithRiskyPaths supplies the org-configured risky-path prefixes (from a
// shared pr-analyzer.yaml's codeshape.risky_paths). A changed file under
// one of them is flagged in Report.RiskyPathHits and warned, so an org is
// told when a PR touches a sensitive area — independent of file content.
// Matching uses pr-analyzer's codeshape.MatchesRiskyPath so the rule
// stays identical across the two tools.
func WithRiskyPaths(prefixes []string) Option {
	return func(c *scanConfig) { c.riskyPaths = prefixes }
}

// WithLanguagePolicy supplies the org's acceptable/not-acceptable language
// weighting (a shared pr-analyzer.yaml's codeshape.languages). A
// programming language present in the PR but in neither the preferred nor
// the allowed list is "anomalous" — flagged in Report.AnomalousLanguages
// and warned. Detection + bucketing use pr-analyzer's DetectLanguages /
// BucketLanguages, so the rule (including the markup exclusion) is
// identical across the two tools. Zero-value policy = no opinion.
func WithLanguagePolicy(cfg codeshape.LanguageConfig) Option {
	return func(c *scanConfig) { c.langPolicy = cfg }
}

// binarySniffWindow bounds the prefix inspected for a NUL byte.
const binarySniffWindow = 8192

// Scan runs the content-level scanners over the changelist and derives a
// verdict. Removed files are dropped before any read; binary and
// oversized files are recorded as skipped, not scanned. A per-file or
// per-language analyzer error is non-fatal — it never aborts the scan,
// since a partial result is still a useful gate.
func Scan(ctx context.Context, src ContentProvider, headSHA string, changed []ChangedFile, opts ...Option) (Report, error) {
	var cfg scanConfig
	for _, o := range opts {
		o(&cfg)
	}

	rep := Report{HeadSHA: headSHA}
	langFiles := map[string][]astfeature.SourceFile{}

	// Org-policy risky-path touch — independent of content and of whether
	// the file is read below. Touching a sensitive area (added / modified /
	// removed) is the signal, so this pass runs over the full changelist.
	for _, cf := range changed {
		if codeshape.MatchesRiskyPath(cf.Path, cfg.riskyPaths) {
			rep.RiskyPathHits = append(rep.RiskyPathHits, cf.Path)
		}
	}

	// Org-policy language weighting — path-based, over the full changelist.
	// A programming language in the PR but outside preferred/allowed is
	// anomalous (markup is excluded by BucketLanguages). Reuses
	// pr-analyzer's detection + bucketing so the verdict matches the
	// overview's posture for the same shared config.
	if len(cfg.langPolicy.Preferred) > 0 || len(cfg.langPolicy.Allowed) > 0 {
		files := make([]codeshape.File, len(changed))
		for i, cf := range changed {
			files[i] = codeshape.File{Path: cf.Path}
		}
		posture := codeshape.BucketLanguages(codeshape.DetectLanguages(files), cfg.langPolicy)
		rep.AnomalousLanguages = posture.Anomalous
	}

	for _, cf := range changed {
		if isRemovedStatus(cf.Status) {
			rep.Skipped = append(rep.Skipped, SkippedFile{Path: cf.Path, Reason: SkipRemoved})
			continue
		}

		// Agent-config classification is path-only — record it even if the
		// content read below is skipped, so a touched-but-unreadable
		// agent-config file still surfaces.
		isAgentCfg := agentconfig.IsConfigPath(cf.Path)
		if isAgentCfg {
			rep.AgentConfigPaths = append(rep.AgentConfigPaths, cf.Path)
		}

		content, skip := src.ReadFile(ctx, cf.Path)
		if skip != SkipNone {
			rep.Skipped = append(rep.Skipped, SkippedFile{Path: cf.Path, Reason: skip})
			continue
		}
		if isBinary(content) {
			rep.Skipped = append(rep.Skipped, SkippedFile{Path: cf.Path, Reason: SkipBinary})
			continue
		}
		rep.Scanned++

		// Content injection. The markdown-comment primitive is suppressed
		// on agent-config files (imperative prose is expected there).
		var opts contentinjection.ScanOptions
		if isAgentCfg {
			opts.SuppressPrimitives = []contentinjection.Primitive{contentinjection.PrimitiveMarkdownComment}
		}
		if ci := contentinjection.ScanWithOptions(content, opts); ci.HasFindings() {
			rep.ContentInjection = append(rep.ContentInjection, FileInjection{
				Path:          cf.Path,
				IsAgentConfig: isAgentCfg,
				Result:        ci,
			})
		}

		// Exfil-host references.
		rep.ExfilHits = append(rep.ExfilHits, exfilwatch.ScanBytes(cf.Path, content)...)

		// Bucket source files for per-language AST analysis. PR-defense
		// uses LanguageForChangedFile (not LanguageForPath): a changed
		// test file is authored code an attacker abuses (prt-scan's
		// conftest.py), so test files are scanned here even though the
		// source-evolution baseline excludes them.
		if lang, ok := astfeature.LanguageForChangedFile(cf.Path); ok {
			langFiles[lang] = append(langFiles[lang], astfeature.SourceFile{Path: cf.Path, Content: content})
		}
	}

	// AST concern per language bucket (single-checkout; no anomaly).
	for _, lang := range sortedKeys(langFiles) {
		counts, err := analyzeLanguage(ctx, lang, langFiles[lang])
		if err != nil {
			continue // analyzer failure on a bucket is non-fatal
		}
		concern := source.DetectConcern([]source.MatrixRow{{Version: headSHA, AST: &counts}})
		if concern.ConcernPresent {
			rep.ASTConcerns = append(rep.ASTConcerns, LanguageConcern{Language: lang, Concern: concern})
		}
	}

	rep.Verdict, rep.Reasons = deriveVerdict(rep)
	return rep, nil
}

// analyzeLanguage routes a language bucket to its analyzer and returns
// the aggregated AST counts.
func analyzeLanguage(ctx context.Context, lang string, files []astfeature.SourceFile) (astfeature.Counts, error) {
	var a source.LanguageAnalyzer
	switch lang {
	case astfeature.LangGo:
		a = golang.NewAnalyzer()
	case astfeature.LangPython:
		a = python.NewAnalyzer()
	case astfeature.LangJavaScript:
		a = node.NewAnalyzer()
	case astfeature.LangRust:
		a = rust.NewAnalyzer()
	default:
		return astfeature.Counts{}, fmt.Errorf("prdefense: no analyzer for language %q", lang)
	}
	return a.Analyze(ctx, sliceSeq(files))
}

// deriveVerdict reduces the findings to a verdict + human reasons.
//
//   - block: any exfil hit, OR content injection in an agent-config
//     file, OR an AST concern.
//   - warn:  content injection outside agent-config, OR an agent-config
//     file touched without an injection hit.
//   - clear: nothing fired.
func deriveVerdict(rep Report) (Verdict, []string) {
	var blockReasons, warnReasons []string

	if n := len(rep.ExfilHits); n > 0 {
		blockReasons = append(blockReasons, fmt.Sprintf("%d exfil-host reference(s)", n))
	}

	var agentInjections, otherInjections int
	for _, fi := range rep.ContentInjection {
		if fi.IsAgentConfig {
			agentInjections++
		} else {
			otherInjections++
		}
	}
	if agentInjections > 0 {
		blockReasons = append(blockReasons, fmt.Sprintf("content injection in %d agent-config file(s)", agentInjections))
	}
	if len(rep.ASTConcerns) > 0 {
		langs := make([]string, len(rep.ASTConcerns))
		for i, c := range rep.ASTConcerns {
			langs[i] = c.Language
		}
		blockReasons = append(blockReasons, "AST concern in "+strings.Join(langs, ", "))
	}

	if len(blockReasons) > 0 {
		return VerdictBlock, blockReasons
	}

	if otherInjections > 0 {
		warnReasons = append(warnReasons, fmt.Sprintf("content injection in %d file(s)", otherInjections))
	}
	if len(rep.AgentConfigPaths) > 0 {
		warnReasons = append(warnReasons, fmt.Sprintf("%d agent-config file(s) touched", len(rep.AgentConfigPaths)))
	}
	if len(rep.RiskyPathHits) > 0 {
		warnReasons = append(warnReasons, fmt.Sprintf("%d org-defined sensitive path(s) modified", len(rep.RiskyPathHits)))
	}
	if len(rep.AnomalousLanguages) > 0 {
		warnReasons = append(warnReasons, fmt.Sprintf("non-acceptable language(s): %s", strings.Join(rep.AnomalousLanguages, ", ")))
	}
	if len(warnReasons) > 0 {
		return VerdictWarn, warnReasons
	}
	return VerdictClear, nil
}

func isRemovedStatus(status string) bool {
	return strings.EqualFold(status, "removed")
}

func isBinary(content []byte) bool {
	window := content
	if len(window) > binarySniffWindow {
		window = window[:binarySniffWindow]
	}
	return bytes.IndexByte(window, 0) >= 0
}

func sliceSeq(files []astfeature.SourceFile) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		for _, f := range files {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func sortedKeys(m map[string][]astfeature.SourceFile) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
