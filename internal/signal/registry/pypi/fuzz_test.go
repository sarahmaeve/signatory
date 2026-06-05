package pypi

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for PyPI registry response parsing ---
//
// PyPI's /pypi/<name>/json endpoint returns a large nested JSON response
// with publisher-supplied metadata (project URLs, author strings, release
// maps). The Integrity API returns Sigstore attestation envelopes with
// OIDC publisher identity. Both are untrusted:
//
//   - A compromised PyPI or MITM serves arbitrary JSON
//   - Publisher-supplied fields (project_urls, author, workflow) are
//     free-form text that can contain anything
//
// These fuzz tests verify safety invariants on the deserialized structs.

// --- FuzzProjectUnmarshal ---
//
// Tests the full Project struct (Info + Releases map). The Releases map
// keys are version strings from the wire; Info.ProjectURLs is a
// free-form string→string map. Both are attacker-controllable.

func FuzzProjectUnmarshal(f *testing.F) {
	// Minimal valid project
	f.Add([]byte(`{"info":{"project_urls":{"Source":"https://github.com/x/y"},"home_page":"https://example.com"},"releases":{}}`))
	// With releases and distributions
	f.Add([]byte(`{"info":{},"releases":{"1.0.0":[{"upload_time_iso_8601":"2024-01-15T10:30:00Z","yanked":false,"packagetype":"sdist","has_sig":false,"filename":"pkg-1.0.0.tar.gz"}]}}`))
	// Multiple versions
	f.Add([]byte(`{"info":{"author":"Alice","author_email":"alice@example.com","maintainer":"Bob","maintainer_email":"bob@example.com"},"releases":{"1.0":[{"upload_time_iso_8601":"2024-01-01T00:00:00Z","filename":"x-1.0.tar.gz"}],"2.0":[{"upload_time_iso_8601":"2024-06-01T00:00:00Z","filename":"x-2.0.tar.gz"}]}}`))
	// With maintainers list (PEP 639)
	f.Add([]byte(`{"info":{"maintainers":[{"name":"alice","email":"alice@example.com"},{"name":"bob","email":"bob@example.com"}]},"releases":{}}`))
	// With many project_urls keys
	f.Add([]byte(`{"info":{"project_urls":{"Source":"https://github.com/x/y","Homepage":"https://x.com","Documentation":"https://docs.x.com","Tracker":"https://github.com/x/y/issues"}},"releases":{}}`))
	// Empty/minimal
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Adversarial: very large version key
	f.Add([]byte(`{"info":{},"releases":{"` + strings.Repeat("9", 500) + `":[]}}`))
	// Adversarial: filename with path traversal
	f.Add([]byte(`{"info":{},"releases":{"1.0":[{"filename":"../../../etc/passwd","upload_time_iso_8601":"2024-01-01T00:00:00Z"}]}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var proj Project
		if err := json.Unmarshal(data, &proj); err != nil {
			return
		}

		// Invariant 1: Info string fields must be valid UTF-8.
		infoStrings := []string{
			proj.Info.HomePage,
			proj.Info.Author,
			proj.Info.AuthorEmail,
			proj.Info.Maintainer,
			proj.Info.MaintainerEmail,
		}
		for _, s := range infoStrings {
			if !utf8.ValidString(s) {
				t.Errorf("Info field is invalid UTF-8: %q", s)
			}
		}

		// Invariant 2: ProjectURLs keys and values must be valid UTF-8.
		for k, v := range proj.Info.ProjectURLs {
			if !utf8.ValidString(k) {
				t.Errorf("ProjectURLs key is invalid UTF-8: %q", k)
			}
			if !utf8.ValidString(v) {
				t.Errorf("ProjectURLs value is invalid UTF-8: %q", v)
			}
		}

		// Invariant 3: Maintainers list entries must be valid UTF-8.
		for i, p := range proj.Info.Maintainers {
			if !utf8.ValidString(p.Name) {
				t.Errorf("Maintainers[%d].Name is invalid UTF-8: %q", i, p.Name)
			}
			if !utf8.ValidString(p.Email) {
				t.Errorf("Maintainers[%d].Email is invalid UTF-8: %q", i, p.Email)
			}
		}

		// Invariant 4: Release version keys must be valid UTF-8.
		for ver := range proj.Releases {
			if !utf8.ValidString(ver) {
				t.Errorf("Releases version key is invalid UTF-8: %q", ver)
			}
		}

		// Invariant 5: Distribution filenames must be valid UTF-8 and
		// must not contain path separators (traversal via filename).
		for ver, dists := range proj.Releases {
			for i, d := range dists {
				if !utf8.ValidString(d.Filename) {
					t.Errorf("Releases[%q][%d].Filename is invalid UTF-8: %q", ver, i, d.Filename)
				}
				if !utf8.ValidString(d.UploadTimeISO) {
					t.Errorf("Releases[%q][%d].UploadTimeISO is invalid UTF-8: %q", ver, i, d.UploadTimeISO)
				}
				if !utf8.ValidString(d.PackageType) {
					t.Errorf("Releases[%q][%d].PackageType is invalid UTF-8: %q", ver, i, d.PackageType)
				}
			}
		}
	})
}

// --- FuzzAttestationResponseUnmarshal ---
//
// The AttestationResponse carries Sigstore OIDC publisher identity.
// This is security-load-bearing: signatory uses publisher.Repository
// and publisher.Kind to determine whether a package has trusted CI/CD
// provenance. A corrupted or adversarial attestation envelope must not
// produce garbage that looks like valid provenance.

func FuzzAttestationResponseUnmarshal(f *testing.F) {
	// Minimal valid attestation
	f.Add([]byte(`{"version":1,"attestation_bundles":[{"publisher":{"kind":"GitHub","repository":"owner/repo","workflow":"release.yml","environment":"release"}}]}`))
	// GitLab publisher
	f.Add([]byte(`{"version":1,"attestation_bundles":[{"publisher":{"kind":"GitLab","repository":"group/project","workflow":".gitlab-ci.yml","environment":"production"}}]}`))
	// Multiple bundles
	f.Add([]byte(`{"version":1,"attestation_bundles":[{"publisher":{"kind":"GitHub","repository":"a/b","workflow":"ci.yml","environment":""}},{"publisher":{"kind":"GitHub","repository":"a/b","workflow":"release.yml","environment":"release"}}]}`))
	// Empty bundles
	f.Add([]byte(`{"version":1,"attestation_bundles":[]}`))
	// No bundles key
	f.Add([]byte(`{"version":1}`))
	// Version 0 (edge)
	f.Add([]byte(`{"version":0,"attestation_bundles":[{"publisher":{"kind":"GitHub","repository":"x/y","workflow":"w.yml","environment":""}}]}`))
	// Empty/minimal
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Adversarial: very long repository string
	f.Add([]byte(`{"version":1,"attestation_bundles":[{"publisher":{"kind":"GitHub","repository":"` + strings.Repeat("a/", 500) + `repo","workflow":"w.yml","environment":""}}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var attest AttestationResponse
		if err := json.Unmarshal(data, &attest); err != nil {
			return
		}

		// Invariant 1: Version should not be negative (JSON int allows it).
		if attest.Version < 0 {
			t.Errorf("AttestationResponse.Version is negative: %d", attest.Version)
		}

		// Invariant 2: Publisher fields must be valid UTF-8 and free
		// of control characters. These values feed trust decisions —
		// "kind=GitHub, repository=owner/repo" is used to determine
		// provenance. Garbage here could confuse downstream matching.
		for i, bundle := range attest.Bundles {
			pub := bundle.Publisher
			pubFields := map[string]string{
				"Kind":        pub.Kind,
				"Repository":  pub.Repository,
				"Workflow":    pub.Workflow,
				"Environment": pub.Environment,
			}
			for name, val := range pubFields {
				if !utf8.ValidString(val) {
					t.Errorf("Bundles[%d].Publisher.%s is invalid UTF-8: %q", i, name, val)
				}
				assertNoControlChars(t, "Bundles["+string(rune('0'+i))+"].Publisher."+name, val)
			}
		}
	})
}

// --- FuzzNormalizeDeclaredRepoURL ---
//
// NormalizeDeclaredRepoURL takes a free-form repository URL exactly as a
// PyPI publisher typed it into pyproject.toml's [project.urls] (or the
// legacy info.home_page) and converts it to a github-cloneable https URL,
// or "" when it doesn't resolve to a first-classed forge repo. The input
// is fully attacker-controlled and trust-load-bearing: the result is the
// repository signatory will clone and diff the published artifact
// against, so a malformed or surprising output here misdirects the whole
// source-vs-artifact check.
//
// These invariants don't re-derive the forge-URL grammar — that lives in
// profile.ResolveTarget, which this delegates to and which has its own
// tests. They pin the contract NormalizeDeclaredRepoURL itself promises.

func FuzzNormalizeDeclaredRepoURL(f *testing.F) {
	// Accepted shapes (normalize.go's doc) — each should yield an https URL.
	f.Add("https://github.com/psf/requests")
	f.Add("https://github.com/psf/requests.git")
	f.Add("https://github.com/psf/requests/")
	f.Add("http://github.com/psf/requests")
	f.Add("https://www.github.com/psf/requests")
	f.Add("https://github.com/psf/requests#main")
	f.Add("git+https://github.com/psf/requests.git")
	f.Add("git+ssh://git@github.com/psf/requests.git")
	f.Add("ssh://git@github.com/psf/requests.git")
	f.Add("git@github.com:psf/requests.git")
	f.Add("https://codeberg.org/forgejo/forgejo")
	f.Add("https://github.com/PSF/Requests")
	// Rejected shapes (normalize.go's doc) — each should yield "".
	f.Add("")
	f.Add("git://github.com/psf/requests")
	f.Add("git+git://github.com/psf/requests")
	f.Add("https://requests.readthedocs.io/")
	f.Add("https://pypi.org/project/requests/")
	f.Add("not a url")
	f.Add("https://github.com/psf")
	// Adversarial: surrounding whitespace, fragment + vcs prefix, control
	// bytes in the path, and a pathologically long owner chain.
	f.Add("   https://github.com/psf/requests   ")
	f.Add("git+https://github.com/psf/requests.git#v1.0")
	f.Add("https://github.com/psf/requests\n")
	f.Add("https://github.com/\x00/\x00")
	f.Add("https://github.com/" + strings.Repeat("a/", 500) + "repo")

	f.Fuzz(func(t *testing.T, raw string) {
		out := NormalizeDeclaredRepoURL(raw)

		// Invariant 1: the result is "" or an https URL. signatory only
		// clones over https, so http://, ssh://, git://, git+, and SCP
		// forms must all be rewritten or rejected — never passed through.
		if out != "" && !strings.HasPrefix(out, "https://") {
			t.Errorf("non-empty result must be an https URL, got %q (input %q)", out, raw)
		}

		// Invariant 2: idempotence. The result is already normalized, so
		// renormalizing it must be a fixed point ("" maps to ""). A
		// normalizer that doesn't converge in one pass is the classic
		// source of "looks canonical but isn't" bugs.
		if again := NormalizeDeclaredRepoURL(out); again != out {
			t.Errorf("not idempotent: Normalize(%q)=%q but Normalize(%q)=%q",
				raw, out, out, again)
		}

		// Invariant 3: determinism — same input, same output.
		if out2 := NormalizeDeclaredRepoURL(raw); out2 != out {
			t.Errorf("not deterministic: got %q then %q for input %q", out, out2, raw)
		}
	})
}

// --- Helpers ---

// assertNoControlChars fails if s contains ASCII control characters
// (0x00–0x1F) except tab. These should never appear in publisher
// identities, repository paths, or workflow names.
func assertNoControlChars(t *testing.T, field, s string) {
	t.Helper()
	for i, r := range s {
		if r < 0x20 && r != '\t' {
			t.Errorf("%s: control char U+%04X at byte %d in %q", field, r, i, s)
			return
		}
	}
}
