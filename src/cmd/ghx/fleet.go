package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brunoborges/ghx/src/internal/governor"
)

func prepareFleet(args []string) error {
	caller := os.Getenv("GH_CALLER")
	if caller == "" {
		caller = os.Getenv("CODEX_LANE")
	}
	if caller == "" {
		caller = os.Getenv("DEVMSG_LANE")
	}
	if caller == "" {
		host, _ := os.Hostname()
		caller = host + ":" + os.Getenv("USER")
	}
	os.Setenv("GH_CALLER", caller)
	addr := os.Getenv("GHX_GATE_ADDR")
	if addr == "" {
		return nil
	}
	for _, a := range args {
		if a == "--watch" || strings.HasPrefix(a, "--watch=") {
			return fmt.Errorf("watch refused; run gh pr checks once")
		}
	}
	if len(args) > 1 && args[0] == "run" && args[1] == "watch" {
		return fmt.Errorf("watch refused; run gh run view once")
	}
	dir := os.Getenv("GHX_GATE_DIR")
	if dir == "" {
		return fmt.Errorf("GHX_GATE_DIR missing; run devmsg box install-ghx")
	}
	if _, err := os.ReadFile(filepath.Join(dir, "proxy-proof")); err != nil {
		return fmt.Errorf("box gate unavailable; run devmsg box install-ghx")
	}
	os.Setenv("HTTPS_PROXY", governor.ProxyURL(addr, governor.Caller{Lane: caller, Class: "automation"}, dir))
	os.Setenv("HTTP_PROXY", os.Getenv("HTTPS_PROXY"))
	os.Setenv("NO_PROXY", "")
	os.Setenv("SSL_CERT_FILE", filepath.Join(dir, "ca.pem"))
	return nil
}
func logCall(args []string, cached bool, code int) {
	path := os.Getenv("GHX_CALL_LOG")
	if path == "" {
		return
	}
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 && args[0] != "api" {
		cmd += " " + args[1]
	}
	cwd, _ := os.Getwd()
	key := sha256.Sum256([]byte(cwd + "\x00" + strings.Join(args, "\x00")))
	data, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "caller": os.Getenv("GH_CALLER"), "command": cmd, "key": fmt.Sprintf("%x", key), "cached": cached, "exit": code})
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghx: ledger unavailable")
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}
func runLoggedDirect(path string, args []string) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	logCall(args, false, code)
	os.Exit(code)
}
