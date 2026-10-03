package resource

import (
	"net/url"
	"strconv"
	"strings"

	execctx "github.com/brunoborges/ghx/src/internal/context"
)

// Identity is a dependency, not a credential or command cache key. Empty ID
// denotes a collection or an unresolved selector and requires broader eviction.
type Identity struct{ Host, Repo, Kind, ID string }

func Repository(host, repo string) (string, string) {
	if host == "" {
		host = "github.com"
	}
	if strings.HasPrefix(repo, "https://") {
		u, err := url.Parse(repo)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return host, ""
		}
		host, repo = u.Host, strings.Trim(u.Path, "/")
	}
	parts := strings.Split(repo, "/")
	if len(parts) == 3 {
		host, parts = parts[0], parts[1:]
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(strings.Join(parts, ""), "{}?# ") {
		return strings.ToLower(host), ""
	}
	return strings.ToLower(host), strings.ToLower(strings.Join(parts, "/"))
}

// Identify resolves explicit repositories and numeric selectors. Ambiguous branch
// selectors remain broad; writes to them must not leave a numeric snapshot alive.
func Identify(ctx execctx.ExecContext, args []string) Identity {
	host, repo := Repository(ctx.Host, ctx.Repo)
	d := Identity{Host: host, Repo: repo}
	if len(args) == 0 {
		return d
	}
	d.Kind = args[0]
	if d.Kind == "repo" && len(args) > 1 {
		if args[1] == "create" || args[1] == "fork" || args[1] == "list" {
			d.Repo = ""
		}
		if len(args) > 2 && !strings.HasPrefix(args[2], "-") && args[1] != "list" {
			d.Host, d.Repo = Repository(d.Host, args[2])
		}
	}
	if len(args) > 1 && args[1] == "transfer" {
		d.Repo = ""
		return d
	}
	if args[0] == "api" {
		a := ParseAPI(args)
		if a.Host != "" {
			d.Host = strings.ToLower(a.Host)
		}
		d.Repo = "" // endpoints outside repos may affect any repository on this host
		path := strings.Split(strings.Trim(strings.SplitN(a.Endpoint, "?", 2)[0], "/"), "/")
		if len(path) >= 3 && path[0] == "repos" {
			_, d.Repo = Repository(d.Host, strings.Join(path[1:3], "/"))
			if len(path) >= 4 {
				switch path[3] {
				case "pulls":
					d.Kind = "pr"
				case "issues":
					d.Kind = "issue"
				case "actions":
					d.Kind = "run"
				}
				n := 4
				if d.Kind == "run" {
					n = 5
					if len(path) <= 4 || path[4] != "runs" {
						d.Kind = "api"
					}
				}
				if len(path) > n {
					d.ID = numeric(path[n])
				}
			}
		}
		return d
	}
	for i := 2; i < len(args); i++ {
		flag, value, equal := strings.Cut(args[i], "=")
		if flag == "--repo" || flag == "-R" {
			if !equal && i+1 < len(args) {
				i++
				value = args[i]
			}
			d.Host, d.Repo = Repository(d.Host, value)
		} else if strings.HasPrefix(flag, "-R") && len(flag) > 2 {
			d.Host, d.Repo = Repository(d.Host, flag[2:])
		}
	}
	if len(args) > 2 && !strings.HasPrefix(args[2], "-") {
		selector := args[2]
		if u, err := url.Parse(selector); err == nil && u.Scheme == "https" {
			p := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(p) >= 4 {
				d.Host, d.Repo = Repository(u.Host, strings.Join(p[:2], "/"))
				selector = p[len(p)-1]
			}
		}
		d.ID = numeric(selector)
	}
	return d
}

func numeric(s string) string {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

// APIShape parses the method independently of flag order. Conditional requests
// are limited to an unformatted, single-page GET, so native output stays exact.
type APIShape struct {
	Method, Endpoint, Host string
	Valid, Conditional     bool
}

func ParseAPI(args []string) APIShape {
	a := APIShape{Method: "GET", Conditional: true, Valid: true}
	explicit, body := false, false
	for i := 1; i < len(args); i++ {
		flag, value, equal := strings.Cut(args[i], "=")
		if !strings.HasPrefix(flag, "-") {
			if a.Endpoint != "" {
				a.Valid = false
			}
			a.Endpoint = args[i]
			continue
		}
		switch {
		case flag == "--method" || flag == "-X" || strings.HasPrefix(flag, "-X"):
			if strings.HasPrefix(flag, "-X") && len(flag) > 2 {
				value = flag[2:]
				equal = true
			}
			if !equal {
				i++
				if i >= len(args) {
					a.Valid = false
					break
				}
				value = args[i]
			}
			a.Method = strings.ToUpper(value)
			explicit = true
		case flag == "--hostname" || flag == "--header" || flag == "-H" || flag == "--field" || flag == "-F" || flag == "--raw-field" || flag == "-f" || flag == "--input" || flag == "--jq" || flag == "-q" || flag == "--template" || flag == "-t" || flag == "--cache" || flag == "--preview" || flag == "-p":
			if !equal {
				i++
				if i >= len(args) {
					a.Valid = false
					break
				}
				value = args[i]
			}
			switch flag {
			case "--hostname":
				a.Host = value
			case "--header", "-H":
				name, _, _ := strings.Cut(value, ":")
				if strings.EqualFold(strings.TrimSpace(name), "If-None-Match") || strings.EqualFold(strings.TrimSpace(name), "If-Modified-Since") {
					a.Conditional = false
				}
			default:
				a.Conditional = false
				if flag == "--field" || flag == "-F" || flag == "--raw-field" || flag == "-f" || flag == "--input" {
					body = true
				}
			}
		case strings.HasPrefix(flag, "-f") || strings.HasPrefix(flag, "-F"):
			body = true
			a.Conditional = false
		case flag == "--include" || flag == "-i" || flag == "--silent" || flag == "--paginate" || flag == "--slurp" || flag == "--verbose":
			a.Conditional = false
		default:
			a.Valid = false
			a.Conditional = false
		}
	}
	if body && !explicit {
		a.Method = "POST"
	}
	if a.Endpoint == "" {
		a.Valid = false
	}
	if a.Endpoint == "graphql" || a.Endpoint == "/graphql" {
		a.Conditional = false
		a.Method = "GRAPHQL"
	}
	a.Conditional = a.Conditional && a.Valid && a.Method == "GET"
	return a
}
