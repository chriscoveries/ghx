package daemon

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/cache"
	"github.com/brunoborges/ghx/src/internal/config"
)

func TestServerByteBudgetAndDiagnostics(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.MaxCacheBytes != cache.DefaultMaxBytes {
		t.Fatalf("default byte budget = %d, want %d", cfg.MaxCacheBytes, cache.DefaultMaxBytes)
	}
	cfg.MaxCacheBytes = 1
	s := NewServer(cfg, "test", "gh")
	data, err := os.ReadFile("../../../LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	s.cache.Set(&cache.Entry{Key: "license", Stdout: data, CachedAt: time.Now(), TTL: time.Minute})
	var stats struct {
		CacheSize int   `json:"cache_size"`
		Bytes     int64 `json:"cache_bytes"`
		MaxBytes  int64 `json:"max_cache_bytes"`
		Rejected  int64 `json:"cache_rejected"`
	}
	if err := json.Unmarshal(s.handler.handleStats().Stdout, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.CacheSize != 0 || stats.Bytes != 0 || stats.MaxBytes != 1 || stats.Rejected != 1 {
		t.Fatalf("stats = %+v; configured budget and rejection must be visible", stats)
	}
}
