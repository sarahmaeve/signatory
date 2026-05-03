// Package gem implements the rubygems.org registry signal collector.
// It queries the public JSON API to emit supply-chain trust signals
// for Ruby gems: publication freshness, download volume, maintainer
// governance, and native-extension anomaly detection.
package gem

// GemResponse is the JSON shape returned by GET /api/v1/gems/{name}.json.
type GemResponse struct {
	Name             string   `json:"name"`
	Downloads        int      `json:"downloads"`
	VersionDownloads int      `json:"version_downloads"`
	Version          string   `json:"version"`
	VersionCreatedAt string   `json:"version_created_at"`
	CreatedAt        string   `json:"created_at"`
	Authors          string   `json:"authors"`
	Info             string   `json:"info"`
	Licenses         []string `json:"licenses"`
	SourceCodeURI    string   `json:"source_code_uri"`
	HomepageURI      string   `json:"homepage_uri"`
	ChangelogURI     string   `json:"changelog_uri"`
	BugTrackerURI    string   `json:"bug_tracker_uri"`
	MFARequired      bool     `json:"mfa_required"`
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
