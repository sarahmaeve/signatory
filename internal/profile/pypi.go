package profile

import (
	"regexp"
	"strings"
)

// PyPI package-name normalization per PEP 503.
//
// Unlike npm (which is case-sensitive — `Express` ≠ `express`) and Go
// modules (which preserve the verbatim import path), PyPI treats
// package names as case-insensitive and collapses runs of `.`, `-`,
// and `_` into a single `-`. The registry itself performs this
// normalization on lookup — querying `pypi.org/pypi/Requests/json`,
// `pypi.org/pypi/requests/json`, and `pypi.org/pypi/REQUESTS/json`
// all return identical responses.
//
// For signatory, that means the canonical URI for a PyPI package
// MUST be normalized before storage. Without normalization,
// `pkg:pypi/Requests` and `pkg:pypi/requests` would be distinct
// entity rows in the store, fragmenting postures and signals across
// identities that the registry considers the same.
//
// Normalization is applied at the resolution boundary — every path
// that emits a canonical URI for a PyPI target (URL parsing,
// canonical-URI acceptance, manifest parsing, signal collection)
// routes the name through NormalizePyPIName. The URI produced after
// that point is what's stored and queried.
//
// Reference: PEP 503 §Normalized Names
// (https://peps.python.org/pep-0503/#normalized-names).
//
// Reference implementation from PyPA's packaging library:
//
//	_canonicalize_regex = re.compile(r"[-_.]+")
//	def canonicalize_name(name):
//	    return _canonicalize_regex.sub("-", name).lower()

// pypiSeparatorRun matches one or more of PEP 503's three separator
// characters. ASCII-only by design — PEP 503 names are ASCII, and
// non-ASCII separators in a PyPI name already fail PEP 508's name
// grammar upstream of this function.
var pypiSeparatorRun = regexp.MustCompile(`[-_.]+`)

// NormalizePyPIName returns the PEP 503 canonical form of a PyPI
// package name: lowercase, with every run of `.`, `-`, or `_`
// collapsed to a single `-`. Safe to call on any string; empty
// input returns empty output. No grammar validation — a name that
// violates PEP 508 (empty, starts/ends with separator, contains
// non-ASCII) passes through this function unchanged and is caught
// by downstream grammar checks (Layer 4 registry client).
//
// Idempotent: NormalizePyPIName(NormalizePyPIName(x)) == NormalizePyPIName(x).
func NormalizePyPIName(name string) string {
	return strings.ToLower(pypiSeparatorRun.ReplaceAllString(name, "-"))
}
