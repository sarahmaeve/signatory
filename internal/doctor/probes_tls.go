package doctor

// probeNodeExtraCACerts delegates to internal/certs.Check, the
// same call site `signatory certs check` and `serve start`
// preflight already use. Doctor wraps it so the user gets one
// unified report instead of having to run a separate command —
// but the truth lives in `certs`, and any future change to the
// check policy stays in one place.
func probeNodeExtraCACerts(r resolved) Result {
	const name = "node-extra-ca-certs"
	cr := r.certsCheck()
	if cr.OK {
		return Result{
			Name:    name,
			Status:  StatusOK,
			Message: cr.Message,
		}
	}
	// certs.CheckResult guarantees Fix is populated on failure.
	// Translating OK=false → fail (not warn) because every concrete
	// failure mode certs.Check distinguishes is something Claude Code
	// will trip over: the pipeline service over HTTPS becomes
	// unreachable, /analyze hits "unable to verify the first
	// certificate," and the workflow stops.
	return Result{
		Name:    name,
		Status:  StatusFail,
		Message: cr.Message,
		Fix:     cr.Fix,
	}
}
