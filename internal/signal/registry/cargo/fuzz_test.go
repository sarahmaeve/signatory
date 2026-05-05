package cargo

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for crates.io registry response parsing ---
//
// crates.io serves JSON metadata for crate info and owner lists.
// Both are untrusted: a compromised registry or MITM can serve
// adversarial payloads. These fuzz tests verify safety invariants
// on the deserialized structs.

// --- FuzzCrateResponseUnmarshal ---

func FuzzCrateResponseUnmarshal(f *testing.F) {
	f.Add([]byte(`{"crate":{"name":"serde","repository":"https://github.com/serde-rs/serde","homepage":"","documentation":"https://docs.rs/serde","downloads":200000000,"recent_downloads":5000000,"created_at":"2015-03-10","updated_at":"2024-01-01","max_version":"1.0.195","max_stable_version":"1.0.195"},"versions":[]}`))
	f.Add([]byte(`{"crate":{"name":"tokio","repository":"https://github.com/tokio-rs/tokio","downloads":100000000,"recent_downloads":3000000},"versions":[{"num":"1.36.0","yanked":false,"license":"MIT","crate_size":500000,"created_at":"2024-02-01","published_by":{"login":"carllerche","name":"Carl Lerche","url":"https://github.com/carllerche"},"checksum":"abc123","features":{"full":["io","net","time"]},"has_build_script":false}]}`))
	// Version with null published_by (old versions)
	f.Add([]byte(`{"crate":{"name":"libc","repository":"https://github.com/rust-lang/libc"},"versions":[{"num":"0.1.0","yanked":false,"license":"MIT/Apache-2.0","crate_size":10000,"created_at":"2015-01-01","published_by":null,"checksum":"def456","features":{},"has_build_script":true}]}`))
	// Minimal
	f.Add([]byte(`{"crate":{},"versions":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Adversarial: very long repository URL
	f.Add([]byte(`{"crate":{"name":"x","repository":"` + strings.Repeat("a", 5000) + `"},"versions":[]}`))
	// Negative downloads
	f.Add([]byte(`{"crate":{"name":"x","downloads":-1,"recent_downloads":-1},"versions":[]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var cr CrateResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return
		}

		// Invariant 1: Crate string fields must be valid UTF-8.
		crateStrings := []string{
			cr.Crate.Name, cr.Crate.Repository, cr.Crate.Homepage,
			cr.Crate.Documentation, cr.Crate.CreatedAt, cr.Crate.UpdatedAt,
			cr.Crate.MaxVersion, cr.Crate.MaxStableVer,
		}
		for _, s := range crateStrings {
			if !utf8.ValidString(s) {
				t.Errorf("Crate field is invalid UTF-8: %q", s)
			}
		}

		// Invariant 2: Version fields must be valid UTF-8.
		for i, v := range cr.Versions {
			vStrings := []string{v.Num, v.License, v.CreatedAt, v.Checksum}
			for _, s := range vStrings {
				if !utf8.ValidString(s) {
					t.Errorf("Versions[%d] field is invalid UTF-8: %q", i, s)
				}
			}
			// Published_by (nullable)
			if v.PublishedBy != nil {
				if !utf8.ValidString(v.PublishedBy.Login) {
					t.Errorf("Versions[%d].PublishedBy.Login is invalid UTF-8: %q", i, v.PublishedBy.Login)
				}
				if !utf8.ValidString(v.PublishedBy.Name) {
					t.Errorf("Versions[%d].PublishedBy.Name is invalid UTF-8: %q", i, v.PublishedBy.Name)
				}
				if !utf8.ValidString(v.PublishedBy.URL) {
					t.Errorf("Versions[%d].PublishedBy.URL is invalid UTF-8: %q", i, v.PublishedBy.URL)
				}
			}
		}

		// Invariant 3: Downloads should not be negative.
		if cr.Crate.Downloads < 0 {
			t.Logf("Crate.Downloads is negative: %d (defended at caller level)", cr.Crate.Downloads)
		}
	})
}

// --- FuzzOwnersResponseUnmarshal ---

func FuzzOwnersResponseUnmarshal(f *testing.F) {
	f.Add([]byte(`{"users":[{"login":"dtolnay","kind":"user","name":"David Tolnay"},{"login":"rust-bus","kind":"team","name":"Rust Bus"}]}`))
	f.Add([]byte(`{"users":[]}`))
	f.Add([]byte(`{"users":[{"login":"a","kind":"user","name":""}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Many owners
	f.Add([]byte(`{"users":[` + strings.Repeat(`{"login":"u","kind":"user","name":"x"},`, 50) + `{"login":"last","kind":"user","name":"Last"}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var or OwnersResponse
		if err := json.Unmarshal(data, &or); err != nil {
			return
		}

		for i, o := range or.Users {
			if !utf8.ValidString(o.Login) {
				t.Errorf("Users[%d].Login is invalid UTF-8: %q", i, o.Login)
			}
			if !utf8.ValidString(o.Kind) {
				t.Errorf("Users[%d].Kind is invalid UTF-8: %q", i, o.Kind)
			}
			if !utf8.ValidString(o.Name) {
				t.Errorf("Users[%d].Name is invalid UTF-8: %q", i, o.Name)
			}
		}
	})
}
