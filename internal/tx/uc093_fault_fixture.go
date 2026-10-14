//go:build yzagmgr_uc093_fixture

package tx

import "errors"

// ApplyWithUC093FixtureFault exists only in the dedicated UC-093 fixture build.
// It executes the real transaction and uses the package-private hook to place
// an OS filesystem failure between a successful write and rollback.
func ApplyWithUC093FixtureFault(lease *Lease, opts Options, afterWrite func(string)) (Result, error) {
	if afterWrite == nil {
		return Result{}, errors.New("UC-093 fixture requires an after-write hook")
	}
	opts.faultAfterWrite = afterWrite
	return Apply(lease, opts)
}
