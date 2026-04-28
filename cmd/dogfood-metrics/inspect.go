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
		sessionID:                sessionID,
		spanNameCounts:           map[string]int{},
		spanAttrKeysByName:       map[string]map[string]int{},
		resourceAttrKeys:         map[string]int{},
		spansBySessionAcrossFile: map[string]int{},
		dispatchesInSession:      nil,
		spansByParentID:          map[string][]inspectChildSpan{},
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

					// Trace-correlation pass: count every span by its
					// session.id (resource OR span level — whichever
					// has it), regardless of whether it matches the
					// requested session. Lets the renderer surface the
					// full session inventory in the file, so the
					// operator sees what other sessions exist without
					// running list-sessions separately.
					spanSessionID := resSessionID
					if spanSessionID == "" {
						spanSessionID = stringAttr(sp.Attributes, "session.id")
					}
					if spanSessionID != "" {
						agg.spansBySessionAcrossFile[spanSessionID]++
					}

					// Trace-correlation pass: index every span by its
					// parentSpanId so the renderer can report each
					// dispatch span's children efficiently. spanID
					// being unique-within-trace, indexing by parentID
					// across the whole file is the right grain — a
					// dispatch span in the requested session may have
					// children that report the same session.id (the
					// usual case) or a forked session.id (hypothetical
					// future Task semantics).
					if sp.ParentSpanID != "" {
						agg.spansByParentID[sp.ParentSpanID] = append(
							agg.spansByParentID[sp.ParentSpanID],
							inspectChildSpan{
								SpanID:    sp.SpanID,
								Name:      sp.Name,
								SessionID: spanSessionID,
								TraceID:   sp.TraceID,
							},
						)
					}

					// Match by either resource OR span attribute. The
					// report's filter today only checks resource;
					// inspect surfaces both paths so the operator sees
					// what's there even when the report would drop it.
					matchedResource := resSessionID == sessionID
					matchedSpan := !matchedResource && stringAttr(sp.Attributes, "session.id") == sessionID
					if !matchedResource && !matchedSpan {
						continue
					}

					// Within the requested session: capture every
					// dispatch span (subagent_type set) so the
					// renderer can report per-dispatch child linkage.
					if st := stringAttr(sp.Attributes, "subagent_type"); st != "" {
						agg.dispatchesInSession = append(agg.dispatchesInSession,
							inspectDispatchSpan{
								SpanID:       sp.SpanID,
								SubagentType: st,
								SessionID:    spanSessionID,
								TraceID:      sp.TraceID,
							},
						)
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

	// Trace correlation — populated unconditionally across the file
	// so the renderer can surface session inventory and per-dispatch
	// child linkage. The 2026-04-28 dogfood verified that current
	// Claude Code does NOT fork to a new session.id on Task
	// dispatch; this aggregation makes that finding (and any future
	// shape change) visible at a glance instead of requiring custom
	// trace-file probing.
	//
	// spansBySessionAcrossFile counts every span in traces.jsonl by
	// session.id (resource-level if present, otherwise span-level).
	// dispatchesInSession lists every claude_code.tool span in the
	// REQUESTED session that carries a subagent_type attribute.
	// spansByParentID indexes every span across the file by its
	// parentSpanId — used by the renderer to walk each dispatch's
	// children and report session continuity.
	spansBySessionAcrossFile map[string]int
	dispatchesInSession      []inspectDispatchSpan
	spansByParentID          map[string][]inspectChildSpan
}

// inspectDispatchSpan summarizes one Task-tool span that carried
// subagent_type — i.e., a subagent dispatch from the requested
// session. The spanId is the join key the renderer uses against
// spansByParentID to find the children.
type inspectDispatchSpan struct {
	SpanID       string
	SubagentType string
	SessionID    string
	TraceID      string
}

// inspectChildSpan summarizes one span found via parentSpanId
// during file traversal. SpanID is the child's own span ID — load-
// bearing for transitive descent (the BFS needs it to recurse),
// not just for attribution. Name/sessionID/traceID drive the
// continuity verdicts and the per-name filtering in the BFS.
type inspectChildSpan struct {
	SpanID    string
	Name      string
	SessionID string
	TraceID   string
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

	renderTraceCorrelation(&b, agg)

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

// renderTraceCorrelation emits the "Trace correlation" section:
//
//   - All session.ids present in the file with their span counts —
//     so the operator can see every session at once and pick the
//     right one without running list-sessions separately.
//   - Per-dispatch child linkage table — for each subagent_type
//     dispatch in the requested session, report child-span count
//     plus whether children share the parent's session.id (the
//     "session continuity" classification) and traceId. The 2026-
//     04-28 verification showed children do share parent session.id
//     in current Claude Code; if Task semantics ever fork, the
//     "forked-session" rows will surface immediately.
//
// Skipped entirely when the file has no parentSpanId references
// AND no dispatches in the requested session — there's nothing to
// correlate, and an empty section would just be noise.
func renderTraceCorrelation(b *strings.Builder, agg *inspectAggregated) {
	if len(agg.spansBySessionAcrossFile) == 0 && len(agg.dispatchesInSession) == 0 {
		return
	}

	b.WriteString("## Trace correlation\n\n")
	b.WriteString("All session.ids in the trace file plus per-dispatch child\n")
	b.WriteString("linkage. \"Same-session\" children stay in the parent\n")
	b.WriteString("session.id (current Claude Code shape — Task does not\n")
	b.WriteString("fork); \"forked\" rows would indicate a Task semantics\n")
	b.WriteString("change worth investigating.\n\n")

	if len(agg.spansBySessionAcrossFile) > 0 {
		b.WriteString("### Sessions in this trace file\n\n")
		b.WriteString("| Session | Spans |\n|---|---|\n")
		for _, sid := range sortedKeys(agg.spansBySessionAcrossFile) {
			fmt.Fprintf(b, "| %s | %d |\n", sid, agg.spansBySessionAcrossFile[sid])
		}
		b.WriteString("\n")
	}

	if len(agg.dispatchesInSession) > 0 {
		b.WriteString("### Subagent dispatches (this session)\n\n")
		b.WriteString("Direct children = spans with parentSpanId == dispatch.spanId.\n")
		b.WriteString("Transitive llm_requests = every claude_code.llm_request reachable\n")
		b.WriteString("from this dispatch via parentSpanId chains. The transitive\n")
		b.WriteString("count is the cross-check for the per-agent economics table —\n")
		b.WriteString("a single dispatch's transitive llm count equals that agent's\n")
		b.WriteString("Calls in the report's by-agent rollup; multiple dispatches of\n")
		b.WriteString("the same subagent_type sum.\n\n")
		b.WriteString("| Dispatch span | Subagent type | Direct children | Transitive llm_requests | Session continuity | Trace continuity |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, d := range agg.dispatchesInSession {
			children := agg.spansByParentID[d.SpanID]

			// Classify children's session continuity. Three buckets:
			//   - "no children" (orphan dispatch — surfaces dropped trace data)
			//   - "same-session" (every child carries the parent's session.id)
			//   - "forked: <id>" (some child carries a different session.id)
			//
			// Trace continuity is computed the same way against TraceID.
			sessionVerdict := classifyContinuity(children, d.SessionID, "same-session", func(c inspectChildSpan) string {
				return c.SessionID
			})
			traceVerdict := classifyContinuity(children, d.TraceID, "same-trace", func(c inspectChildSpan) string {
				return c.TraceID
			})

			// Transitive llm_request descendants: BFS the
			// spansByParentID graph from this dispatch's spanId,
			// counting every node where the underlying span name
			// is claude_code.llm_request. Decoupled from
			// session/trace continuity classification because the
			// answer to "how many LLM calls did this agent
			// make" is structural, independent of whether spans
			// stayed in-session.
			transitiveLLM := countTransitiveLLMRequests(d.SpanID, agg.spansByParentID)

			spanIDDisplay := d.SpanID
			if spanIDDisplay == "" {
				spanIDDisplay = "(unknown)"
			} else if len(spanIDDisplay) > 12 {
				spanIDDisplay = spanIDDisplay[:12]
			}

			fmt.Fprintf(b, "| %s | %s | %d | %d | %s | %s |\n",
				spanIDDisplay, d.SubagentType, len(children), transitiveLLM, sessionVerdict, traceVerdict)
		}
		b.WriteString("\n")
	}
}

// countTransitiveLLMRequests returns the count of
// claude_code.llm_request spans reachable from rootSpanID via the
// parent-pointer graph encoded in spansByParentID. BFS over the
// graph, terminating when the frontier is empty.
//
// Symmetric with the per-agent attribution algorithm in
// report.go's attributeAgent (which walks the OPPOSITE direction:
// a child looking up for a dispatch ancestor). This one walks
// down from a dispatch enumerating its descendants. Both
// algorithms see the same graph; the cross-check that the per-
// agent table's Calls equals this transitive count (or sums to
// it across multiple dispatches of the same subagent_type) is the
// invariant a future refactor would break loudly via diverging
// numbers.
//
// Visited set guards against cycles. Well-formed OTLP traces
// don't have any, but a corrupt parentSpanId pointing at an
// ancestor would otherwise spin forever; the cost of the visited
// map is trivial.
func countTransitiveLLMRequests(rootSpanID string, spansByParentID map[string][]inspectChildSpan) int {
	if rootSpanID == "" {
		return 0
	}
	count := 0
	visited := map[string]bool{rootSpanID: true}
	frontier := []string{rootSpanID}
	for len(frontier) > 0 {
		next := frontier[0]
		frontier = frontier[1:]
		for _, child := range spansByParentID[next] {
			if child.Name == "claude_code.llm_request" {
				count++
			}
			// Recurse into every child regardless of name —
			// non-llm spans (Bash, Read, nested Task) can have
			// llm_request descendants. Skip already-visited
			// nodes; without SpanID, fall back to skipping (rare
			// in practice but defensible).
			if child.SpanID == "" || visited[child.SpanID] {
				continue
			}
			visited[child.SpanID] = true
			frontier = append(frontier, child.SpanID)
		}
	}
	return count
}

// classifyContinuity reports, for a set of child spans, whether
// they all share the parent's value for some accessor (session.id
// or trace.id). Returns one of:
//
//   - "no children" — empty children slice. The dispatch is in the
//     trace data but its children aren't, which usually means trace
//     data was dropped or the receiver wasn't running for the
//     children's portion of the run.
//   - sameLabel — every child has the same value as the parent.
//     Caller provides the label ("same-session" or "same-trace")
//     so the rendered table reads naturally per-column.
//   - "forked: <other-value>" — one or more children carry a
//     different value. Surfaces the FIRST distinct value so the
//     operator can investigate without scanning the whole row set.
//
// Generic over the accessor function so session-continuity and
// trace-continuity reuse the same logic.
func classifyContinuity(children []inspectChildSpan, parentValue, sameLabel string, accessor func(inspectChildSpan) string) string {
	if len(children) == 0 {
		return "no children"
	}
	for _, c := range children {
		v := accessor(c)
		if v != parentValue {
			return "forked: " + v
		}
	}
	return sameLabel
}
