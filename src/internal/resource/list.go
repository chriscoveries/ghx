package resource

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	execctx "github.com/brunoborges/ghx/src/internal/context"
)

// List reads are the dominant fleet traffic and every lane asks for them
// differently: one lane wants the newest run's conclusion, the next wants ten
// runs with a different field list, a third pipes a jq filter. Keyed on the
// literal argv each of those is a separate cache entry, so a box watching the
// same repository reports almost no reuse.
//
// A list shape fixes both halves of that. The argv is canonicalized (flags
// sorted, shorthands expanded, the projection flags removed), and a JSON read
// is answered from one wider fetch: the full field policy at the widest page
// size, with each caller's fields and row count projected back out of it. A
// caller that asks for more rows than the superset holds, or for a field the
// command cannot emit, keeps native execution.

// listSpec fixes the grammar of one list command. Anything not named here —
// an unlisted flag, a positional selector, an output form we cannot project —
// makes the shape decline, so native execution keeps authority.
type listSpec struct {
	policy     string
	valueFlags map[string]bool
	shorthands map[string]string
	// filters are carried verbatim into the superset fetch; only their order
	// changes, which is what makes two lanes agree on a key.
	filters []string
}

var listCommands = map[string]listSpec{
	"run list": {
		policy: "run",
		valueFlags: map[string]bool{
			"--branch": true, "--commit": true, "--created": true, "--event": true,
			"--status": true, "--user": true, "--workflow": true, "--repo": true,
			"--limit": true, "--json": true, "--jq": true, "--template": true,
		},
		shorthands: map[string]string{
			"-b": "--branch", "-c": "--commit", "-e": "--event", "-s": "--status",
			"-u": "--user", "-w": "--workflow", "-R": "--repo", "-L": "--limit",
			"-q": "--jq", "-t": "--template",
		},
		filters: []string{"--branch", "--commit", "--created", "--event", "--status", "--user", "--workflow"},
	},
}

// supersetLimit is the page gh fetches for a shared list read. It is also the
// ceiling: a caller asking for more rows than this is served natively rather
// than answered from a body that cannot hold its answer.
const supersetLimit = 100

// PrepareList returns nil whenever the caller's read cannot be served from a
// wider cached body.
func PrepareList(ctx execctx.ExecContext, args []string) *Shape {
	if len(args) < 2 {
		return nil
	}
	spec, ok := listCommands[args[0]+" "+args[1]]
	if !ok {
		return nil
	}
	s := &Shape{Context: ctx, Kind: args[0], List: true, Limit: 30, Filters: map[string]string{}}
	repo := ctx.Repo
	var filters []string
	seen := map[string]bool{}

	for i := 2; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if canonical, ok := spec.shorthands[name]; ok {
			name = canonical
		}
		if !strings.HasPrefix(name, "-") || !spec.valueFlags[name] {
			// A positional selector or an unknown flag changes what the command
			// means; the shape cannot reason about it, so it declines.
			return nil
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		if !hasValue {
			i++
			if i >= len(args) {
				return nil
			}
			value = args[i]
		}
		switch name {
		case "--json":
			for _, f := range strings.Split(value, ",") {
				if f == "" {
					return nil
				}
				s.Fields = append(s.Fields, f)
			}
		case "--jq":
			s.JQ = value
		case "--template":
			// Templates are rendered by gh over the caller's own rows; the
			// projection cannot reproduce them, so keep the native path.
			return nil
		case "--repo":
			repo = value
		case "--limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil
			}
			s.Limit = n
		default:
			filters = append(filters, name+"="+value)
			s.Filters[name] = value
		}
	}
	// A jq filter is rendered over projected JSON, which needs the JSON form.
	if s.JQ != "" && (len(s.Fields) == 0 || runtime.GOOS == "windows") {
		return nil
	}
	host, resolved := Repository(ctx.Host, repo)
	if resolved == "" || host != ctx.Host {
		return nil
	}
	sort.Strings(s.Fields)
	full := policies[spec.policy].ListFields
	if len(s.Fields) > 0 {
		if full == "" {
			return nil
		}
		allowed := "," + full + ","
		for _, f := range s.Fields {
			if !strings.Contains(allowed, ","+f+",") {
				return nil
			}
		}
	}
	// Canonical order: the caller's flag order is not part of the query, so the
	// same read from two lanes lands on one key.
	sort.Strings(filters)
	s.Context.Host, s.Context.Repo, s.Context.Branch = host, resolved, ""
	if len(s.Fields) > 0 {
		if s.Limit > supersetLimit {
			return nil
		}
		s.Args = []string{args[0], args[1], "--repo", host + "/" + resolved, "--limit", strconv.Itoa(supersetLimit), "--json", full}
	} else {
		s.Args = []string{args[0], args[1], "--repo", host + "/" + resolved, "--limit", strconv.Itoa(s.Limit)}
	}
	for _, f := range filters {
		name, value, _ := strings.Cut(f, "=")
		s.Args = append(s.Args, name, value)
	}
	return s
}

func validList(s *Shape, body []byte) bool {
	if len(s.Fields) == 0 {
		return len(body) > 0
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(body, &rows) != nil {
		return false
	}
	for _, row := range rows {
		for _, f := range s.Fields {
			if _, ok := row[f]; !ok {
				return false
			}
		}
	}
	return true
}

func projectList(s *Shape, body []byte) ([]byte, error) {
	// A text read has no projection: the cached body is the answer.
	if len(s.Fields) == 0 {
		if len(body) == 0 {
			return nil, fmt.Errorf("native list output is empty")
		}
		return body, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	if len(rows) > s.Limit {
		rows = rows[:s.Limit]
	}
	out := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		projected := make(map[string]json.RawMessage, len(s.Fields))
		for _, f := range s.Fields {
			v, ok := row[f]
			if !ok {
				return nil, fmt.Errorf("native list output is missing field %s", f)
			}
			projected[f] = v
		}
		out = append(out, projected)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
