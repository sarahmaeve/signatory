package cargo

// CrateResponse models the subset of crates.io's /api/v1/crates/{name}
// response signatory's Layer 6 source-resolution slice reads. Phase B
// extends this with Versions, owners, downloads, etc.
type CrateResponse struct {
	Crate Crate `json:"crate"`
}

// Crate is the top-level crate metadata. Phase A models only the
// fields the source resolver needs; Phase B adds Downloads,
// RecentDownloads, MaxVersion, etc.
type Crate struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Homepage   string `json:"homepage"`
}
