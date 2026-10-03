//go:build !windows

package resource

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brunoborges/ghx/src/internal/authenv"
	"github.com/brunoborges/ghx/src/internal/executor"
	"gopkg.in/yaml.v3"
)

// RenderFilter retains native gh's jq semantics over cached, real JSON. The
// private transport never reads credential files or sends a request upstream.
func RenderFilter(s *Shape, body []byte, _ authenv.Environment, execute func([]string, authenv.Environment) *executor.Result) (*executor.Result, error) {
	dir, err := os.MkdirTemp("", "ghx-render-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "render.sock")
	config, err := yaml.Marshal(map[string]string{"http_unix_socket": socket})
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "config.yml"), config, 0600); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	if err = os.Chmod(socket, 0600); err != nil {
		return nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || (r.URL.Path != "/ghx/cache/render" && r.URL.Path != "/api/v3/ghx/cache/render") {
			http.Error(w, "local rendering only", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})}
	go server.Serve(ln)
	defer server.Close()
	// Local credentials are placeholders for CLI startup only. No user credential
	// is copied into the temporary directory or presented to this transport.
	localEnv := authenv.Environment{"GH_CONFIG_DIR": dir, "GH_HOST": "ghx-render.invalid", "GH_TOKEN": "ghx-local-render", "GH_ENTERPRISE_TOKEN": "ghx-local-render"}
	// Unsupported socket configuration fails locally instead of reaching GitHub.
	localEnv["HTTPS_PROXY"] = "http://127.0.0.1:0"
	localEnv["HTTP_PROXY"] = "http://127.0.0.1:0"
	localEnv["NO_PROXY"] = "ghx-render-no-bypass.invalid"
	return execute([]string{"api", "/ghx/cache/render", "--jq", s.JQ}, localEnv), nil
}
