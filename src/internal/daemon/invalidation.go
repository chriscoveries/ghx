package daemon

import (
	"strings"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/cache"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
)

func (h *Handler) invalidateWrite(req *protocol.Request) {
	d := resource.Identify(req.Context, req.Args)
	collectionsOnly := len(req.Args) > 1 && req.Args[1] == "create"
	count := h.cache.Invalidate(func(e *cache.Entry) bool {
		if !strings.EqualFold(e.Host, d.Host) {
			return false
		}
		// Search and unresolved API reads can span repositories on the host.
		if e.Resource == allowlist.ResourceSearch || (e.Resource == allowlist.ResourceAPI && e.Repo == "") {
			return true
		}
		if d.Repo != "" && e.Repo != "" && !strings.EqualFold(e.Repo, d.Repo) {
			return false
		}
		if d.Kind == "api" || d.Kind == "repo" || d.Kind == "label" {
			return true
		}
		same := string(e.Resource) == d.Kind || e.Resource == allowlist.ResourceAPI
		// Pull requests also occupy issue endpoints (comments, labels, assignees).
		if d.Kind == "pr" && e.Resource == allowlist.ResourceIssue || d.Kind == "issue" && e.Resource == allowlist.ResourcePR {
			same = true
		}
		if d.Kind == "workflow" && e.Resource == allowlist.ResourceRun {
			same = true
		}
		if !same {
			return false
		}
		if collectionsOnly {
			return e.ResourceID == ""
		}
		return d.ID == "" || e.ResourceID == "" || e.ResourceID == d.ID
	})
	h.stats.RecordInvalidation(count)
}
