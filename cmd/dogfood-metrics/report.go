package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	Name       string   `json:"name"`
	Attributes []otlpKV `json:"attributes"`
}

// otlpKV is OTLP's attribute encoding: each attribute is a
// {key, value} pair, value being a typed wrapper. We only read
// stringValue today; the report doesn't need numeric or array
// attribute types.
type otlpKV struct {
	Key   string      `json:"key"`
	Value otlpKVValue `json:"value"`
}

type otlpKVValue struct {
	StringValue string `json:"stringValue"`
}

// stringAttr returns the stringValue for the named key, or "" if
// not present.
func stringAttr(attrs []otlpKV, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.StringValue
		}
	}
	return ""
}

// subagentStats aggregates span counts per query_source. The
// breakdown by span-name kind (llm_request vs tool) makes the
// "where is this subagent spending its turns" question answerable.
type subagentStats struct {
	LLMRequests int
	ToolCalls   int
}

// aggregated carries everything the markdown renderer needs.
// Built once from disk inputs, consumed by render().
type aggregated struct {
	SessionID            string
	SpansBySubagent      map[string]*subagentStats
	ClassificationCounts map[string]int
	ExternalCalls        []hookEvent
	SourceReads          []hookEvent
	HasHookData          bool
	HasTraceData         bool
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
		SessionID:            sessionID,
		SpansBySubagent:      map[string]*subagentStats{},
		ClassificationCounts: map[string]int{},
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
// We filter resourceSpans by session.id resource attribute and
// aggregate matching spans into agg.
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
			if stringAttr(rs.Resource.Attributes, "session.id") != sessionID {
				continue
			}
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					agg.HasTraceData = true
					qs := stringAttr(sp.Attributes, "query_source")
					if qs == "" {
						qs = "(unknown)"
					}
					stats, ok := agg.SpansBySubagent[qs]
					if !ok {
						stats = &subagentStats{}
						agg.SpansBySubagent[qs] = stats
					}
					switch sp.Name {
					case "claude_code.llm_request":
						stats.LLMRequests++
					case "claude_code.tool":
						stats.ToolCalls++
					}
				}
			}
		}
	}
	return scanner.Err()
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

	renderSubagentTable(&b, agg)
	renderClassificationTable(&b, agg)
	renderExternalCalls(&b, agg)
	renderSourceReads(&b, agg)

	return b.String()
}

// renderSubagentTable writes the per-subagent span counts. Sorted
// alphabetically by query_source for stable output.
func renderSubagentTable(b *strings.Builder, agg *aggregated) {
	b.WriteString("## Subagent activity\n\n")
	if !agg.HasTraceData {
		b.WriteString("no trace spans recorded\n\n")
		return
	}
	keys := make([]string, 0, len(agg.SpansBySubagent))
	for k := range agg.SpansBySubagent {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("| Subagent | LLM requests | Tool calls |\n")
	b.WriteString("|---|---|---|\n")
	for _, k := range keys {
		s := agg.SpansBySubagent[k]
		fmt.Fprintf(b, "| %s | %d | %d |\n", k, s.LLMRequests, s.ToolCalls)
	}
	b.WriteString("\n")
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
// ROADMAP "improve economics," each entry is something to review
// for "did we have this in the local DB?"
func renderExternalCalls(b *strings.Builder, agg *aggregated) {
	b.WriteString("## External calls (cache-miss candidates)\n\n")
	b.WriteString("Per the ROADMAP \"improve economics\" subsection: external calls\n")
	b.WriteString("to data we already have are bugs. Review each entry below — if\n")
	b.WriteString("the data should have been in the local DB, file a\n")
	b.WriteString("missing-collector gap.\n\n")
	if len(agg.ExternalCalls) == 0 {
		b.WriteString("no external calls in this session\n\n")
		return
	}
	b.WriteString("| Tool | Detail |\n")
	b.WriteString("|---|---|\n")
	for _, ev := range agg.ExternalCalls {
		fmt.Fprintf(b, "| %s | %s |\n", ev.ToolName, ev.Detail)
	}
	b.WriteString("\n")
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
