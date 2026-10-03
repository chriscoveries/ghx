// Package resource shares JSON view reads without changing native gh formatting.
package resource

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"

	execctx "github.com/brunoborges/ghx/src/internal/context"
)

//go:embed policy.json
var policyJSON []byte

type fieldPolicy struct {
	Fields    string   `json:"fields"`
	Immutable []string `json:"immutable"`
}

var policies = func() map[string]fieldPolicy {
	var p map[string]fieldPolicy
	if err := json.Unmarshal(policyJSON, &p); err != nil {
		panic(err)
	}
	return p
}()

type Shape struct {
	Context                execctx.ExecContext
	Args, Fields           []string
	JQ, Template, Kind, ID string
	Attempt                bool
}

func (s *Shape) Valid(body []byte) bool {
	var data map[string]json.RawMessage
	if json.Unmarshal(body, &data) != nil {
		return false
	}
	field := "number"
	if s.Kind == "run" {
		field = "databaseId"
	}
	return string(data[field]) == s.ID
}

// Prepare returns nil for selectors/flags whose dependencies cannot be resolved.
func Prepare(ctx execctx.ExecContext, args []string) *Shape {
	if len(args) < 2 || args[1] != "view" || policies[args[0]].Fields == "" {
		return nil
	}
	s := &Shape{Context: ctx, Kind: args[0]}
	repo := ctx.Repo
	var extra []string
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		a, value, has := strings.Cut(args[i], "=")
		switch a {
		case "--json", "--jq", "-q", "--template", "-t", "--repo", "-R", "--attempt", "-a":
			canonical := a
			switch a {
			case "-q":
				canonical = "--jq"
			case "-t":
				canonical = "--template"
			case "-R":
				canonical = "--repo"
			case "-a":
				canonical = "--attempt"
			}
			if seen[canonical] {
				return nil
			}
			seen[canonical] = true
			if !has {
				i++
				if i >= len(args) {
					return nil
				}
				value = args[i]
			}
			switch a {
			case "--json":
				s.Fields = strings.Split(value, ",")
			case "--jq", "-q":
				s.JQ = value
			case "--template", "-t":
				s.Template = value
			case "--repo", "-R":
				repo = value
			case "--attempt", "-a":
				if s.Kind != "run" {
					return nil
				}
				n, e := strconv.ParseUint(value, 10, 32)
				if e != nil || n == 0 {
					return nil
				}
				extra = []string{"--attempt", strconv.FormatUint(n, 10)}
				s.Attempt = true
			}
		default:
			if strings.HasPrefix(a, "-") || s.ID != "" {
				return nil
			}
			s.ID = args[i]
		}
	}
	if s.ID == "" || len(s.Fields) == 0 || s.Template != "" || (runtime.GOOS == "windows" && s.JQ != "") {
		return nil
	}
	if u, e := url.Parse(s.ID); e == nil && u.Scheme != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		want := map[string]string{"pr": "pull", "issue": "issues", "run": "actions"}[s.Kind]
		length := 4
		if s.Kind == "run" {
			length = 5
		}
		if u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.RawPath != "" || u.Fragment != "" || len(parts) != length || parts[2] != want || (s.Kind == "run" && parts[3] != "runs") {
			return nil
		}
		s.Context.Host = strings.ToLower(u.Host)
		repo = parts[0] + "/" + parts[1]
		s.ID = parts[len(parts)-1]
	}
	host, repo := Repository(s.Context.Host, repo)
	n, e := strconv.ParseUint(s.ID, 10, 64)
	if e != nil || n == 0 || repo == "" || host != ctx.Host {
		return nil
	}
	s.ID = strconv.FormatUint(n, 10)
	s.Context.Host, s.Context.Repo = host, repo
	s.Context.Branch = ""
	allowed := "," + policies[s.Kind].Fields + ","
	for _, f := range s.Fields {
		if f == "" || !strings.Contains(allowed, ","+f+",") {
			return nil
		}
	}
	sort.Strings(s.Fields)
	s.Args = []string{s.Kind, "view", s.ID, "--repo", s.Context.Host + "/" + s.Context.Repo, "--json", policies[s.Kind].Fields}
	s.Args = append(s.Args, extra...)
	return s
}

func (s *Shape) Project(body []byte) ([]byte, error) {
	var data map[string]json.RawMessage
	if e := json.Unmarshal(body, &data); e != nil {
		return nil, e
	}
	out := map[string]json.RawMessage{}
	for _, f := range s.Fields {
		v, ok := data[f]
		if !ok {
			return nil, fmt.Errorf("native resource output is missing field %s", f)
		}
		out[f] = v
	}
	b, e := json.Marshal(out)
	return append(b, '\n'), e
}

// Only stable subsets get a longer TTL. Closed issues can reopen; default run IDs
// can rerun. Completed attempt-specific snapshots have a separate identity.
func (s *Shape) Immutable(body []byte) bool {
	var data map[string]json.RawMessage
	if json.Unmarshal(body, &data) != nil {
		return false
	}
	if s.Kind == "run" && s.Attempt && string(data["status"]) == `"completed"` {
		return true
	}
	if s.Kind != "pr" || string(data["state"]) != `"MERGED"` {
		return false
	}
	for _, f := range s.Fields {
		found := false
		for _, allowed := range policies[s.Kind].Immutable {
			if allowed == f {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
