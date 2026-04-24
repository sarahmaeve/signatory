package store

import "errors"

var (
	// ErrNotFound is returned when a requested entity, signal, posture, or burn does not exist.
	ErrNotFound = errors.New("not found")

	// ErrNilInput is returned when a required input parameter is nil.
	ErrNilInput = errors.New("nil input")

	// ErrOrphanedEntity is returned when a write path is asked to
	// insert a row whose entity_id does not resolve to a row in the
	// entities table. Distinct from ErrNotFound (which describes a
	// caller-initiated read miss): ErrOrphanedEntity describes a
	// rejection of a write that would have landed a row whose
	// foreign key doesn't resolve. Callers that want to programmatically
	// distinguish "you asked me to write an invalid reference" from
	// generic write failures use errors.Is.
	//
	// Defined here ahead of its first production use (Phase 5 of the
	// orphan-prevention audit; see design/orphanage.md). Callers will
	// surface it from AppendResolution and other write paths whose
	// entity_id referent must be pre-validated before INSERT.
	ErrOrphanedEntity = errors.New("referenced entity does not exist")
)
