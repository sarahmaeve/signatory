package store

import "errors"

var (
	// ErrNotFound is returned when a requested entity, signal, posture, or burn does not exist.
	ErrNotFound = errors.New("not found")

	// ErrNilInput is returned when a required input parameter is nil.
	ErrNilInput = errors.New("nil input")
)
