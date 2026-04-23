package profile

import "encoding/json"

// SignalsSummary is the grouped-and-flattened view of a collection of
// Signals, organized by the question each group answers. Each group
// maps signal-type names to the signal's `value` JSON (unmarshaled to
// map[string]interface{}). This is the shape signatory_analyze's MCP
// tool returns, and the same shape the provenance-handoff renderer
// inlines into the agent's handoff body as pre-collected ground truth.
//
// Groups are omitempty so a target with only governance signals
// doesn't ship empty vitality/criticality/hygiene/publication keys.
type SignalsSummary struct {
	Vitality    map[string]interface{} `json:"vitality,omitempty"`
	Governance  map[string]interface{} `json:"governance,omitempty"`
	Criticality map[string]interface{} `json:"criticality,omitempty"`
	Hygiene     map[string]interface{} `json:"hygiene,omitempty"`
	Publication map[string]interface{} `json:"publication,omitempty"`
}

// Summarize groups signals by SignalGroup and flattens each signal's
// raw JSON value into the per-group map, keyed by signal type. Returns
// an empty SignalsSummary for a nil or empty input slice — callers
// that distinguish "no signals" from "all-empty groups" must check the
// input before calling.
//
// Decode robustness: a corrupt Signal.Value (non-object JSON, truncated
// bytes) for one signal does not drop the whole summary. On decode
// failure that one signal's entry becomes an empty map, which is the
// documented shape for an unknown or unreadable value. The raw bytes
// remain in the store for debugging via signatory_signals.
//
// Signals in a SignalGroup other than the five enumerated above
// (vitality, governance, criticality, hygiene, publication) are
// silently skipped. That includes SignalGroupPosture, which is a
// caller-side concern — posture lives at the entity level, not in the
// per-signal summary.
func Summarize(signals []Signal) SignalsSummary {
	s := SignalsSummary{}
	for _, sig := range signals {
		var val map[string]interface{}
		_ = json.Unmarshal(sig.Value, &val) //nolint:errcheck // nil-safe summary on decode failure; raw bytes preserved in store
		if val == nil {
			val = map[string]interface{}{}
		}
		switch sig.Group {
		case SignalGroupVitality:
			if s.Vitality == nil {
				s.Vitality = map[string]interface{}{}
			}
			s.Vitality[sig.Type] = val
		case SignalGroupGovernance:
			if s.Governance == nil {
				s.Governance = map[string]interface{}{}
			}
			s.Governance[sig.Type] = val
		case SignalGroupCriticality:
			if s.Criticality == nil {
				s.Criticality = map[string]interface{}{}
			}
			s.Criticality[sig.Type] = val
		case SignalGroupHygiene:
			if s.Hygiene == nil {
				s.Hygiene = map[string]interface{}{}
			}
			s.Hygiene[sig.Type] = val
		case SignalGroupPublication:
			if s.Publication == nil {
				s.Publication = map[string]interface{}{}
			}
			s.Publication[sig.Type] = val
		}
	}
	return s
}
