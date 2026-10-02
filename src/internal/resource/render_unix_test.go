//go:build !windows

package resource

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/brunoborges/ghx/src/internal/authenv"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
)

func nativeCLI(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("native gh unavailable; renderer integration needs gh")
	}
	return path
}
func TestNativeJQOfflineAndPrivateTransport(t *testing.T) {
	path := nativeCLI(t)
	s := &Shape{Context: execctx.ExecContext{Host: "github.com"}, JQ: `[.number, (.title | length)] | @json`}
	body := fixture(t)
	var dir string
	result, err := RenderFilter(s, body, authenv.Environment{"GH_TOKEN": "must-not-reach-renderer"}, func(args []string, env authenv.Environment) *executor.Result {
		dir = env["GH_CONFIG_DIR"]
		if env["GH_TOKEN"] == "must-not-reach-renderer" || env["HTTPS_PROXY"] != "http://127.0.0.1:0" || env["GH_HOST"] != "ghx-render.invalid" {
			t.Fatal("renderer did not isolate credentials and network")
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("renderer directory is not private")
		}
		return executor.Execute(context.Background(), path, args, "", env)
	})
	if err != nil || result.ExitCode != 0 || !strings.HasPrefix(string(result.Stdout), "[21,") {
		t.Fatalf("native renderer: err=%v result=%+v", err, result)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("renderer directory retained")
	}
}
func TestNativeIncludeOutputCanBeParsedWithoutChangingBody(t *testing.T) {
	path := nativeCLI(t)
	b := fixture(t)
	s := &Shape{Context: execctx.ExecContext{Host: "github.com"}}
	r, err := RenderFilter(s, b, nil, func(_ []string, env authenv.Environment) *executor.Result {
		return executor.Execute(context.Background(), path, []string{"api", "/ghx/cache/render", "--include"}, "", env)
	})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("native include: err=%v result=%+v", err, r)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(r.Stdout)), nil)
	if err != nil {
		t.Fatalf("actual native headers: %v", err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("native include changed body: %v", err)
	}
}
