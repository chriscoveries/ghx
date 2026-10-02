package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/cache"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/metrics"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
)

func (h *Handler) handleConditional(req *protocol.Request) *protocol.Response {
	start := time.Now()
	key := execctx.CacheKey(req.Context, req.Args)
	if e := h.cache.Peek(key); e != nil && !e.IsExpired() {
		h.stats.Record("api_get", key, metrics.ResultHit, time.Since(start).Seconds()*1000)
		return &protocol.Response{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode, Cached: true}
	}
	return h.refreshConditional(req, key, start)
}

func (h *Handler) refreshConditional(req *protocol.Request, key string, start time.Time) *protocol.Response {
	version := h.cache.Version()
	reused := false
	result, coalesced := h.doSingleflightExec(fmt.Sprintf("%s/%d", key, version), func() *executor.Result {
		old := h.cache.Peek(key)
		if old != nil && !old.IsExpired() {
			return &executor.Result{Stdout: old.Stdout, Stderr: old.Stderr, ExitCode: old.ExitCode}
		}
		args := append(append([]string{}, req.Args...), "--include")
		if old != nil && old.ETag != "" {
			args = append(args, "--header", "If-None-Match: "+old.ETag)
		}
		r := h.execGH(args, req.WorkDir, req.AuthEnv)
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(r.Stdout)), nil)
		if err != nil {
			// Failures usually have no stdout. Preserve their native diagnostic. A
			// successful unparseable CLI output falls back without poisoning validators.
			if r.ExitCode != 0 {
				return r
			}
			log.Printf("ghx: conditional response fallback command=api_get")
			return h.execGH(req.Args, req.WorkDir, req.AuthEnv)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return &executor.Result{Stderr: []byte("ghx: conditional body: " + err.Error() + "\n"), ExitCode: 1}
		}
		tag := response.Header.Get("ETag")
		// Validators are metadata outside the response-byte budget. Bound them
		// independently so malformed upstream headers cannot bypass that budget.
		if len(tag) > 1024 {
			tag = ""
		}
		if response.StatusCode == http.StatusNotModified {
			if old == nil || old.ETag == "" || version != h.cache.Version() {
				return h.execGH(req.Args, req.WorkDir, req.AuthEnv)
			}
			body = old.Stdout
			if tag == "" {
				tag = old.ETag
			}
			r.ExitCode = 0
			reused = true
		}
		r.Stdout = body
		if r.ExitCode == 0 && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusNotModified) {
			d := resource.Identify(req.Context, req.Args)
			h.cache.SetVersion(&cache.Entry{Key: key, Stdout: body, Stderr: r.Stderr, CachedAt: time.Now(), TTL: h.requestTTL(req, "api_get"), ETag: tag, Host: d.Host, Repo: d.Repo, Resource: allowlist.ResourceType(d.Kind), ResourceID: d.ID}, version)
		}
		return r
	})
	status := metrics.ResultMiss
	if coalesced {
		status = metrics.ResultCoalesced
	} else if reused {
		status = metrics.ResultRevalidated
	}
	h.stats.Record("api_get", key, status, time.Since(start).Seconds()*1000)
	return &protocol.Response{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Cached: reused || coalesced}
}
