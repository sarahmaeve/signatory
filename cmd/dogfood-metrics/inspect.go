package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runInspect reads <inDir>/traces.jsonl and writes a markdown
// diagnosis to w describing the structure of the spans for the
// requested session: span name distribution, attribute keys per
// span name, resource attribute keys, and a filter-diagnosis block
// that names exactly which of the report's filters drops what.
//
// Built to answer questions like "why does the report say 'no trace
// spans recorded' when traces.jsonl has 553 lines?" — the report's
// silent-skip on session/name/attribute-mismatch is operator-
// hostile; this subcommand is the explicit diagnostic counterpart.
//
// Returns error when traces.jsonl is missing (operator-actionable —
// the receiver isn't running, or the path is wrong) and surfaces
// otherwise via the rendered output.
func runInspect(sessionID, inDir string, w io.Writer) error {
	path := filepath.Join(inDir, "traces.jsonl")
	f, err := os.Open(path) //nolint:gosec // G304: inDir is caller-controlled flag, not user input at this layer
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("traces.jsonl not found at %s — is the receiver running, or is -in-dir wrong?", path)
		}
		return err
	}
	defer f.Close() //nolint:errcheck

	agg := &inspectAggregated{
		sessionID:          sessionID,
		spanNameCounts:     map[string]int{},
		spanAttrKeysByName: map[string]map[string]int{},
		resourceAttrKeys:   map[string]int{},
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var batch otlpTraceBatch
		if err := json.Unmarshal(scanner.Bytes(), &batch); err != nil {
			agg.malformedLines++
			continue
		}
		agg.totalBatches++
		for _, rs := range batch.ResourceSpans {
			resSessionID := stringAttr(rs.Resource.Attributes, "session.id")
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					agg.totalSpansAllSessions++

					// Match by either resource OR span attribute. The
					// report's filter today only checks resource;
					// inspect surfaces both paths so the operator sees
					// what's there even when the report would drop it.
					matchedResource := resSessionID == sessionID
					matchedSpan := !matchedResource && stringAttr(sp.Attributes, "session.id") == sessionID
					if !matchedResource && !matchedSpan {
						continue
					}

					agg.matchingSpans++
					if matchedResource {
						agg.sessionMatchedSpans++
					}
					if matchedSpan {
						agg.sessionIDOnSpan++
					}

					// Track resource attr keys for matching spans.
					for _, a := range rs.Resource.Attributes {
						agg.resourceAttrKeys[a.Key]++
					}

					// Span name distribution.
					agg.spanNameCounts[sp.Name]++

					// Span attribute keys per name.
					if _, ok := agg.spanAttrKeysByName[sp.Name]; !ok {
						agg.spanAttrKeysByName[sp.Name] = map[string]int{}
					}
					for _, a := range sp.Attributes {
						agg.spanAttrKeysByName[sp.Name][a.Key]++
					}

					// Filter diagnosis: report.go's later filters,
					// gated on the resource-level match (so the
					// numbers track what report.go would report).
					if matchedResource {
						if sp.Name == "claude_code.llm_request" || sp.Name == "claude_code.tool" {
							agg.nameFilterMatched++
						}
						if stringAttr(sp.Attributes, "query_source") != "" {
							agg.querySourcePresent++
						}
					}

					// Capture first matching span as the sample.
					if agg.sampleSpan == nil {
						sample := sp // copy off the loop var
						agg.sampleSpan = &sample
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan traces.jsonl: %w", err)
	}

	return renderInspect(w, agg)
}

// inspectAggregated holds the per-batch / per-span counts and the
// sample span used by the markdown renderer.
type inspectAggregated struct {
	sessionID string

	// File-level counts.
	totalBatches          int
	totalSpansAllSessions int
	malformedLines        int

	// Per-session counts.
	matchingSpans       int
	sessionMatchedSpans int // alias for matchingSpans, kept for filter-diagnosis clarity
	nameFilterMatched   int // session-matched AND name in {llm_request, tool}
	querySourcePresent  int // session-matched AND query_source attribute present
	sessionIDOnSpan     int // session.id appears as a SPAN attribute (resource doesn't match)

	// Distributions.
	spanNameCounts     map[string]int
	spanAttrKeysByName map[string]map[string]int
	resourceAttrKeys   map[string]int

	// Sample span for hands-on inspection.
	sampleSpan *otlpSpan
}

// renderInspect emits the markdown diagnosis. Sections in order:
//
//  1. Header (session id, file-level totals).
//  2. Filter diagnosis — which of the report's filters dropped what.
//  3. Span name distribution (matching this session).
//  4. Resource attribute keys (matching this session).
//  5. Span attribute keys per span name (matching this session).
//  6. Sample span (first matching, with raw attributes for human inspection).
func renderInspect(w io.Writer, agg *inspectAggregated) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Trace inspection — session %s\n\n", agg.sessionID)

	fmt.Fprintf(&b, "- total batches read: %d\n", agg.totalBatches)
	fmt.Fprintf(&b, "- total spans seen across all sessions: %d\n", agg.totalSpansAllSessions)
	fmt.Fprintf(&b, "- spans matching session %s: %d\n", agg.sessionID, agg.matchingSpans)
	if agg.malformedLines > 0 {
		fmt.Fprintf(&b, "- malformed JSON lines (skipped): %d\n", agg.malformedLines)
	}
	b.WriteString("\n")

	b.WriteString("## Filter diagnosis\n\n")
	b.WriteString("Each line is one of the report's filters in order. A drop\n")
	b.WriteString("between consecutive rows is the cause of \"no trace spans\n")
	b.WriteString("recorded.\"\n\n")
	fmt.Fprintf(&b, "- session.id resource attr matches: %d\n", agg.sessionMatchedSpans)
	fmt.Fprintf(&b, "- name in {claude_code.llm_request, claude_code.tool}: %d\n", agg.nameFilterMatched)
	fmt.Fprintf(&b, "- query_source attribute present: %d\n", agg.querySourcePresent)
	if agg.sessionIDOnSpan > 0 {
		fmt.Fprintf(&b, "- session.id appears as a SPAN attribute (resource lacks it): %d\n", agg.sessionIDOnSpan)
		b.WriteString("  (note: report.go filters by RESOURCE-level session.id; ")
		b.WriteString("this is shape drift the report doesn't currently follow.)\n")
	}
	b.WriteString("\n")

	if len(agg.spanNameCounts) > 0 {
		b.WriteString("## Span name distribution (this session)\n\n")
		b.WriteString("| Name | Count |\n|---|---|\n")
		names := sortedKeys(agg.spanNameCounts)
		for _, n := range names {
			fmt.Fprintf(&b, "| %s | %d |\n", n, agg.spanNameCounts[n])
		}
		b.WriteString("\n")
	}

	if len(agg.resourceAttrKeys) > 0 {
		b.WriteString("## Resource attribute keys (this session)\n\n")
		b.WriteString("| Key | Frequency |\n|---|---|\n")
		keys := sortedKeys(agg.resourceAttrKeys)
		for _, k := range keys {
			fmt.Fprintf(&b, "| %s | %d |\n", k, agg.resourceAttrKeys[k])
		}
		b.WriteString("\n")
	}

	if len(agg.spanAttrKeysByName) > 0 {
		b.WriteString("## Span attribute keys (per span name, this session)\n\n")
		names := make([]string, 0, len(agg.spanAttrKeysByName))
		for n := range agg.spanAttrKeysByName {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "### %s\n\n", n)
			fmt.Fprintf(&b, "| Key | Frequency |\n|---|---|\n")
			for _, k := range sortedKeys(agg.spanAttrKeysByName[n]) {
				fmt.Fprintf(&b, "| %s | %d |\n", k, agg.spanAttrKeysByName[n][k])
			}
			b.WriteString("\n")
		}
	}

	if agg.sampleSpan != nil {
		b.WriteString("## Sample span (first matching this session)\n\n")
		fmt.Fprintf(&b, "- name: `%s`\n", agg.sampleSpan.Name)
		fmt.Fprintf(&b, "- start: %s\n", agg.sampleSpan.StartTimeUnixNano)
		fmt.Fprintf(&b, "- end:   %s\n", agg.sampleSpan.EndTimeUnixNano)
		if len(agg.sampleSpan.Attributes) > 0 {
			b.WriteString("- attributes:\n")
			for _, a := range agg.sampleSpan.Attributes {
				fmt.Fprintf(&b, "  - %s = %q\n", a.Key, a.Value.StringValue)
			}
		}
		b.WriteString("\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// sortedKeys returns the keys of m in ascending lexicographic order.
// Small helper so tables render stably across runs.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
