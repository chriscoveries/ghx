package daemon

import "github.com/brunoborges/ghx/src/internal/protocol"

func (h *Handler) invalidateWrite(_ *protocol.Request) {
	// Native gh's repository/host selection is not source-bound for every
	// mutation shape. Invalidate globally rather than missing its target.
	h.stats.RecordInvalidation(h.cache.Flush())
}
