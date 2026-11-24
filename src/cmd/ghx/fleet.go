package main

import (
	"crypto/rand"
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
	// CCCC actors do not export CODEX_LANE/DEVMSG_LANE; without this the whole
	// fleet collapses into one hostname caller and per-actor reporting is lost.
	if caller == "" {
		caller = os.Getenv("CCCC_ACTOR_ID")
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
	_, noCache, _ := parseGHXFlags(args)
	os.Setenv("HTTPS_PROXY", governor.ProxyURL(addr, governor.Caller{Lane: caller, Class: "automation", Revalidate: noCache}, dir))
	os.Setenv("HTTP_PROXY", os.Getenv("HTTPS_PROXY"))
	os.Setenv("NO_PROXY", "")
	os.Setenv("SSL_CERT_FILE", filepath.Join(dir, "ca.pem"))
	return nil
}

type callIdentity struct {
	ID      string    `json:"call_id"`
	StartAt time.Time `json:"start_at"`
	Caller  string    `json:"caller"`
	Command string    `json:"command"`
	Key     string    `json:"key"`
}

type callLog struct {
	path    string
	started time.Time
	callIdentity
}

func startCall(args []string) *callLog {
	path := os.Getenv("GHX_CALL_LOG")
	if path == "" {
		return nil
	}
	started := time.Now()
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 && args[0] != "api" {
		cmd += " " + args[1]
	}
	cwd, _ := os.Getwd()
	key := sha256.Sum256([]byte(cwd + "\x00" + strings.Join(args, "\x00")))
	call := &callLog{path: path, started: started, callIdentity: callIdentity{
		ID:      rand.Text(),
		StartAt: started.UTC(),
		Caller:  os.Getenv("GH_CALLER"),
		Command: cmd,
		Key:     fmt.Sprintf("%x", key),
	}}
	// Keep at and exit on the completion row so existing counters see one call.
	call.append(struct {
		callIdentity
		Event string `json:"event"`
	}{call.callIdentity, "start"})
	return call
}

func (call *callLog) finish(cached bool, code int) {
	if call == nil {
		return
	}
	finished := time.Now()
	call.append(struct {
		callIdentity
		Event      string    `json:"event"`
		At         time.Time `json:"at"`
		Cached     bool      `json:"cached"`
		Exit       int       `json:"exit"`
		DurationMS int64     `json:"duration_ms"`
	}{call.callIdentity, "finish", finished.UTC(), cached, code, finished.Sub(call.started).Milliseconds()})
}

func (call *callLog) append(record any) {
	data, err := json.Marshal(record)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghx: ledger unavailable")
		return
	}
	f, err := os.OpenFile(call.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghx: ledger unavailable")
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "ghx: ledger unavailable")
	}
}

func (call *callLog) runDirect(path string, args []string) {
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
	call.finish(false, code)
	os.Exit(code)
}
