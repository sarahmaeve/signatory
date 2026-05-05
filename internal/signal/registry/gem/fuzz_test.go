package gem

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for rubygems.org registry response parsing ---
//
// rubygems.org serves JSON for gem metadata, version lists, and owner
// lists. All publisher-supplied fields (source_code_uri, authors,
// homepage_uri) are untrusted. These fuzz tests verify safety
// invariants on the deserialized structs.

// --- FuzzGemResponseUnmarshal ---

func FuzzGemResponseUnmarshal(f *testing.F) {
	f.Add([]byte(`{"name":"rails","downloads":500000000,"version_downloads":2000000,"version":"7.1.3","version_created_at":"2024-01-15","created_at":"2005-07-13","authors":"David Heinemeier Hansson","info":"Full-stack web framework","licenses":["MIT"],"source_code_uri":"https://github.com/rails/rails","homepage_uri":"https://rubyonrails.org","changelog_uri":"","bug_tracker_uri":"https://github.com/rails/rails/issues","mfa_required":true}`))
	f.Add([]byte(`{"name":"nokogiri","downloads":800000000,"version":"1.16.0","authors":"Mike Dalessio, Aaron Patterson","licenses":["MIT"],"source_code_uri":"https://github.com/sparklemotion/nokogiri","homepage_uri":"https://nokogiri.org","mfa_required":false}`))
	// Minimal
	f.Add([]byte(`{"name":"x"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Adversarial: very long author string
	f.Add([]byte(`{"name":"x","authors":"` + strings.Repeat("Author, ", 500) + `Last Author"}`))
	// Negative downloads
	f.Add([]byte(`{"name":"x","downloads":-1,"version_downloads":-1}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp GemResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}

		// Invariant 1: all string fields must be valid UTF-8.
		strings := []string{
			resp.Name, resp.Version, resp.VersionCreatedAt,
			resp.CreatedAt, resp.Authors, resp.Info,
			resp.SourceCodeURI, resp.HomepageURI,
			resp.ChangelogURI, resp.BugTrackerURI,
		}
		for _, s := range strings {
			if !utf8.ValidString(s) {
				t.Errorf("GemResponse field is invalid UTF-8: %q", s)
			}
		}

		// Invariant 2: license entries must be valid UTF-8.
		for i, lic := range resp.Licenses {
			if !utf8.ValidString(lic) {
				t.Errorf("Licenses[%d] is invalid UTF-8: %q", i, lic)
			}
		}
	})
}

// --- FuzzVersionEntryUnmarshal ---

func FuzzVersionEntryUnmarshal(f *testing.F) {
	f.Add([]byte(`[{"number":"7.1.3","created_at":"2024-01-15","downloads_count":2000000,"authors":"DHH","prerelease":false,"yanked":false,"sha256":"abc123","platform":"ruby","rubygems_mfa_required":true}]`))
	f.Add([]byte(`[{"number":"0.1.0.pre","created_at":"2024-01-01","downloads_count":0,"authors":"","prerelease":true,"yanked":false,"sha256":"","platform":"ruby","rubygems_mfa_required":false}]`))
	// Many versions
	f.Add([]byte(`[` + strings.Repeat(`{"number":"1.0","created_at":"2024-01-01","downloads_count":1,"authors":"a","prerelease":false,"yanked":false,"sha256":"x","platform":"ruby","rubygems_mfa_required":false},`, 50) + `{"number":"2.0","created_at":"2024-06-01","downloads_count":1,"authors":"a","prerelease":false,"yanked":false,"sha256":"x","platform":"ruby","rubygems_mfa_required":false}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var versions []VersionEntry
		if err := json.Unmarshal(data, &versions); err != nil {
			return
		}

		for i, v := range versions {
			vStrings := []string{v.Number, v.CreatedAt, v.Authors, v.SHA, v.Platform}
			for _, s := range vStrings {
				if !utf8.ValidString(s) {
					t.Errorf("Versions[%d] field is invalid UTF-8: %q", i, s)
				}
			}
		}
	})
}

// --- FuzzOwnerEntryUnmarshal ---

func FuzzOwnerEntryUnmarshal(f *testing.F) {
	f.Add([]byte(`[{"handle":"tenderlove"},{"handle":"flavorjones"}]`))
	f.Add([]byte(`[{"handle":""}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var owners []OwnerEntry
		if err := json.Unmarshal(data, &owners); err != nil {
			return
		}

		for i, o := range owners {
			if !utf8.ValidString(o.Handle) {
				t.Errorf("Owners[%d].Handle is invalid UTF-8: %q", i, o.Handle)
			}
		}
	})
}
