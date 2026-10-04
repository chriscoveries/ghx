//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/client"
	"github.com/brunoborges/ghx/src/internal/config"
	"github.com/brunoborges/ghx/src/internal/daemon"
	"github.com/brunoborges/ghx/src/internal/protocol"
)

// Run the real main/execDirect paths in subprocesses, with a fake upstream gh.
func TestStdinProcess(t *testing.T) {
	mode := os.Getenv("GHX_STDIN_TEST_PROCESS")
	if mode == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"ghx"}, os.Args[i+1:]...)
			break
		}
	}
	if mode == "ghx" {
		main()
		os.Exit(0)
	}
	if len(os.Args) >= 3 && os.Args[1] == "auth" && os.Args[2] == "token" {
		os.Exit(0) // Context probes from a regressed daemon execution path.
	}
	var body struct{ Text string }
	if err := json.NewDecoder(os.Stdin).Decode(&body); err != nil {
		fmt.Fprintln(os.Stderr, "HTTP 400: Body should be a JSON object")
		os.Exit(1)
	}
	// Barrier: concurrent calls must each reach the upstream independently.
	if dir := os.Getenv("GHX_STDIN_TEST_BARRIER"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, body.Text), nil, 0600); err != nil {
			os.Exit(1)
		}
		for {
			entries, _ := os.ReadDir(dir)
			if len(entries) == 2 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	fmt.Printf("<p><strong>%s</strong></p>\n", strings.Trim(body.Text, "*"))
	os.Exit(0)
}

func stdinHarness(t *testing.T) func(io.Reader, bool, ...string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "gh")
	// Single-quote the executable path for the fixture shell script.
	script := "#!/bin/sh\nGHX_STDIN_TEST_PROCESS=upstream exec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestStdinProcess$ -- \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("GHX_GATE_ADDR", "")
	t.Setenv("GHX_GH_PATH", fake)
	t.Setenv("GHX_SOCKET", filepath.Join(dir, "ghxd.sock"))
	t.Setenv("GHX_CALL_LOG", "")
	t.Setenv("GHX_STDIN_TEST_PROCESS", "ghx")
	if err := os.Mkdir(filepath.Join(dir, ".ghx"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ghx", "config.yaml"), []byte("auto_start: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.GHPath, cfg.SocketPath, cfg.DashboardPort = fake, os.Getenv("GHX_SOCKET"), 0
	srv := daemon.NewServer(cfg, "test", fake)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	t.Cleanup(func() {
		srv.Shutdown()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	cl := client.New(cfg.SocketPath)
	deadline := time.Now().Add(5 * time.Second)
	for !cl.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("test daemon did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		resp, err := cl.Send(&protocol.Request{Type: protocol.TypeStats})
		if err != nil {
			t.Error(err)
			return
		}
		var stats struct{ Total, Coalesced int }
		if err := json.Unmarshal(resp.Stdout, &stats); err != nil || stats.Total != 0 || stats.Coalesced != 0 {
			t.Errorf("stdin calls reached daemon/cache: %s (%v)", resp.Stdout, err)
		}
	})
	return func(input io.Reader, logged bool, args ...string) *exec.Cmd {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestStdinProcess$", "--"}, args...)...)
		cmd.Stdin = input
		cmd.Env = os.Environ()
		if logged {
			cmd.Env = append(cmd.Env, "GHX_CALL_LOG="+filepath.Join(dir, "calls.jsonl"))
		}
		return cmd
	}
}

func TestStdinMarkdownRepro(t *testing.T) {
	command := stdinHarness(t)
	for _, logged := range []bool{false, true} {
		for _, args := range [][]string{
			{"api", "markdown", "--method", "POST", "--input", "-"},
			{"api", "markdown", "--method=POST", "--input=-"},
			{"api", "graphql", "--input", "-"},
			{"api", "markdown", "-X", "GET", "-F", "text=@-"},
			{"api", "markdown"}, // Implicit stdin reader, no '-' argument.
			{"secret", "set", "TEST_SECRET"},
			{"gist", "create"},
			{"pr", "create", "--body-file=-"},
			{"extension", "stdin-consumer"},
		} {
			out, err := command(strings.NewReader("{\"text\":\"**hi**\"}\n"), logged, args...).CombinedOutput()
			if err != nil || string(out) != "<p><strong>hi</strong></p>\n" {
				t.Fatalf("logged=%v %v: %s (%v)", logged, args, out, err)
			}
		}
	}
	input, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("{\"text\":\"**hi**\"}"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if out, err := command(input, false, "api", "markdown").CombinedOutput(); err != nil || string(out) != "<p><strong>hi</strong></p>\n" {
		t.Fatalf("redirected file: %s (%v)", out, err)
	}
}

func TestStdinCallsNeverCoalesced(t *testing.T) {
	command := stdinHarness(t)
	t.Setenv("GHX_STDIN_TEST_BARRIER", t.TempDir())
	// Explicit GET would normally be cacheable, even with --input. Identical
	// arguments with different bodies must execute twice while both are in flight.
	args := []string{"api", "markdown", "--method", "GET", "--input", "-"}
	type result struct {
		out []byte
		err error
	}
	results := make(chan result, 2)
	for _, text := range []string{"first", "second"} {
		cmd := command(strings.NewReader(fmt.Sprintf("{\"text\":%q}", text)), true, args...)
		go func() {
			out, err := cmd.CombinedOutput()
			results <- result{out, err}
		}()
	}
	seen := make(map[string]bool)
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatalf("stdin call: %s (%v)", r.out, r.err)
		}
		seen[string(r.out)] = true
	}
	for _, text := range []string{"first", "second"} {
		if !seen[fmt.Sprintf("<p><strong>%s</strong></p>\n", text)] {
			t.Fatalf("distinct stdin bodies lost or coalesced: %v", seen)
		}
	}
}
