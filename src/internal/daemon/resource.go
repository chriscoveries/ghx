package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/authenv"
	"github.com/brunoborges/ghx/src/internal/cache"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/metrics"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
)

func (h *Handler) handleResource(req *protocol.Request, s *resource.Shape, classification allowlist.Classification) *protocol.Response {
	start := time.Now()
	key := execctx.CacheKey(s.Context, s.Args)
	entry := h.cache.Peek(key)
	longTTL := h.cfg.ImmutableTTL
	// An explicit override controls freshness even for immutable projections.
	immutable := entry != nil && req.TTLOverride == 0 && time.Since(entry.CachedAt) < longTTL && s.Immutable(entry.Stdout)
	if entry != nil && (!entry.IsExpired() || immutable) {
		if _, err := s.Project(entry.Stdout); err == nil {
			h.stats.Record(classification.CmdKey, key, metrics.ResultHit, time.Since(start).Seconds()*1000)
			return h.renderResource(req, s, entry.Stdout, entry.Stderr, true)
		}
	}
	version := h.cache.Version()
	result, coalesced := h.doSingleflightExec(fmt.Sprintf("%s/%d", key, version), func() *executor.Result {
		// A caller may have observed a miss before the previous flight stored its bytes.
		if e := h.cache.Get(key); e != nil {
			return &executor.Result{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode}
		}
		r := h.execGH(s.Args, req.WorkDir, req.AuthEnv)
		if r.ExitCode == 0 && s.Valid(r.Stdout) {
			if _, err := s.Project(r.Stdout); err == nil {
				h.cache.SetVersion(&cache.Entry{Key: key, Stdout: r.Stdout, Stderr: r.Stderr, CachedAt: time.Now(), TTL: h.requestTTL(req, classification.CmdKey), Resource: classification.Resource, ResourceID: s.ID, Host: s.Context.Host, Repo: s.Context.Repo}, version)
			}
		}
		return r
	})
	status := metrics.ResultMiss
	if coalesced {
		status = metrics.ResultCoalesced
	}
	if result.ExitCode != 0 || !s.Valid(result.Stdout) {
		if result.ExitCode != 0 && upstreamUnavailable(result.Stderr) {
			h.stats.Record(classification.CmdKey, key, status, time.Since(start).Seconds()*1000)
			return &protocol.Response{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
		}
		// Unsupported CLI fields/permissions retain native execution. This diagnostic
		// exposes recurring fallbacks without logging repository names or request data.
		log.Printf("ghx: resource superset fallback command=%s", classification.CmdKey)
		return nil
	}
	if _, err := s.Project(result.Stdout); err != nil {
		log.Printf("ghx: resource projection fallback command=%s", classification.CmdKey)
		return nil
	}
	h.stats.Record(classification.CmdKey, key, status, time.Since(start).Seconds()*1000)
	return h.renderResource(req, s, result.Stdout, result.Stderr, coalesced)
}

func upstreamUnavailable(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "rate limit") || strings.Contains(s, "http 401") || strings.Contains(s, "http 403") || strings.Contains(s, "http 429") || strings.Contains(s, "connection refused") || strings.Contains(s, "timeout")
}

func (h *Handler) requestTTL(req *protocol.Request, cmdKey string) time.Duration {
	if req.TTLOverride > 0 {
		return time.Duration(req.TTLOverride) * time.Second
	}
	return h.cfg.CommandTTL(cmdKey)
}

func (h *Handler) renderResource(req *protocol.Request, s *resource.Shape, body, stderr []byte, cached bool) *protocol.Response {
	projected, err := s.Project(body)
	if err != nil {
		return &protocol.Response{Stderr: []byte("ghx: " + err.Error() + "\n"), ExitCode: 1}
	}
	if s.JQ == "" {
		return &protocol.Response{Stdout: projected, Stderr: stderr, Cached: cached}
	}
	r, err := resource.RenderFilter(s, projected, req.AuthEnv, func(args []string, env authenv.Environment) *executor.Result {
		return h.execute(context.Background(), h.GHPath(), args, req.WorkDir, env)
	})
	if err != nil {
		return &protocol.Response{Stderr: []byte("ghx: local renderer: " + err.Error() + "\n"), ExitCode: 1}
	}
	return &protocol.Response{Stdout: r.Stdout, Stderr: append(append([]byte{}, stderr...), r.Stderr...), ExitCode: r.ExitCode, Cached: cached}
}
