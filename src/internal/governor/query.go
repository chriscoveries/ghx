package governor

import (
	"encoding/json"
	"strings"
)

// File/stdin inputs are excluded because their contents aren't in a command key.
func ReadArgs(args []string) bool {
	if len(args) < 2 || args[0] != "api" || args[1] != "graphql" {
		return false
	}
	query := ""
	for i := 2; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-F") || strings.HasPrefix(a, "--field") || strings.HasPrefix(a, "--input") {
			return false
		}
		if a == "-f" || a == "--raw-field" || a == "-X" || a == "--method" {
			if i+1 >= len(args) {
				return false
			}
			i++
			if a == "-X" || a == "--method" {
				a = "-X" + args[i]
			} else {
				a = args[i]
			}
		}
		method := strings.TrimPrefix(strings.TrimPrefix(a, "--method="), "-X")
		if method != a && method != "POST" && method != "GET" {
			return false
		}
		a = strings.TrimPrefix(strings.TrimPrefix(a, "--raw-field="), "-f")
		if strings.HasPrefix(a, "query=") {
			query = strings.TrimPrefix(a, "query=")
		}
	}
	b, _ := json.Marshal(map[string]string{"query": query})
	return ReadQuery(b)
}
