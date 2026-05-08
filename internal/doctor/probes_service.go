package doctor

import (
	"fmt"
	"time"
)

// probePipelineService reports whether the local pipeline message
// service is currently listening. Unlike the other probes, "not
// listening" is the EXPECTED steady state — `signatory serve` is
// on-demand and most users will not have it running when they
// invoke `doctor`. We surface the answer in Message but keep the
// Status at OK for both branches; the user only needs to act on
// this if they tried to start the service and it didn't come up.
//
// Trade-off: this means doctor never raises a flag for a wedged
// service. Users who suspect that path should run `signatory serve
// status` directly, which knows about the pidfile and the binary-
// recycling defense; doctor's role here is informational.
func probePipelineService(r resolved) Result {
	const name = "pipeline-service"
	const probeTimeout = 500 * time.Millisecond

	listening := r.probePort(r.pipelinePort, probeTimeout)
	if listening {
		return Result{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("listening on 127.0.0.1:%d", r.pipelinePort),
		}
	}
	return Result{
		Name:   name,
		Status: StatusOK,
		Message: fmt.Sprintf(
			"not running on 127.0.0.1:%d (the service is on-demand; start with `signatory serve start` when you need /analyze)",
			r.pipelinePort),
	}
}
