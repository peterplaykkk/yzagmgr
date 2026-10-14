//go:build !yzagmgr_uc093_fixture

package api

import "github.com/peterplaykkk/yzagmgr/internal/tx"

// applyTransaction is the API-to-transaction boundary. Ordinary daemon builds
// forward directly to tx.Apply; no fault controls are present in this build.
func (s *Server) applyTransaction(lease *tx.Lease, opts tx.Options) (tx.Result, error) {
	return tx.Apply(lease, opts)
}
