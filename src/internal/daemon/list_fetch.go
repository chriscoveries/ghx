package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/cache"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/metrics"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
)

// A run list is the fleet's most repeated read and also the one it repeats
// longest: lanes poll the same repository every minute and most of those reads
// are answered by bytes that have not changed. The CLI does not expose
// validators, so an expired entry used to mean a full refetch. Reading the same
// collection through the REST endpoint the CLI uses gives the daemon an ETag to
// hand back, and a 304 can refresh the cached bytes. When the fleet proxy is
// configured it separately attributes the conditional request; do not infer
// quota or accounting from the response status alone.

// restRunFields maps the JSON names `gh run list --json` accepts onto the
// representation the REST endpoint returns. A field outside this table keeps
// native execution, because it is not derivable from the collection body.
var restRunFields = map[string]string{
	"conclusion":         "conclusion",
	"createdAt":          "created_at",
	"databaseId":         "id",
	"displayTitle":       "display_title",
	"event":              "event",
	"headBranch":         "head_branch",
	"headSha":            "head_sha",
	"name":               "name",
	"number":             "run_number",
	"startedAt":          "run_started_at",
	"status":             "status",
	"updatedAt":          "updated_at",
	"url":                "html_url",
	"workflowDatabaseId": "workflow_id",
	"workflowName":       "name",
}

// runNumericFields are the JSON fields the CLI prints as numbers. An absent
// one cannot be represented, so the read stays native instead of degrading.
var runNumericFields = map[string]bool{"databaseId": true, "number": true, "workflowDatabaseId": true}

// listFilterParams maps the filters gh applies to the run collection onto the
// query parameters of that same collection. Filters with no parameter here are
// served natively.
var listFilterParams = map[string]string{
	"--branch":  "branch",
	"--commit":  "head_sha",
	"--created": "created",
	"--event":   "event",
	"--status":  "status",
}

func conditionalRunsPath(s *resource.Shape) (string, bool) {
	if !s.List || len(s.Fields) == 0 || s.Context.Repo == "" {
		return "", false
	}
	for _, f := range s.Fields {
		if _, ok := restRunFields[f]; !ok {
			return "", false
		}
	}
	path := "/repos/" + s.Context.Repo + "/actions/runs?per_page=" + strconv.Itoa(100)
	names := make([]string, 0, len(s.Filters))
	for name := range s.Filters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		param, ok := listFilterParams[name]
		if !ok {
			return "", false
		}
		path += "&" + param + "=" + url.QueryEscape(s.Filters[name])
	}
	return path, true
}

// translateRuns renders the REST collection in the JSON form the CLI prints, so
// one cached body can answer every caller's projection.
func translateRuns(body []byte, fields []string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc struct {
		WorkflowRuns []map[string]json.RawMessage `json:"workflow_runs"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.WorkflowRuns == nil {
		return nil, fmt.Errorf("run collection has no workflow_runs")
	}
	out := make([]map[string]json.RawMessage, 0, len(doc.WorkflowRuns))
	for _, run := range doc.WorkflowRuns {
		row := make(map[string]json.RawMessage, len(fields))
		for _, f := range fields {
			name, ok := restRunFields[f]
			if !ok {
				return nil, fmt.Errorf("field %s is not in the run collection", f)
			}
			v, ok := run[name]
			if !ok || string(v) == "null" {
				// The CLI prints an empty string for a value the collection
				// does not carry yet (a queued run has no start time). A missing
				// identifier is not a formatting difference; keep it native.
				if runNumericFields[f] {
					return nil, fmt.Errorf("run collection is missing %s", name)
				}
				row[f] = json.RawMessage(`""`)
				continue
			}
			row[f] = v
		}
		out = append(out, row)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// handleConditionalList answers a collection read from a cached body it can
// revalidate. It returns nil whenever the shape is not serviceable this way, so
// the caller keeps the native path.
func (h *Handler) handleConditionalList(req *protocol.Request, s *resource.Shape, cmdKey string) *protocol.Response {
	path, ok := conditionalRunsPath(s)
	if !ok {
		return nil
	}
	start := time.Now()
	key := execctx.CacheKey(s.Context, s.Args)
	if entry := h.cache.Peek(key); entry != nil && !entry.IsExpired() {
		projected, err := s.Project(entry.Stdout)
		if err == nil {
			h.stats.Record(cmdKey, key, metrics.ResultHit, time.Since(start).Seconds()*1000)
			return &protocol.Response{Stdout: projected, Stderr: entry.Stderr, ExitCode: entry.ExitCode, Cached: true}
		}
	}
	version := h.cache.Version()
	old := h.cache.Peek(key)
	args := []string{"api", "--include", path}
	if old != nil && old.ETag != "" {
		args = append(args, "--header", "If-None-Match: "+old.ETag)
	}
	r := h.execGH(args, req.WorkDir, req.AuthEnv)
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(r.Stdout)), nil)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	// gh reports an HTTP 304 as exit 1 even with --include; the retained
	// fleet capture demonstrates that this is a valid revalidation response.
	if r.ExitCode != 0 && !(r.ExitCode == 1 && response.StatusCode == http.StatusNotModified) {
		return nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil
	}
	tag := response.Header.Get("ETag")
	if len(tag) > 1024 {
		tag = ""
	}
	status := metrics.ResultMiss
	if response.StatusCode == http.StatusNotModified {
		if old == nil || old.ETag == "" {
			return nil
		}
		// Refresh only if the generation is still current. The atomic check
		// closes the race between a separate Version check and this insertion.
		refreshed := *old
		refreshed.CachedAt = time.Now()
		if tag == "" {
			refreshed.ETag = old.ETag
		}
		if !h.cache.SetVersion(&refreshed, version) {
			h.stats.Record(cmdKey, key, metrics.ResultMiss, time.Since(start).Seconds()*1000)
			return &protocol.Response{Stderr: []byte("ghx: cached list invalidated during revalidation; retry explicitly\n"), ExitCode: 1}
		}
		status = metrics.ResultRevalidated
		body = old.Stdout
	} else if response.StatusCode == http.StatusOK {
		translated, err := translateRuns(body, strings.Split(resource.ListFields(s.Kind), ","))
		if err != nil {
			return nil
		}
		if _, err := s.Project(translated); err != nil {
			return nil
		}
		body = translated
		h.cache.SetVersion(&cache.Entry{Key: key, Stdout: body, CachedAt: time.Now(), TTL: h.requestTTL(req, cmdKey), ETag: tag, Resource: allowlist.ResourceType(s.Kind), Host: s.Context.Host, Repo: s.Context.Repo}, version)
	} else {
		return nil
	}
	projected, err := s.Project(body)
	if err != nil {
		return nil
	}
	h.stats.Record(cmdKey, key, status, time.Since(start).Seconds()*1000)
	return &protocol.Response{Stdout: projected, Cached: status != metrics.ResultMiss}
}
