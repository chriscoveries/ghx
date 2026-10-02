package cache

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
)

func TestGetSetBasic(t *testing.T) {
	c := New(10)
	c.Set(&Entry{
		Key:      "k1",
		Stdout:   []byte("hello"),
		ExitCode: 0,
		CachedAt: time.Now(),
		TTL:      10 * time.Second,
	})

	e := c.Get("k1")
	if e == nil {
		t.Fatal("expected entry, got nil")
	}
	if string(e.Stdout) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(e.Stdout))
	}
}

func TestTTLExpiry(t *testing.T) {
	c := New(10)
	c.Set(&Entry{
		Key:      "k1",
		Stdout:   []byte("old"),
		CachedAt: time.Now().Add(-5 * time.Second),
		TTL:      2 * time.Second,
	})

	if e := c.Get("k1"); e != nil {
		t.Fatal("expected nil for expired entry")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(2)
	c.Set(&Entry{Key: "a", CachedAt: time.Now(), TTL: time.Minute})
	c.Set(&Entry{Key: "b", CachedAt: time.Now(), TTL: time.Minute})
	c.Set(&Entry{Key: "c", CachedAt: time.Now(), TTL: time.Minute})

	// "a" should have been evicted (LRU)
	if c.Get("a") != nil {
		t.Fatal("expected 'a' to be evicted")
	}
	if c.Get("b") == nil {
		t.Fatal("expected 'b' to exist")
	}
	if c.Get("c") == nil {
		t.Fatal("expected 'c' to exist")
	}
}

func TestInvalidateNamespace(t *testing.T) {
	c := New(100)
	c.Set(&Entry{Key: "pr1", Host: "github.com", Repo: "o/r", Resource: allowlist.ResourcePR, CachedAt: time.Now(), TTL: time.Minute})
	c.Set(&Entry{Key: "pr2", Host: "github.com", Repo: "o/r", Resource: allowlist.ResourcePR, CachedAt: time.Now(), TTL: time.Minute})
	c.Set(&Entry{Key: "issue1", Host: "github.com", Repo: "o/r", Resource: allowlist.ResourceIssue, CachedAt: time.Now(), TTL: time.Minute})

	n := c.InvalidateNamespace("github.com", "o/r", allowlist.ResourcePR)
	if n != 2 {
		t.Fatalf("expected 2 invalidated, got %d", n)
	}
	if c.Get("pr1") != nil || c.Get("pr2") != nil {
		t.Fatal("PR entries should be gone")
	}
	if c.Get("issue1") == nil {
		t.Fatal("issue entry should still exist")
	}
}

func TestFlushAll(t *testing.T) {
	c := New(100)
	c.Set(&Entry{Key: "a", CachedAt: time.Now(), TTL: time.Minute})
	c.Set(&Entry{Key: "b", CachedAt: time.Now(), TTL: time.Minute})

	n := c.Flush()
	if n != 2 {
		t.Fatalf("expected 2 flushed, got %d", n)
	}
	if c.Size() != 0 {
		t.Fatal("cache should be empty")
	}
}

// The byte-budget tests retain real repository bytes rather than generated output.
func budgetEntry(t *testing.T, key string, n int) *Entry {
	t.Helper()
	data, err := os.ReadFile("../../../LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	return &Entry{Key: key, Stdout: append([]byte(nil), data[:n]...), CachedAt: time.Now(), TTL: time.Minute}
}

func TestByteBudgetLRU(t *testing.T) {
	c := NewWithByteLimit(10, 6)
	c.Set(budgetEntry(t, "a", 3))
	c.Set(budgetEntry(t, "b", 3))
	c.Get("a") // Reading a makes b the eviction candidate.
	c.Set(budgetEntry(t, "c", 3))
	if c.Get("a") == nil || c.Get("b") != nil || c.Get("c") == nil {
		t.Fatal("byte pressure must evict the least recently used entry")
	}
	if got := c.Usage(); got.Bytes != 6 || got.MaxBytes != 6 || got.Rejected != 0 {
		t.Fatalf("usage = %+v", got)
	}
}

func TestByteBudgetReplacement(t *testing.T) {
	c := NewWithByteLimit(10, 6)
	c.Set(budgetEntry(t, "a", 3))
	c.Set(budgetEntry(t, "b", 3))
	c.Set(budgetEntry(t, "a", 5))
	if c.Get("b") != nil || c.Size() != 1 || c.Usage().Bytes != 5 {
		t.Fatal("growing a replacement must evict other entries and release old bytes")
	}
	c.Set(budgetEntry(t, "a", 1))
	if c.Usage().Bytes != 1 {
		t.Fatal("shrinking a replacement must release old bytes")
	}
	c.Set(budgetEntry(t, "b", 3))
	c.Set(budgetEntry(t, "a", 7))
	if c.Get("a") != nil || c.Get("b") == nil || c.Usage().Bytes != 3 || c.Usage().Rejected != 1 {
		t.Fatal("oversized replacement must drop the old value and preserve other entries")
	}
}

func TestByteBudgetCountsStderrAndReleases(t *testing.T) {
	c := NewWithByteLimit(2, 6)
	a := budgetEntry(t, "a", 3)
	a.Stderr = a.Stdout
	c.Set(a)
	if c.Usage().Bytes != 6 {
		t.Fatal("stderr must count toward the response budget")
	}
	c.Set(budgetEntry(t, "b", 1))
	if c.Usage().Bytes != 1 {
		t.Fatal("eviction must release stdout and stderr bytes")
	}
	b := c.Get("b")
	b.CachedAt = time.Now().Add(-2 * time.Minute)
	if c.Get("b") != nil || c.Usage().Bytes != 0 {
		t.Fatal("expiry must release bytes")
	}
	c.Set(a)
	c.InvalidateNamespace("", "", a.Resource)
	if c.Usage().Bytes != 0 {
		t.Fatal("invalidation must release bytes")
	}
	c.Set(a)
	c.Flush()
	if c.Usage().Bytes != 0 {
		t.Fatal("flush must release bytes")
	}
}

func TestNonpositiveCapacityDisablesStorage(t *testing.T) {
	for _, limits := range []struct {
		entries int
		bytes   int64
	}{{0, 6}, {-1, 6}, {1, 0}, {1, -1}} {
		c := NewWithByteLimit(limits.entries, limits.bytes)
		c.Set(budgetEntry(t, "a", 1))
		if c.Get("a") != nil || c.Size() != 0 || c.Usage().Bytes != 0 || c.Usage().Rejected != 1 {
			t.Fatalf("nonpositive capacity %+v must disable storage", limits)
		}
	}
}

func TestByteBudgetConcurrent(t *testing.T) {
	c := NewWithByteLimit(3, 6)
	entry := budgetEntry(t, "same", 2)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				c.Set(entry)
				c.Get(entry.Key)
				c.Usage()
			}
		})
	}
	wg.Wait()
	if c.Usage().Bytes != 2 || c.Size() != 1 {
		t.Fatal("concurrent replacement must preserve accounting")
	}
}
