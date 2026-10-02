package replay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReplayRespectsHashesTTLMutationsAndUnknownAPI(t *testing.T) {
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	rows := []struct {
		sec      int
		cmd, key string
		exit     int
	}{
		{0, "pr view", "one", 0}, {1, "pr view", "two", 0}, {2, "pr view", "one", 0},
		{33, "pr view", "one", 0}, {34, "pr edit", "write", 0}, {35, "pr view", "one", 0},
		{36, "api", "unknown-method", 0}, {37, "pr view", "one", 0},
		{38, "run view", "failed", 1}, {39, "run view", "failed", 1},
	}
	var log bytes.Buffer
	for _, r := range rows {
		json.NewEncoder(&log).Encode(Call{At: start.Add(time.Duration(r.sec) * time.Second), Command: r.cmd, Key: r.key, Exit: r.exit})
	}
	report, err := Analyze(&log)
	if err != nil {
		t.Fatal(err)
	}
	if report.Calls != 10 || report.Before.Reads != 8 || report.Before.Hits != 2 || report.After.Hits != 1 || report.PotentialWriteFlushes != 2 || report.AmbiguousAPI != 1 {
		t.Fatalf("report=%+v", report)
	}
	if report.ResourceHitRate != nil {
		t.Fatal("invented a resource hit rate from opaque hashes")
	}
}
func TestReplayRejectsCorruptInputWithoutEchoingContent(t *testing.T) {
	_, err := Analyze(strings.NewReader("private-secret-not-json"))
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("corrupt line accepted or echoed")
	}
}
func TestReplayLRUBoundsAndExpiry(t *testing.T) {
	now := time.Now()
	s := newStore(2)
	s.set("a", now.Add(time.Second))
	s.set("b", now.Add(time.Second))
	s.get("a", now)
	s.set("c", now.Add(time.Second))
	if s.get("b", now) || !s.get("a", now) || s.get("a", now.Add(2*time.Second)) {
		t.Fatal("LRU or expiry incorrect")
	}
}
