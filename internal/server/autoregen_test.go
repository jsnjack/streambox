package server

import (
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer builds a Server suitable for unit testing. We use the
// production constructor, which spawns the regenCheckLoop goroutine — that
// goroutine has no effect inside a test that finishes in milliseconds
// (it ticks every regenCheckInterval = 5s).
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.UUID == "" {
		cfg.UUID = "initial-uuid"
	}
	return New(cfg)
}

func TestMarkRegenPending_IsIdempotent(t *testing.T) {
	s := newTestServer(t, Config{})
	if s.regenPending {
		t.Fatal("regenPending must start false")
	}
	s.markRegenPending()
	if !s.regenPending {
		t.Fatal("regenPending should be true after first call")
	}
	s.markRegenPending()
	if !s.regenPending {
		t.Fatal("regenPending must stay true on repeat calls")
	}
}

func TestTryFireRegen_FiresWhenPending(t *testing.T) {
	var fired atomic.Int32
	s := newTestServer(t, Config{
		OnAutoRegen: func() { fired.Add(1) },
	})
	s.markRegenPending()
	s.tryFireRegen()
	if fired.Load() != 1 {
		t.Fatalf("expected 1 regen, got %d", fired.Load())
	}
	if s.regenPending {
		t.Fatal("regenPending must clear after a successful fire")
	}
}

func TestTryFireRegen_NoOpWhenNotPending(t *testing.T) {
	var fired atomic.Int32
	s := newTestServer(t, Config{
		OnAutoRegen: func() { fired.Add(1) },
	})
	s.tryFireRegen()
	if fired.Load() != 0 {
		t.Fatal("regen must not fire when nothing is pending")
	}
}

func TestTryFireRegen_RespectsCooldown(t *testing.T) {
	var fired atomic.Int32
	s := newTestServer(t, Config{
		OnAutoRegen: func() { fired.Add(1) },
	})
	s.markRegenPending()
	s.tryFireRegen()
	if fired.Load() != 1 {
		t.Fatalf("first regen should fire, got %d", fired.Load())
	}
	// Mark again immediately. Cooldown should block.
	s.markRegenPending()
	s.tryFireRegen()
	if fired.Load() != 1 {
		t.Fatalf("second regen must be blocked by cooldown, got %d", fired.Load())
	}
	if !s.regenPending {
		t.Fatal("regenPending must remain true while cooldown blocks")
	}
}

func TestTryFireRegen_FiresAfterCooldownElapses(t *testing.T) {
	var fired atomic.Int32
	s := newTestServer(t, Config{
		OnAutoRegen: func() { fired.Add(1) },
	})
	s.markRegenPending()
	s.tryFireRegen()
	if fired.Load() != 1 {
		t.Fatalf("first regen should fire, got %d", fired.Load())
	}
	// Backdate lastRegen so cooldown is considered elapsed.
	s.regenMu.Lock()
	s.lastRegen = time.Now().Add(-(regenCooldown + time.Second))
	s.regenMu.Unlock()
	s.markRegenPending()
	s.tryFireRegen()
	if fired.Load() != 2 {
		t.Fatalf("post-cooldown regen must fire, got %d", fired.Load())
	}
}

func TestBumpUpdateID_MarksPendingForLaterFire(t *testing.T) {
	s := newTestServer(t, Config{
		OnAutoRegen: func() {},
	})
	s.BumpUpdateID()
	// tryFireRegen runs from the background ticker, so we don't see an
	// immediate fire — but the flag must be set so the next tick handles it.
	if !s.regenPending {
		t.Fatal("BumpUpdateID must set regenPending")
	}
}
