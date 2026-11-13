package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/authenv"
	"github.com/brunoborges/ghx/src/internal/cache"
	"github.com/brunoborges/ghx/src/internal/config"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/ghcli"
	"github.com/brunoborges/ghx/src/internal/metrics"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
	"github.com/brunoborges/ghx/src/internal/shape"
)

// Handler processes incoming requests from clients.
type Handler struct {
	cfg        *config.Config
	cache      *cache.Cache
	classifier *allowlist.Classifier
	stats      *metrics.Stats

	// ghPath holds the resolved gh binary path, refreshed periodically.
	ghPath atomic.Value // stores string

	// singleflight: one in-flight request per cache key
	mu       sync.Mutex
	inflight map[string]*call
	execute  func(context.Context, string, []string, string, authenv.Environment) *executor.Result

	// fieldIndex maps a --json command's base key to cached field sets.
	fieldsMu   sync.Mutex
	fieldIndex map[string][]fieldEntry
}

type call struct {
	wg      sync.WaitGroup
	res     *executor.Result
	version uint64 // cache generation when the fetch started
}

func NewHandler(cfg *config.Config, c *cache.Cache, cl *allowlist.Classifier, s *metrics.Stats) *Handler {
	h := &Handler{
		cfg:        cfg,
		cache:      c,
		classifier: cl,
		stats:      s,
		inflight:   make(map[string]*call),
		fieldIndex: make(map[string][]fieldEntry),
		execute:    executor.Execute,
	}
	h.ghPath.Store(cfg.GHPath)
	c.OnEvict(func(key string) {
		s.RecordEviction()
	})
	return h
}

// GHPath returns the current resolved gh binary path.
func (h *Handler) GHPath() string {
	return h.ghPath.Load().(string)
}

// SetGHPath atomically updates the resolved gh binary path.
func (h *Handler) SetGHPath(path string) {
	h.ghPath.Store(path)
}

// execGH runs gh and retries once with a re-resolved path if the binary is not found.
func (h *Handler) execGH(args []string, workDir string, env authenv.Environment) *executor.Result {
	ghPath := h.GHPath()
	if executor.IsBinaryNotFound(ghPath) {
		if newPath := h.reResolveGHPath(); newPath != "" {
			ghPath = newPath
		}
	}
	return h.execute(context.Background(), ghPath, args, workDir, env)
}

// reResolveGHPath attempts to find a new gh binary and updates the stored path.
// Returns the new path on success or empty string on failure.
func (h *Handler) reResolveGHPath() string {
	resolved, err := ghcli.ResolveGHPath(h.cfg.GHPath)
	if err != nil {
		log.Printf("gh re-resolve failed: %v", err)
		return ""
	}
	current := h.GHPath()
	if resolved != current {
		log.Printf("gh path updated: %s -> %s", current, resolved)
		h.SetGHPath(resolved)
	}
	return resolved
}

// Handle processes a single client request and returns a response.
func (h *Handler) Handle(req *protocol.Request) *protocol.Response {
	switch req.Type {
	case protocol.TypeStats:
		return h.handleStats()
	case protocol.TypeFlush:
		return h.handleFlush(req)
	case protocol.TypeKeys:
		return h.handleKeys()
	case protocol.TypeShutdown:
		return &protocol.Response{Stdout: []byte("shutting down\n")}
	default:
		return h.handleExec(req)
	}
}

func (h *Handler) handleExec(req *protocol.Request) *protocol.Response {
	start := time.Now()

	classification := h.classifier.Classify(req.Args)
	cmdKey := classification.CmdKey
	if cmdKey == "" {
		cmdKey = sanitizeCmdKey(req.Args)
	}
	// The client fingerprint identifies its default host credential. An
	// explicit different host may use another credential, so do not share or
	// populate cache entries when that effective identity is unbound.
	defaultHost := req.Context.Host
	if defaultHost == "" {
		defaultHost = "github.com"
	}
	api := resource.ParseAPI(req.Args)
	absoluteAPI := len(req.Args) > 0 && req.Args[0] == "api" && (strings.Contains(api.Endpoint, "://") || strings.HasPrefix(api.Endpoint, "//"))
	literalAPI := len(req.Args) > 0 && req.Args[0] == "api" && api.Valid && !absoluteAPI && !strings.ContainsAny(api.Endpoint, "{}")
	repoHost, _ := resource.Repository(req.Context.Host, req.Context.Repo)
	ambiguousAPI := literalAPI && api.Host == "" && (!req.Context.RepoBound || !strings.EqualFold(repoHost, defaultHost))
	if literalAPI && !req.Context.RepoBound {
		// A literal API path cannot use the origin guess. Discard it before
		// cache-key and invalidation metadata are derived.
		copy := *req
		copy.Context.Repo = ""
		req = &copy
	}
	targetHost := resource.Identify(req.Context, req.Args).Host
	if literalAPI {
		targetHost = defaultHost
		if api.Host != "" {
			targetHost = api.Host
		}
	}
	unverifiedRepo := !req.Context.RepoBound && !literalAPI
	if classification.Type == allowlist.Cacheable && (absoluteAPI || ambiguousAPI || unverifiedRepo || req.Context.TokenHash == "" || !strings.EqualFold(targetHost, defaultHost)) {
		result := h.execGH(req.Args, req.WorkDir, req.AuthEnv)
		h.record(req, nil, cmdKey, "", metrics.ResultPassthrough, time.Since(start).Seconds()*1000)
		return &protocol.Response{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
	}
	if h.cfg.ResourceViews && !req.NoCache && classification.Type == allowlist.Cacheable {
		if shape := resource.Prepare(req.Context, req.Args); shape != nil {
			// Native fallbacks are exact-key cached. Reuse them before another
			// unsupported superset attempt on older CLI versions/permissions.
			if e := h.cache.Get(execctx.CacheKey(req.Context, req.Args)); e != nil {
				h.stats.Record(cmdKey, e.Key, metrics.ResultHit, time.Since(start).Seconds()*1000)
				return &protocol.Response{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode, Cached: true}
			}
			if response := h.handleResource(req, shape, classification); response != nil {
				return response
			}
		}
		if shape := resource.PrepareList(req.Context, req.Args); shape != nil {
			if e := h.cache.Get(execctx.CacheKey(req.Context, req.Args)); e != nil {
				h.stats.Record(cmdKey, e.Key, metrics.ResultHit, time.Since(start).Seconds()*1000)
				return &protocol.Response{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode, Cached: true}
			}
			// A collection read can carry validators, so an expired body is
			// revalidated instead of refetched. Anything unserviceable falls
			// through to the shared superset/native path below.
			if response := h.handleConditionalList(req, shape, cmdKey); response != nil {
				return response
			}
			if response := h.handleResource(req, shape, classification); response != nil {
				return response
			}
		}
	}

	// Non-cacheable: execute directly via daemon (captures output)
	if classification.Type == allowlist.Passthrough || (req.NoCache && classification.Type != allowlist.Mutation) {
		result := h.execGH(req.Args, req.WorkDir, req.AuthEnv)
		latency := time.Since(start).Seconds() * 1000
		h.record(req, nil, cmdKey, "", metrics.ResultPassthrough, latency)

		return &protocol.Response{
			Stdout:   result.Stdout,
			Stderr:   result.Stderr,
			ExitCode: result.ExitCode,
		}
	}

	// Mutation: execute directly, then invalidate
	if classification.Type == allowlist.Mutation {
		result := h.execGH(req.Args, req.WorkDir, req.AuthEnv)
		latency := time.Since(start).Seconds() * 1000
		h.record(req, nil, cmdKey, "", metrics.ResultPassthrough, latency)

		// A failed command may have partially changed remote state.
		h.invalidateWrite(req)

		return &protocol.Response{
			Stdout:   result.Stdout,
			Stderr:   result.Stderr,
			ExitCode: result.ExitCode,
		}
	}

	// Conditional api requests take our snapshot path.
	if req.Args[0] == "api" && resource.ParseAPI(req.Args).Conditional {
		return h.handleConditional(req)
	}
	// Non-cacheable: execute directly via daemon (captures output)
	if classification.Type == allowlist.Passthrough {
		result := h.execGH(req.Args, req.WorkDir, req.AuthEnv)
		latency := time.Since(start).Seconds() * 1000
		h.record(req, nil, cmdKey, "", metrics.ResultPassthrough, latency)

		return &protocol.Response{
			Stdout:   result.Stdout,
			Stderr:   result.Stderr,
			ExitCode: result.ExitCode,
		}
	}

	// Cacheable: check cache → singleflight → execute → store. The cache is
	// keyed on the command without output filters (--jq, --json field order),
	// which are re-applied to the shared response.
	plan := shape.Normalize(req.Args)
	kctx := keyContext(req.Context, classification)
	cacheKey := execctx.CacheKey(kctx, plan.Fetch)
	var baseKey string
	if plan.Base != nil {
		baseKey = execctx.CacheKey(kctx, plan.Base)
	}

	// Check cache, then any entry that fetched a superset of the fields
	if entry := h.cache.Get(cacheKey); entry != nil {
		h.record(req, plan.Fetch, cmdKey, cacheKey, metrics.ResultHit, time.Since(start).Seconds()*1000)
		return h.finish(plan, req, entryResult(entry), true)
	}
	if result := h.lookupSuperset(baseKey, plan.Fields); result != nil {
		h.record(req, plan.Fetch, cmdKey, cacheKey, metrics.ResultHit, time.Since(start).Seconds()*1000)
		return h.finish(plan, req, result, true)
	}

	// Singleflight: coalesce concurrent requests for the same key. The store
	// fence uses the generation the fetch started at, so a caller joining a
	// pre-invalidation fetch cannot store its stale result under its own
	// newer generation.
	result, version, coalesced := h.doSingleflight(cacheKey, plan.Fetch, req)
	latency := time.Since(start).Seconds() * 1000

	if coalesced {
		h.record(req, plan.Fetch, cmdKey, cacheKey, metrics.ResultCoalesced, latency)
	} else {
		h.record(req, plan.Fetch, cmdKey, cacheKey, metrics.ResultMiss, latency)
	}

	h.store(cacheKey, cmdKey, classification, req, result, version)
	h.indexFields(baseKey, cacheKey, plan.Fields)

	return h.finish(plan, req, result, false)
}

func (h *Handler) record(req *protocol.Request, fetch []string, cmdKey, cacheKey string, result metrics.RequestResult, latencyMs float64) {
	if !h.cfg.LogArgs {
		h.stats.Record(cmdKey, cacheKey, result, latencyMs)
		return
	}
	args := req.Args
	if req.NoCache {
		args = append([]string{"--no-cache"}, args...)
	}
	if slices.Equal(fetch, req.Args) {
		fetch = nil
	}
	h.stats.RecordDetailed(cmdKey, cacheKey, result, latencyMs, metrics.RedactArgs(args), metrics.RedactArgs(fetch))
}

func entryResult(e *cache.Entry) *executor.Result {
	return &executor.Result{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode}
}

// finish applies the caller's --jq filter to a fetched result. gh reports
// failures before filtering, so on any error the original command runs
// directly to reproduce gh's exact output.
func (h *Handler) finish(plan shape.Plan, req *protocol.Request, result *executor.Result, cached bool) *protocol.Response {
	if plan.JQ != "" {
		var out []byte
		var err error = errNonZeroExit
		if result.ExitCode == 0 {
			out, err = shape.ApplyJQ(result.Stdout, plan.JQ)
		}
		if err != nil {
			result = h.execGH(req.Args, req.WorkDir, req.AuthEnv)
			return &protocol.Response{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
		}
		result = &executor.Result{Stdout: out, Stderr: result.Stderr, ExitCode: 0}
	}
	return &protocol.Response{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Cached: cached}
}

var errNonZeroExit = fmt.Errorf("non-zero exit")

// fieldEntry records a cached --json response and the fields it selected.
type fieldEntry struct {
	key    string
	fields []string
}

const maxFieldEntriesPerBase = 8

// indexFields remembers that cacheKey holds output for fields, so requests for
// a subset of those fields with the same base command can reuse it.
func (h *Handler) indexFields(baseKey, cacheKey string, fields []string) {
	if baseKey == "" {
		return
	}
	h.fieldsMu.Lock()
	defer h.fieldsMu.Unlock()
	if len(h.fieldIndex) >= h.cfg.MaxCacheEntries {
		h.pruneFieldIndexLocked()
	}
	entries := h.fieldIndex[baseKey]
	for i, e := range entries {
		if e.key == cacheKey {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	entries = append(entries, fieldEntry{key: cacheKey, fields: fields})
	if len(entries) > maxFieldEntriesPerBase {
		entries = entries[len(entries)-maxFieldEntriesPerBase:]
	}
	h.fieldIndex[baseKey] = entries
}

func (h *Handler) pruneFieldIndexLocked() {
	for base, entries := range h.fieldIndex {
		live := entries[:0]
		for _, e := range entries {
			if h.cache.Contains(e.key) {
				live = append(live, e)
			}
		}
		if len(live) == 0 {
			delete(h.fieldIndex, base)
		} else {
			h.fieldIndex[base] = live
		}
	}
}

// lookupSuperset finds a successful cached response for a superset of fields
// and projects it down to the requested fields.
func (h *Handler) lookupSuperset(baseKey string, fields []string) *executor.Result {
	if baseKey == "" {
		return nil
	}
	h.fieldsMu.Lock()
	candidates := append([]fieldEntry(nil), h.fieldIndex[baseKey]...)
	h.fieldsMu.Unlock()

	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if !shape.Covers(c.fields, fields) {
			continue
		}
		entry := h.cache.Get(c.key)
		if entry == nil || entry.ExitCode != 0 {
			continue
		}
		if out, ok := shape.Project(entry.Stdout, fields); ok {
			return &executor.Result{Stdout: out, Stderr: entry.Stderr}
		}
	}
	return nil
}

func (h *Handler) store(cacheKey, cmdKey string, classification allowlist.Classification, req *protocol.Request, result *executor.Result, version uint64) {
	// Retain successful results and the gh pr checks pending status only.
	if result.ExitCode != 0 && !(cmdKey == "pr_checks" && result.ExitCode == 8) {
		return
	}
	ttl := h.cfg.CommandTTL(cmdKey)
	if req.TTLOverride > 0 {
		ttl = time.Duration(req.TTLOverride) * time.Second
	} else if _, overridden := h.cfg.TTLOverrides[cmdKey]; !overridden && result.ExitCode == 0 {
		if classification.Immutable && h.cfg.UpImmutableTTL > ttl {
			ttl = h.cfg.UpImmutableTTL
		} else if classification.Settles && h.cfg.SettledTTL > ttl && reportsCompleted(result.Stdout) {
			ttl = h.cfg.SettledTTL
		}
	}

	d := resource.Identify(req.Context, req.Args)
	h.cache.SetVersion(&cache.Entry{
		Key:        cacheKey,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		ExitCode:   result.ExitCode,
		CachedAt:   time.Now(),
		TTL:        ttl,
		Resource:   allowlist.ResourceType(d.Kind),
		ResourceID: d.ID,
		Host:       d.Host,
		Repo:       d.Repo,
	}, version)
}

// reportsCompleted reports whether stdout is a JSON object whose top-level
// "status" is "completed" (the terminal state for Actions runs and jobs).
func reportsCompleted(stdout []byte) bool {
	var row struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(stdout, &row) == nil && row.Status == "completed"
}

func (h *Handler) doSingleflightExec(key string, execute func() *executor.Result) (*executor.Result, bool) {
	return h.doSingleflightExecWithJoin(key, execute, nil)
}

// The optional join callback lets the concurrency control observe the exact
// handoff before the follower waits; production calls do not install one.
func (h *Handler) doSingleflightExecWithJoin(key string, execute func() *executor.Result, joined func()) (*executor.Result, bool) {
	h.mu.Lock()
	if c, ok := h.inflight[key]; ok {
		h.mu.Unlock()
		if joined != nil {
			joined()
		}
		c.wg.Wait()
		return c.res, true
	}
	c := &call{}
	c.wg.Add(1)
	h.inflight[key] = c
	h.mu.Unlock()
	c.res = execute()
	c.wg.Done()
	h.mu.Lock()
	delete(h.inflight, key)
	h.mu.Unlock()
	return c.res, false
}

func keyContext(ctx execctx.ExecContext, cl allowlist.Classification) execctx.ExecContext {
	if cl.TargetRepo != "" {
		ctx.Repo = cl.TargetRepo
	}
	return ctx
}

func (h *Handler) doSingleflight(key string, args []string, req *protocol.Request) (*executor.Result, uint64, bool) {
	h.mu.Lock()
	version := h.cache.Version()
	flightKey := fmt.Sprintf("normalized/%s/%d", key, version)
	if c, ok := h.inflight[flightKey]; ok {
		h.mu.Unlock()
		c.wg.Wait()
		// Coalesced onto a fetch that started at the recorded generation: the
		// result carries that generation, not the joining call's.
		return c.res, c.version, true
	}

	c := &call{version: version}
	c.wg.Add(1)
	h.inflight[flightKey] = c
	h.mu.Unlock()

	c.res = h.execGH(args, req.WorkDir, req.AuthEnv)
	c.wg.Done()

	h.mu.Lock()
	delete(h.inflight, flightKey)
	h.mu.Unlock()

	return c.res, c.version, false
}

type cacheSnapshot struct {
	metrics.Snapshot
	cache.Usage
}

func (h *Handler) snapshot() cacheSnapshot {
	return cacheSnapshot{h.stats.Snapshot(h.cache.Size(), h.cfg.MaxCacheEntries), h.cache.Usage()}
}

func (h *Handler) handleStats() *protocol.Response {
	snap := h.snapshot()
	data, _ := json.Marshal(snap)
	return &protocol.Response{Stdout: data}
}

func (h *Handler) handleFlush(req *protocol.Request) *protocol.Response {
	var count int
	if req.Dependency != nil || !req.Context.RepoBound {
		count = h.cache.Flush()
	} else if req.Resource == allowlist.ResourceUnknown {
		count = h.cache.Flush()
	} else {
		count = h.invalidate(req.Context.Host, req.Context.Repo, req.Resource)
		h.stats.RecordInvalidation(count)
	}
	msg := []byte(fmt.Sprintf("flushed %d entries\n", count))
	return &protocol.Response{Stdout: msg}
}

// invalidate drops entries affected by a mutation on resource. Raw API reads can
// observe any resource, so they are flushed alongside it; a raw API write can
// change any resource, so it flushes everything cached for the repository.
func (h *Handler) invalidate(host, repo string, resource allowlist.ResourceType) int {
	if resource == allowlist.ResourceAPI {
		return h.cache.InvalidateRepo(host, repo)
	}
	return h.cache.InvalidateNamespace(host, repo, resource) +
		h.cache.InvalidateNamespace(host, repo, allowlist.ResourceAPI)
}

func (h *Handler) handleKeys() *protocol.Response {
	keys := h.cache.Keys()
	data, _ := json.Marshal(keys)
	return &protocol.Response{Stdout: data}
}

// sanitizeCmdKey builds a metrics key from at most the first two args
// (subcommand + action), avoiding exposure of flags or values that could
// contain sensitive data.
func sanitizeCmdKey(args []string) string {
	switch {
	case len(args) >= 2:
		return args[0] + "_" + args[1]
	case len(args) == 1:
		return args[0]
	default:
		return "unknown"
	}
}
