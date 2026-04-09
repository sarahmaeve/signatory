package engine

import (
	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Engine is the core analysis engine that coordinates signal collection,
// entity profile construction, and trust assessment.
type Engine struct {
	store      store.Store
	collectors []signal.Collector
	ecosystems []ecosystem.Provider
}

// New creates a new Engine with the given store, collectors, and providers.
func New(s store.Store, collectors []signal.Collector, ecosystems []ecosystem.Provider) *Engine {
	return &Engine{
		store:      s,
		collectors: collectors,
		ecosystems: ecosystems,
	}
}
