package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Minimal OTLP-JSON shape — only the fields the report reads.
// Unknown fields decode silently (json.Unmarshal default), so the
// receiver can keep writing the full body verbatim while the
// reader stays narrow.
type otlpTraceBatch struct {
	ResourceSpans []otlpResourceSpan `json:"resourceSpans"`
}

type otlpResourceSpan struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpScopeSpan struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	Name              string   `json:"name"`
	StartTimeUnixNano string   `json:"startTimeUnixNano"`
	EndTimeUnixNano   string   `json:"endTimeUnixNano"`
	Attributes        []otlpKV `json:"attributes"`
}

// otlpKV is OTLP's attribute encoding: each attribute is a
// {key, value} pair, value being a typed wrapper. We only read
// stringValue today; the report doesn't need numeric or array
// attribute types.
type otlpKV struct {
	Key   string      `json:"key"`
	Value otlpKVValue `json:"value"`
}

// otlpKVValue covers the small set of OTLP attribute-value types
// the report reads. The proto3-canonical OTLP-JSON encoding writes
// int64 as a decimal STRING under `intValue` (to preserve precision
// past 2^53), but in practice some producers emit it as a JSON
// NUMBER instead. Storing the raw JSON bytes here lets stringAttr
// strip-quotes-if-string-else-return-bytes handle both shapes
// uniformly. Pre-fix (when this was a plain `string`), the
// number-form failed to decode and broke 651 of 658 batches in
// real Claude Code traces.
type otlpKVValue struct {
	StringValue string          `json:"stringValue,omitempty"`
	IntValue    json.RawMessage `json:"intValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
}

// stringAttr returns a value's string form for the named key, or
// "" if not present. Coerces across OTLP's typed value
// representations:
//
//   - stringValue → returned verbatim.
//   - intValue (whether JSON-encoded as a string `"1234"` or as a
//     bare number `1234`) → returned as decimal string, suitable
//     for parseStringInt.
//   - boolValue → "true" or "false".
//
// Coercion to a single string lets the report's downstream parsers
// stay shape-agnostic — every numeric attribute reads the same way
// regardless of which OTLP type the producer chose, and either
// JSON encoding the producer used.
func stringAttr(attrs []otlpKV, key string) string {
	for _, a := range attrs {
		if a.Key != key {
			continue
		}
		if a.Value.StringValue != "" {
			return a.Value.StringValue
		}
		if len(a.Value.IntValue) > 0 {
			// json.RawMessage holds the literal bytes. For string
			// form (`"1234"`) we trim the surrounding quotes; for
			// number form (`1234`) the bytes are already the
			// decimal representation.
			s := strings.Trim(string(a.Value.IntValue), `"`)
			return s
		}
		if a.Value.BoolValue != nil {
			if *a.Value.BoolValue {
				return "true"
			}
			return "false"
		}
		return ""
	}
	return ""
}

// modelEconomics aggregates token + duration totals per LLM model.
// Driven entirely by `claude_code.llm_request` spans' attributes.
//
// Cache token semantics (per Anthropic's prompt-caching docs):
//
//   - InputTokens         — non-cached input tokens (paid full price)
//   - CacheCreationTokens — input tokens written to the cache
//     (also paid full price; turn N pays so
//     turns N+1..N+TTL get the cache_read price)
//   - CacheReadTokens     — input tokens served from the cache
//     (charged at ~10% of normal input rate)
//   - OutputTokens        — generated output tokens
//
// The "cache hit ratio" we render is CacheRead / (InputTokens +
// CacheCreation + CacheRead) — the fraction of input-side tokens
// that the cache absorbed.
type modelEconomics struct {
	Calls               int
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalDurationMs     int64

	// TTFTSamples is the full distribution of time-to-first-token
	// values, one entry per span where ttft_ms parsed cleanly.
	// Stored verbatim so the renderer can compute p50 (median),
	// p95 (tail), or any other quantile at output time. Mean is
	// no longer tracked — for skewed latency distributions, mean
	// is a poor summary; the median + tail tell the story the
	// operator actually cares about. See renderLLMEconomics for
	// the percentile algorithm.
	TTFTSamples []int64
}

// aggregated carries everything the markdown renderer needs.
// Built once from disk inputs, consumed by render().
type aggregated struct {
	SessionID string

	// Trace-derived counts. Populated by loadTraces.
	InteractionCount      int
	LLMRequestCount       int
	ToolCallCount         int
	ToolNameCounts        map[string]int // by `tool_name` attribute on claude_code.tool spans
	SubagentDispatchTypes map[string]int // by `subagent_type` attribute on claude_code.tool spans
	EconomicsByModel      map[string]*modelEconomics

	// Hook-derived. Populated by loadHooks.
	ClassificationCounts map[string]int
	ExternalCalls        []hookEvent
	SourceReads          []hookEvent

	HasHookData  bool
	HasTraceData bool
}

// runReport reads OTLP-JSON traces and hook events from inDir,
// filters everything to sessionID, aggregates, and writes the
// rendered markdown report to outDir/<sessionID>/report.md.
//
// Returns an error if both the trace stream AND the hook file
// produce zero session-matched records — that's the
// "session-id-doesn't-exist" case worth surfacing loudly so a
// typo doesn't silently render an empty report.
func runReport(sessionID, inDir, outDir string) error {
	agg := &aggregated{
		SessionID:             sessionID,
		ToolNameCounts:        map[string]int{},
		SubagentDispatchTypes: map[string]int{},
		EconomicsByModel:      map[string]*modelEconomics{},
		ClassificationCounts:  map[string]int{},
	}

	if err := loadTraces(filepath.Join(inDir, "traces.jsonl"), sessionID, agg); err != nil {
		return fmt.Errorf("load traces: %w", err)
	}
	if err := loadHooks(filepath.Join(inDir, "hooks-"+sessionID+".jsonl"), agg); err != nil {
		return fmt.Errorf("load hooks: %w", err)
	}

	if !agg.HasTraceData && !agg.HasHookData {
		return fmt.Errorf("no traces or hook events found for session %q", sessionID)
	}

	sessionDir := filepath.Join(outDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", sessionDir, err)
	}
	reportPath := filepath.Join(sessionDir, "report.md")
	if err := os.WriteFile(reportPath, []byte(render(agg)), 0o644); err != nil { //nolint:gosec // G306: dogfood-metrics report is dev tooling, not production data
		return fmt.Errorf("write %s: %w", reportPath, err)
	}
	return nil
}

// loadTraces streams the traces.jsonl file line by line. Each
// line is one ExportTraceServiceRequest (OTLP/HTTP/JSON spec).
//
// Filters spans for sessionID by checking BOTH the resource-level
// `session.id` attribute and the SPAN-level `session.id` attribute.
// The span-level form is what current Claude Code OTEL emits
// (verified 2026-04-28 via dogfood-metrics inspect — see commit
// 83248f8); the resource-level fallback exists because some older
// SDK builds and the original v0.1 fixtures placed it there.
//
// Span-name dispatch:
//
//   - claude_code.interaction      — counts toward agg.InteractionCount
//   - claude_code.llm_request      — counts toward agg.LLMRequestCount
//     AND adds to agg.EconomicsByModel
//     (input/output/cache tokens, ms)
//   - claude_code.tool             — counts toward agg.ToolCallCount,
//     bumps agg.ToolNameCounts[tool_name],
//     and (when subagent_type is set)
//     bumps agg.SubagentDispatchTypes
//   - claude_code.tool.execution   — IGNORED (sub-span of `tool`;
//     counting it would double-count)
//   - claude_code.tool.blocked_on_user — IGNORED (sub-span of `tool`)
//
// Missing file is NOT an error — a session may have hook data
// without trace data (e.g., the receiver wasn't running).
func loadTraces(path, sessionID string, agg *aggregated) error {
	f, err := os.Open(path) //nolint:gosec // G304: path from caller-controlled inDir, not user input at this layer
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close() //nolint:errcheck // close after read; no err to act on

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // match receiver's body cap
	for scanner.Scan() {
		var batch otlpTraceBatch
		if err := json.Unmarshal(scanner.Bytes(), &batch); err != nil {
			// Malformed line — skip rather than fail the whole
			// report. Future improvement: log to stderr with line
			// number for debuggability.
			continue
		}
		for _, rs := range batch.ResourceSpans {
			resSessionID := stringAttr(rs.Resource.Attributes, "session.id")
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					// Match by either level — see method-doc.
					if resSessionID != sessionID && stringAttr(sp.Attributes, "session.id") != sessionID {
						continue
					}
					agg.HasTraceData = true
					switch sp.Name {
					case "claude_code.interaction":
						agg.InteractionCount++
					case "claude_code.llm_request":
						agg.LLMRequestCount++
						aggregateEconomics(agg, sp)
					case "claude_code.tool":
						agg.ToolCallCount++
						if name := stringAttr(sp.Attributes, "tool_name"); name != "" {
							agg.ToolNameCounts[name]++
						}
						if st := stringAttr(sp.Attributes, "subagent_type"); st != "" {
							agg.SubagentDispatchTypes[st]++
						}
					}
				}
			}
		}
	}
	return scanner.Err()
}

// aggregateEconomics adds one llm_request span's token + duration
// totals into the per-model bucket in agg. The model key prefers
// `gen_ai.request.model` (OTEL semantic-conventions name) and falls
// back to the simpler `model` attribute when absent.
//
// All the numeric attributes are stringValue-typed in OTLP-JSON, so
// we parseInt with errors-as-zero — a missing or malformed value
// doesn't break aggregation; it just contributes nothing.
func aggregateEconomics(agg *aggregated, sp otlpSpan) {
	model := stringAttr(sp.Attributes, "gen_ai.request.model")
	if model == "" {
		model = stringAttr(sp.Attributes, "model")
	}
	if model == "" {
		model = "(unknown)"
	}
	stats, ok := agg.EconomicsByModel[model]
	if !ok {
		stats = &modelEconomics{}
		agg.EconomicsByModel[model] = stats
	}
	stats.Calls++
	stats.InputTokens += parseStringInt(stringAttr(sp.Attributes, "input_tokens"))
	stats.OutputTokens += parseStringInt(stringAttr(sp.Attributes, "output_tokens"))
	stats.CacheReadTokens += parseStringInt(stringAttr(sp.Attributes, "cache_read_tokens"))
	stats.CacheCreationTokens += parseStringInt(stringAttr(sp.Attributes, "cache_creation_tokens"))
	stats.TotalDurationMs += parseStringInt(stringAttr(sp.Attributes, "duration_ms"))
	if ttft := parseStringInt(stringAttr(sp.Attributes, "ttft_ms")); ttft > 0 {
		stats.TTFTSamples = append(stats.TTFTSamples, ttft)
	}
}

// percentile returns the p-th percentile of a slice of int64
// samples using the closest-rank method (the simplest and most
// commonly understood definition for small samples). Returns 0 and
// false when samples is empty.
//
// Closest-rank: for percentile p (0 < p ≤ 100), the index is
// ceil(p/100 * N) - 1, clamped to [0, N-1]. For N=10 samples and
// p=50, index = ceil(5)-1 = 4 → the 5th sample. For p=95, index =
// ceil(9.5)-1 = 9 → the 10th sample.
//
// Caller is responsible for sorting the slice first; this avoids
// repeated sorts when computing multiple percentiles back-to-back.
func percentile(sortedSamples []int64, p float64) (int64, bool) {
	n := len(sortedSamples)
	if n == 0 {
		return 0, false
	}
	idx := int(math.Ceil(p/100*float64(n))) - 1
	idx = max(idx, 0)
	if idx >= n {
		idx = n - 1
	}
	return sortedSamples[idx], true
}

// sortInt64s sorts a slice of int64 in place, ascending. Tiny
// adapter so the renderer reads cleanly — slices.Sort at the call
// site would obscure the intent.
func sortInt64s(s []int64) {
	slices.Sort(s)
}

// formatPercentile renders a percentile cell for the LLM economics
// table. Returns "—" (em-dash) when the sample slice is empty so
// rows for models that recorded zero ttft_ms attributes still align
// with the rest of the table.
func formatPercentile(sortedSamples []int64, p float64) string {
	v, ok := percentile(sortedSamples, p)
	if !ok {
		return "—"
	}
	return strconv.FormatInt(v, 10)
}

// parseStringInt parses s as an int64, returning 0 on empty or
// malformed input. OTLP-JSON encodes numeric attribute values as
// strings; we accept that shape uniformly here so the aggregation
// loop stays branch-free per attribute.
func parseStringInt(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// loadHooks streams the hook JSONL file for this session and
// aggregates classification counts plus the external/source-read
// detail lists. Missing file is not an error (e.g., receiver
// without registered hook).
func loadHooks(path string, agg *aggregated) error {
	f, err := os.Open(path) //nolint:gosec // G304: path from caller-controlled inDir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4*1024), 1*1024*1024) // hook events are tiny; 1 MiB cap is generous
	for scanner.Scan() {
		var ev hookEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		agg.HasHookData = true
		if ev.Classification != "" {
			agg.ClassificationCounts[ev.Classification]++
		}
		switch ev.Classification {
		case "external_web", "external_curl", "external_git":
			agg.ExternalCalls = append(agg.ExternalCalls, ev)
		case "signatory_source":
			agg.SourceReads = append(agg.SourceReads, ev)
		}
	}
	return scanner.Err()
}

// render produces the markdown report. Section order is fixed —
// readers learn where to look and consistency matters more than
// per-report novelty.
func render(agg *aggregated) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Dogfood report — session %s\n\n", agg.SessionID)
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	renderSessionActivity(&b, agg)
	renderLLMEconomics(&b, agg)
	renderToolDistribution(&b, agg)
	renderSubagentDispatches(&b, agg)
	renderClassificationTable(&b, agg)
	renderExternalCalls(&b, agg)
	renderSourceReads(&b, agg)

	return b.String()
}

// renderSessionActivity writes the high-level activity summary —
// counts of interactions, LLM requests, and tool calls. Replaces
// the previous "Subagent activity" section, which was specced
// against a `query_source` attribute Claude Code's OTEL output
// doesn't actually emit (verified 2026-04-28 via dogfood-metrics
// inspect; see commit 83248f8).
func renderSessionActivity(b *strings.Builder, agg *aggregated) {
	b.WriteString("## Session activity\n\n")
	if !agg.HasTraceData {
		b.WriteString("no trace spans recorded\n\n")
		return
	}
	fmt.Fprintf(b, "- %d user interaction(s)\n", agg.InteractionCount)
	fmt.Fprintf(b, "- %d LLM request(s)\n", agg.LLMRequestCount)
	fmt.Fprintf(b, "- %d tool call(s)\n", agg.ToolCallCount)
	if subagentCount := sumIntMap(agg.SubagentDispatchTypes); subagentCount > 0 {
		fmt.Fprintf(b, "- %d subagent dispatch(es) — see Subagent dispatches section\n", subagentCount)
	}
	b.WriteString("\n")
}

// renderLLMEconomics writes the per-model token + duration table.
// Drawn from `claude_code.llm_request` spans' attributes; absent
// trace data renders the section header plus a no-data note so
// readers don't assume "missing section" means "missing feature."
//
// Cache hit ratio is CacheRead / (Input + CacheCreation + CacheRead)
// — the share of input-side tokens served from cache. Formula
// surfaced inline in the rendered output so a reader inspecting a
// surprisingly-high or surprisingly-low ratio can verify what
// they're looking at.
func renderLLMEconomics(b *strings.Builder, agg *aggregated) {
	b.WriteString("## LLM economics\n\n")
	if len(agg.EconomicsByModel) == 0 {
		b.WriteString("no LLM request spans recorded\n\n")
		return
	}
	models := make([]string, 0, len(agg.EconomicsByModel))
	for m := range agg.EconomicsByModel {
		models = append(models, m)
	}
	sort.Strings(models)

	// Percentile columns surface the latency distribution. p50 (the
	// median) is robust to outliers in a way mean isn't; p95 (the
	// tail) is what determines whether the workflow feels fast —
	// the slowest LLM calls are the bottleneck for sequential
	// orchestrator turns. Cross-model latency comparison isn't
	// meaningful (different models have different speeds), so the
	// TOTAL row elides percentiles in favor of em-dashes.
	b.WriteString("| Model | Calls | Input | Output | Cache read | Cache create | Total ms | TTFT p50 | TTFT p95 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")

	var totals modelEconomics
	for _, m := range models {
		s := agg.EconomicsByModel[m]
		// Sort the per-model samples once; both percentile lookups
		// reuse the sorted slice. Sorts in place — modelEconomics
		// is a per-render artifact, mutating it is fine.
		sortInt64s(s.TTFTSamples)
		p50 := formatPercentile(s.TTFTSamples, 50)
		p95 := formatPercentile(s.TTFTSamples, 95)
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d | %s | %s |\n",
			m, s.Calls, s.InputTokens, s.OutputTokens,
			s.CacheReadTokens, s.CacheCreationTokens,
			s.TotalDurationMs, p50, p95)

		totals.Calls += s.Calls
		totals.InputTokens += s.InputTokens
		totals.OutputTokens += s.OutputTokens
		totals.CacheReadTokens += s.CacheReadTokens
		totals.CacheCreationTokens += s.CacheCreationTokens
		totals.TotalDurationMs += s.TotalDurationMs
	}

	// Aggregate row only when more than one model surfaced — for a
	// single-model session the totals row is just visual noise.
	if len(models) > 1 {
		fmt.Fprintf(b, "| **TOTAL** | %d | %d | %d | %d | %d | %d | — | — |\n",
			totals.Calls, totals.InputTokens, totals.OutputTokens,
			totals.CacheReadTokens, totals.CacheCreationTokens,
			totals.TotalDurationMs)
	}
	b.WriteString("\n")

	// Cache hit ratio across the session, formula explicit.
	denom := totals.InputTokens + totals.CacheCreationTokens + totals.CacheReadTokens
	if denom > 0 {
		ratio := float64(totals.CacheReadTokens) / float64(denom) * 100
		fmt.Fprintf(b, "Cache hit ratio: %.1f%% — `cache_read / (input + cache_creation + cache_read)`\n\n", ratio)
	}
}

// renderToolDistribution writes the per-tool-name count table.
// Surfaces "where is the orchestrator spending its tool calls"
// without depending on subagent attribution (which lives in
// separate sessions, see renderSubagentDispatches's note).
func renderToolDistribution(b *strings.Builder, agg *aggregated) {
	b.WriteString("## Tool calls by name\n\n")
	if len(agg.ToolNameCounts) == 0 {
		b.WriteString("no tool spans recorded (or no `tool_name` attribute on the spans)\n\n")
		return
	}
	type kv struct {
		name  string
		count int
	}
	rows := make([]kv, 0, len(agg.ToolNameCounts))
	for k, v := range agg.ToolNameCounts {
		rows = append(rows, kv{k, v})
	}
	// Sort by count descending, ties broken by name ascending — most
	// significant entries surface first; ties stay deterministic.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].name < rows[j].name
	})

	b.WriteString("| Tool | Count |\n|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | %d |\n", r.name, r.count)
	}
	b.WriteString("\n")
}

// renderSubagentDispatches writes the per-subagent-type count table.
// Drawn from `claude_code.tool` spans where `subagent_type` is set
// — those are the Task-tool spawns where the orchestrator dispatched
// a subagent. The subagent's OWN activity (its tool calls, its LLM
// requests) lives in a SEPARATE Claude session with its own
// session.id, and would be reported separately by running the
// report against that session id.
func renderSubagentDispatches(b *strings.Builder, agg *aggregated) {
	b.WriteString("## Subagent dispatches\n\n")
	if len(agg.SubagentDispatchTypes) == 0 {
		b.WriteString("none — this session did not spawn subagents via the Task tool\n\n")
		return
	}
	keys := make([]string, 0, len(agg.SubagentDispatchTypes))
	for k := range agg.SubagentDispatchTypes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	b.WriteString("| Subagent type | Dispatches |\n|---|---|\n")
	for _, k := range keys {
		fmt.Fprintf(b, "| %s | %d |\n", k, agg.SubagentDispatchTypes[k])
	}
	b.WriteString("\n")
	b.WriteString("Each subagent runs in its own Claude session with a separate\n")
	b.WriteString("`session.id`; running this report against that session ID\n")
	b.WriteString("surfaces the subagent's own activity (tool calls, LLM\n")
	b.WriteString("requests, etc.). Cross-session correlation is not yet\n")
	b.WriteString("automated — `dogfood-metrics list-sessions` shows all\n")
	b.WriteString("sessions in the file.\n\n")
}

// sumIntMap returns the sum of values in a string→int map.
func sumIntMap(m map[string]int) int {
	var total int
	for _, v := range m {
		total += v
	}
	return total
}

// renderClassificationTable writes the tool-call breakdown.
// Sorted alphabetically by classification.
func renderClassificationTable(b *strings.Builder, agg *aggregated) {
	b.WriteString("## Tool-call classification\n\n")
	if !agg.HasHookData {
		b.WriteString("no hook events recorded\n\n")
		return
	}
	keys := make([]string, 0, len(agg.ClassificationCounts))
	for k := range agg.ClassificationCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("| Classification | Count |\n")
	b.WriteString("|---|---|\n")
	for _, k := range keys {
		fmt.Fprintf(b, "| %s | %d |\n", k, agg.ClassificationCounts[k])
	}
	b.WriteString("\n")
}

// renderExternalCalls is the cache-miss-candidate section. Per
// ROADMAP "improve economics," each truly-external entry is
// something to review for "did we have this in the local DB?"
//
// Loopback URLs (127.0.0.1, localhost, [::1]) are filtered out
// — they're typically signatory's own pipeline service or local
// dev tooling, not cache-miss candidates. Their count surfaces
// separately so reviewers don't lose visibility (per dogfood-
// errors entry "report classifier under-categorizes external_web").
func renderExternalCalls(b *strings.Builder, agg *aggregated) {
	b.WriteString("## External calls (cache-miss candidates)\n\n")
	b.WriteString("Per the ROADMAP \"improve economics\" subsection: external calls\n")
	b.WriteString("to data we already have are bugs. Review each entry below — if\n")
	b.WriteString("the data should have been in the local DB, file a\n")
	b.WriteString("missing-collector gap.\n\n")

	// Partition into truly-external vs loopback.
	var external, loopback []hookEvent
	for _, ev := range agg.ExternalCalls {
		if isLoopbackURL(ev.Detail) {
			loopback = append(loopback, ev)
		} else {
			external = append(external, ev)
		}
	}

	if len(external) == 0 {
		b.WriteString("no external calls in this session\n")
	} else {
		b.WriteString("| Tool | Detail |\n")
		b.WriteString("|---|---|\n")
		for _, ev := range external {
			fmt.Fprintf(b, "| %s | %s |\n", ev.ToolName, ev.Detail)
		}
	}

	if len(loopback) > 0 {
		// Suffix line with the loopback count so reviewers see the
		// total volume of "external_web" classification but know it
		// excludes local-pipeline / dev-tooling traffic.
		fmt.Fprintf(b, "\n_(%d loopback call%s excluded — local pipeline service / dev tooling)_\n",
			len(loopback), pluralS(len(loopback)))
	}
	b.WriteString("\n")
}

// isLoopbackURL returns true when the URL targets a loopback
// host (127.0.0.1, localhost, IPv6 ::1). Used to filter
// localhost traffic out of the cache-miss-candidate list — a
// signatory pipeline service fetch is loopback-by-design, not a
// "the LLM reached for the network when it should have used the
// store" event.
//
// Unparseable URLs return false (treat as external — better to
// over-report a non-URL entry than to silently swallow it).
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	// IPv4 127.0.0.0/8 and bracket-form IPv6 ::1 already covered;
	// other forms (e.g., "0.0.0.0", "0:0:0:0:0:0:0:1") are
	// non-canonical and rare enough that we don't extend the list
	// pre-emptively. Add cases when a real URL trips us up.
	return false
}

// pluralS returns "s" when n != 1, the empty string otherwise.
// Tiny helper for the loopback-count suffix line.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// renderSourceReads is the underspecification-candidate section.
// Per design/agent-otel.md: the analyst should never need to
// read signatory's own source.
func renderSourceReads(b *strings.Builder, agg *aggregated) {
	b.WriteString("## Source reads (underspecification candidates)\n\n")
	b.WriteString("Per design/agent-otel.md: the analyst should never need to read\n")
	b.WriteString("signatory's own source. Each entry below is evidence that a\n")
	b.WriteString("handoff template, MCP description, or schema doc didn't surface\n")
	b.WriteString("what the analyst needed.\n\n")
	if len(agg.SourceReads) == 0 {
		b.WriteString("no source-tree reads in this session\n\n")
		return
	}
	b.WriteString("| Tool | Path |\n")
	b.WriteString("|---|---|\n")
	for _, ev := range agg.SourceReads {
		fmt.Fprintf(b, "| %s | %s |\n", ev.ToolName, ev.Detail)
	}
	b.WriteString("\n")
}
