// Package gem implements the rubygems.org registry signal collector.
// It queries the public JSON API to emit supply-chain trust signals
// for Ruby gems: publication freshness, download volume, maintainer
// governance, and native-extension anomaly detection.
package gem

// GemResponse is the JSON shape returned by GET /api/v1/gems/{name}.json.
type GemResponse struct {
	Name             string          `json:"name"`
	Downloads        int             `json:"downloads"`
	VersionDownloads int             `json:"version_downloads"`
	Version          string          `json:"version"`
	VersionCreatedAt string          `json:"version_created_at"`
	CreatedAt        string          `json:"created_at"`
	Authors          string          `json:"authors"`
	Info             string          `json:"info"`
	Licenses         []string        `json:"licenses"`
	SourceCodeURI    string          `json:"source_code_uri"`
	HomepageURI      string          `json:"homepage_uri"`
	ChangelogURI     string          `json:"changelog_uri"`
	BugTrackerURI    string          `json:"bug_tracker_uri"`
	MFARequired      bool            `json:"mfa_required"`
	Dependencies     GemDependencies `json:"dependencies"`
}

// GemDependencies is the `dependencies` object in the gem JSON
// response. rubygems.org reports the displayed (latest) version's
// declared dependencies split into runtime (consumed by anything that
// depends on the gem) and development (the gem's own test/build
// tooling — the dev analog, not pulled transitively by consumers).
type GemDependencies struct {
	Runtime     []GemDependency `json:"runtime"`
	Development []GemDependency `json:"development"`
}

// GemDependency is one entry in a GemDependencies list. Name is the
// depended-upon gem; Requirements is the version constraint string
// (e.g. ">= 2.2.4"), carried for completeness but not part of the
// tracked identity.
type GemDependency struct {
	Name         string `json:"name"`
	Requirements string `json:"requirements"`
}

// VersionEntry is one entry from GET /api/v1/versions/{name}.json.
type VersionEntry struct {
	Number              string `json:"number"`
	CreatedAt           string `json:"created_at"`
	DownloadsCount      int    `json:"downloads_count"`
	Authors             string `json:"authors"`
	Prerelease          bool   `json:"prerelease"`
	Yanked              bool   `json:"yanked"`
	SHA                 string `json:"sha256"`
	Platform            string `json:"platform"`
	RubygemsMFARequired bool   `json:"rubygems_mfa_required"`
}

// OwnerEntry is one entry from GET /api/v1/gems/{name}/owners.json.
type OwnerEntry struct {
	Handle string `json:"handle"`
}
