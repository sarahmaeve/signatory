package cargo

// CrateResponse models crates.io's /api/v1/crates/{name} response.
// Phase A modeled only the Crate top-level; Phase B adds Versions for
// longitudinal signals and per-version metadata.
type CrateResponse struct {
	Crate    Crate     `json:"crate"`
	Versions []Version `json:"versions"`
}

// Crate is the top-level crate metadata.
type Crate struct {
	Name            string `json:"name"`
	Repository      string `json:"repository"`
	Homepage        string `json:"homepage"`
	Documentation   string `json:"documentation"`
	Downloads       int    `json:"downloads"`
	RecentDownloads int    `json:"recent_downloads"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	MaxVersion      string `json:"max_version"`
	MaxStableVer    string `json:"max_stable_version"`
}

// Version is a per-version record from the versions[] array in the
// crate response. crates.io includes all published versions (including
// yanked) in this list.
type Version struct {
	Num            string              `json:"num"`
	Yanked         bool                `json:"yanked"`
	License        string              `json:"license"`
	CrateSize      int                 `json:"crate_size"`
	CreatedAt      string              `json:"created_at"`
	PublishedBy    *User               `json:"published_by"` // nullable for old versions
	Checksum       string              `json:"checksum"`
	Features       map[string][]string `json:"features"`
	HasBuildScript bool                `json:"has_build_script"` // per-version build.rs presence
}

// User is a crates.io user reference embedded in version records.
type User struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

// DependenciesResponse models crates.io's
// /api/v1/crates/{name}/{version}/dependencies response. crates.io
// returns only the directly-declared dependencies for that exact
// version — never the resolved transitive graph — so there is no
// indirect-dependency surface to model here.
type DependenciesResponse struct {
	Dependencies []Dependency `json:"dependencies"`
}

// Dependency is one declared dependency edge from a crate version.
// CrateID is the depended-upon crate's name. Kind partitions the
// edge into "normal" (runtime, shipped to consumers), "build"
// (compiled and executed at build time via build.rs on the
// consumer's machine), or "dev" (used only by the crate's own tests
// and benchmarks, never pulled transitively by consumers).
type Dependency struct {
	CrateID  string `json:"crate_id"`
	Req      string `json:"req"`
	Kind     string `json:"kind"` // "normal" | "build" | "dev"
	Optional bool   `json:"optional"`
}

// OwnersResponse models crates.io's /api/v1/crates/{name}/owners response.
type OwnersResponse struct {
	Users []Owner `json:"users"`
}

// Owner is a crates.io owner entry — either a user or a team.
type Owner struct {
	Login string `json:"login"`
	Kind  string `json:"kind"` // "user" or "team"
	Name  string `json:"name"`
}
