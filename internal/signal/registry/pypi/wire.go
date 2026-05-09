package pypi

// Project models the subset of PyPI's /pypi/<name>/json response
// signatory reads. The legacy JSON endpoint returns "info" (project
// metadata), "releases" (the full historical version map), and "urls"
// (the latest release's distribution files); "urls" is deliberately
// unmodelled — the json package skips unknown fields by default, so
// it's decoded but not allocated.
//
// Releases is modelled as a map from version string to a slice of
// distribution records. Each version can have multiple distributions
// (sdist, wheel, etc.); we read the upload_time_iso_8601 from the
// first entry to derive timestamps for last_publish and burst signals.
type Project struct {
	Info     Info                      `json:"info"`
	Releases map[string][]Distribution `json:"releases"`
}

// Distribution models one distribution file within a release.
// Fields beyond the ones modelled here (size, etc.) are skipped by
// the JSON decoder's unknown-field policy.
//
// URL and Digests are consumed by the artifact_url handoff to the
// artifact-vs-repo collector — sdist URL plus its sha256 are the
// minimum the downstream fetcher needs to fetch the bytes and
// (eventually) cross-check the integrity.
type Distribution struct {
	UploadTimeISO string  `json:"upload_time_iso_8601"`
	Yanked        bool    `json:"yanked"`
	PackageType   string  `json:"packagetype"`
	HasSig        bool    `json:"has_sig"`
	Filename      string  `json:"filename"`
	URL           string  `json:"url"`
	Digests       Digests `json:"digests"`
}

// Digests carries the per-distribution hash set PyPI publishes
// alongside each artifact. Only sha256 is read today; md5 and
// blake2b_256 are deliberately unmodelled — md5 is cryptographically
// dead and not worth carrying, blake2b_256 has no current consumer.
type Digests struct {
	SHA256 string `json:"sha256"`
}

// AttestationResponse models the PyPI Integrity API response at
// /integrity/<project>/<version>/<filename>/provenance.
// See PEP 740 and https://docs.pypi.org/api/integrity/.
type AttestationResponse struct {
	Version int                 `json:"version"`
	Bundles []AttestationBundle `json:"attestation_bundles"`
}

// AttestationBundle contains a publisher identity and its associated
// attestation envelopes. We only read the publisher block for signal
// emission — the envelope/verification_material are used by verifiers,
// not by presence-detection.
type AttestationBundle struct {
	Publisher AttestationPublisher `json:"publisher"`
}

// AttestationPublisher identifies the OIDC identity that published
// the distribution. Kind is typically "GitHub" or "GitLab"; the
// remaining fields locate the CI workflow that produced the artifact.
type AttestationPublisher struct {
	Kind        string `json:"kind"`
	Repository  string `json:"repository"`
	Workflow    string `json:"workflow"`
	Environment string `json:"environment"`
}

// Info is the project-level metadata block. Modelled today:
//
//   - ProjectURLs: free-form publisher-supplied map. Keys vary
//     wildly (Repository, Source, Source Code, Homepage, Code,
//     GitHub, Repo, …); the priority lookup in resolve.go walks a
//     fixed key order to pick the most-likely-correct repo URL.
//   - HomePage: the deprecated PEP 621 predecessor of project_urls.
//     Still populated on older releases and used as the final
//     fallback when no project_urls key resolves.
//   - Author / AuthorEmail / Maintainer / MaintainerEmail: legacy
//     PEP 621 single-string fields. Publisher-supplied free text:
//     historically a comma-separated list of human-readable names
//     ("Saurabh Kumar" or "Some Person, Other Person") with optional
//     <email@addr> wrappers. The collector parses these
//     conservatively for publisher-entity minting (collector.go,
//     extractPyPILogins) — login-shaped values only, free-text
//     display names are rejected.
//   - Maintainers: PEP 639 / Trove-style multi-maintainer list. Each
//     entry is a {name, email} object. Newer registry responses
//     populate this; legacy responses leave it nil and use the
//     single-string Maintainer field above.
//
// Other fields the full Layer 5 collector will eventually want
// (requires_python, license, version, downloads, …) land here
// additively when those signals come online.
type Info struct {
	ProjectURLs     map[string]string `json:"project_urls"`
	HomePage        string            `json:"home_page"`
	Author          string            `json:"author"`
	AuthorEmail     string            `json:"author_email"`
	Maintainer      string            `json:"maintainer"`
	MaintainerEmail string            `json:"maintainer_email"`
	Maintainers     []Person          `json:"maintainers"`
}

// Person models one entry in PyPI's PEP 639-style maintainers /
// authors list (the multi-entry parallel to the legacy single-string
// Author / Maintainer fields). Both fields are publisher-supplied;
// Name is the conventional carrier of the registry login when the
// publisher chose to use one rather than a display name. The
// collector applies the same login-shape filter as for the legacy
// fields (extractPyPILogins).
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
