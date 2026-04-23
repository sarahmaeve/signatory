package store

// nullableString returns the string as an `any` suitable for
// database/sql parameter binding: empty becomes SQL NULL, non-empty
// is passed verbatim. Used on FK-nullable columns so empty strings
// never try to resolve against the FK target — without it the
// constraint surfaces as a confusing "FOREIGN KEY constraint failed"
// at commit time rather than a clean NULL storage.
//
// Shared across the store package; new code that binds a
// potentially-empty string into a nullable column should use this
// rather than hand-rolling an `any` switch.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
