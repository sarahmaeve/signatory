package source

import "errors"

// ErrPinTableNotAvailable is returned by VersionPinSource when neither
// the in-run CollectionResult nor the signal store contains a
// version_pin_table for the requested entity. The source-evolution
// collector treats this as a graceful absence (records reason and
// returns) rather than a hard failure.
var ErrPinTableNotAvailable = errors.New("version_pin_table signal not available in run or store")
