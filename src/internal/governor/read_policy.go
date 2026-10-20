package governor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const readTTLSeconds int64 = 300

// statusRead names the reads the fleet polls to watch CI: run lists/views,
// check runs and suites, commit statuses, and GraphQL queries (gh's status
// commands). They get the shared StatusTTL window.
func statusRead(r *http.Request) bool {
	path := r.URL.Path
	return path == "/graphql" ||
		strings.HasSuffix(path, "/check-runs") ||
		strings.HasSuffix(path, "/check-suites") ||
		strings.HasSuffix(path, "/statuses") ||
		strings.HasSuffix(path, "/status") ||
		strings.Contains(path, "/actions/runs") ||
		strings.Contains(path, "/actions/workflows")
}

// readTTL is how long a stored read stays authoritative without going
// upstream. Status reads keep one shared window for the whole box; ordinary
// reads keep the default and still honour a shorter upstream max-age.
func (g *Gate) readTTL(r *http.Request) int64 {
	if statusRead(r) && g.Policy.StatusTTL > 0 {
		return g.Policy.StatusTTL
	}
	return readTTLSeconds
}

// Cache identity describes the representation and credential, not the lane's
// transport or conditional validator. Authorization remains inside the hash.
func readKey(r *http.Request, body []byte) string {
	h := r.Header.Clone()
	for _, name := range []string{"User-Agent", "If-None-Match", "If-Modified-Since",
		"Cache-Control", "Pragma", "Connection", "Proxy-Connection",
		"Proxy-Authorization", "Content-Length", "X-Request-Id"} {
		h.Del(name)
	}
	if r.Method == "GET" {
		h.Del("Content-Type")
	}
	encoded, _ := json.Marshal(h)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(r.URL.String()+"\x00"+string(encoded)+"\x00"+string(body))))
}

func directive(h http.Header, wanted string) bool {
	for _, item := range strings.Split(strings.ToLower(h.Get("Cache-Control")), ",") {
		if strings.TrimSpace(strings.SplitN(item, "=", 2)[0]) == wanted {
			return true
		}
	}
	return false
}

func freshEntry(r *http.Request, e Entry, now, ttl int64) bool {
	if e.At <= 0 || now < e.At || directive(r.Header, "no-cache") || directive(r.Header, "no-store") ||
		r.Header.Get("Pragma") == "no-cache" || directive(e.Header, "no-cache") || directive(e.Header, "no-store") {
		return false
	}
	if ttl <= 0 {
		ttl = readTTLSeconds
	}
	// Status reads are shared box-wide on their own window: GitHub advertises a
	// 60s max-age on check-runs, but many lanes poll the same repo far faster
	// than that, and one upstream read per window is what removes the load.
	if !statusRead(r) {
		for _, item := range strings.Split(strings.ToLower(e.Header.Get("Cache-Control")), ",") {
			key, value, _ := strings.Cut(strings.TrimSpace(item), "=")
			if key == "max-age" {
				if seconds, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64); err == nil && seconds < ttl {
					ttl = seconds
				}
			}
		}
	}

	return now-e.At < ttl
}

func blockedUntil(h http.Header, now, failures int64) int64 {
	delay := readTTLSeconds
	for i := int64(1); i < failures && delay < 1800; i++ {
		delay *= 2
	}
	if delay > 1800 {
		delay = 1800
	}
	until := now + delay
	if retry, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil {
		if now+retry > until {
			until = now + retry
		}
	} else if date, err := http.ParseTime(h.Get("Retry-After")); err == nil && date.Unix() > until {
		until = date.Unix()
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > until {
			until = reset
		}
	}
	return until
}

// GraphQL may return HTTP 200 together with partial data and errors. Neither is
// a valid reusable read. REST error responses never enter this path.
func cacheableQueryBody(body []byte) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || len(doc["data"]) == 0 || string(doc["data"]) == "null" {
		return false
	}
	var errors []json.RawMessage
	if raw, ok := doc["errors"]; ok && (json.Unmarshal(raw, &errors) != nil || len(errors) > 0) {
		return false
	}
	return true
}

func (g *Gate) storeEntry(path string, entry Entry) error {
	if err := save(path, entry); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
	if err != nil {
		return err
	}
	type stored struct {
		path string
		size int64
		at   int64
	}
	items := []stored{}
	var total int64
	for _, file := range files {
		if info, err := os.Stat(file); err == nil {
			items = append(items, stored{file, info.Size(), info.ModTime().UnixNano()})
			total += info.Size()
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at < items[j].at })
	count := len(items)
	for _, item := range items {
		if count <= 128 && total <= 64*1024*1024 {
			break
		}
		if item.path == path {
			continue
		}
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= item.size
		count--
	}
	return nil
}

func rateLimitedQueryBody(body []byte) bool {
	var doc struct {
		Errors []struct {
			Message string
			Type    string
		}
	}
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	for _, error := range doc.Errors {
		message := strings.ToLower(error.Message)
		if error.Type == "RATE_LIMITED" || strings.Contains(message, "rate limit") || strings.Contains(message, "secondary rate") {
			return true
		}
	}
	return false
}
