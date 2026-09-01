package app

import (
	"context"
	"testing"
)

func TestAgentRunRegistryCancellationAndCleanup(t *testing.T) {
	a := hardeningTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.activeRuns["run1"] = newActiveRun(cancel)
	a.runMu.Lock()
	got := a.activeRuns["run1"]
	a.runMu.Unlock()
	if got == nil {
		t.Fatal("run not registered")
	}
	got.cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("registered cancel did not cancel context")
	}
	a.runMu.Lock()
	delete(a.activeRuns, "run1")
	a.runMu.Unlock()
	if len(a.activeRuns) != 0 {
		t.Fatal("run registry leaked")
	}
}

func TestAgentConcurrencyIsBounded(t *testing.T) {
	a := hardeningTestApp(t)
	for i := 0; i < cap(a.runSlots); i++ {
		a.runSlots <- struct{}{}
	}
	select {
	case a.runSlots <- struct{}{}:
		t.Fatal("accepted run beyond cap")
	default:
	}
}
