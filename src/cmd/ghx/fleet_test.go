package main

import (
	"os"
	"testing"
)

// A CCCC actor exports neither CODEX_LANE nor DEVMSG_LANE, and the fleet used
// to fall back to hostname:user, which merged every lane into one caller. The
// caller name is attribution only now, but it still has to name the actor.
func TestPrepareFleetUsesCCCCActorForAttribution(t *testing.T) {
	t.Setenv("GH_CALLER", "")
	t.Setenv("CODEX_LANE", "")
	t.Setenv("DEVMSG_LANE", "")
	t.Setenv("CCCC_ACTOR_ID", "lab-hermes5")
	// No gate address: prepareFleet only names the caller and returns.
	t.Setenv("GHX_GATE_ADDR", "")
	if err := prepareFleet([]string{"pr", "view", "20"}); err != nil {
		t.Fatalf("prepareFleet: %v", err)
	}
	if got := os.Getenv("GH_CALLER"); got != "lab-hermes5" {
		t.Fatalf("GH_CALLER = %q", got)
	}
}

// An explicit lane still wins over the actor id.
func TestPrepareFleetPrefersExplicitLane(t *testing.T) {
	t.Setenv("GH_CALLER", "pr-watch")
	t.Setenv("CCCC_ACTOR_ID", "lab-hermes5")
	t.Setenv("GHX_GATE_ADDR", "")
	if err := prepareFleet(nil); err != nil {
		t.Fatalf("prepareFleet: %v", err)
	}
	if got := os.Getenv("GH_CALLER"); got != "pr-watch" {
		t.Fatalf("GH_CALLER = %q", got)
	}
}
