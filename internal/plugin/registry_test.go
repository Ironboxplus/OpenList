package plugin

import (
	"errors"
	"testing"
)

func TestRegistrySubscribeAndFire(t *testing.T) {
	r := NewRegistry()
	var calls []string
	r.Subscribe("p1", "fs.list.after", 0, func(ctx *HookContext) error {
		calls = append(calls, "p1")
		return nil
	})
	r.Subscribe("p2", "fs.list.after", 0, func(ctx *HookContext) error {
		calls = append(calls, "p2")
		return nil
	})
	r.Subscribe("p3", "other.hook", 0, func(ctx *HookContext) error {
		calls = append(calls, "p3")
		return nil
	})

	_, errs := r.Fire("fs.list.after", nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 handlers fired, got %v", calls)
	}
}

func TestRegistryOrder(t *testing.T) {
	r := NewRegistry()
	var calls []string
	r.Subscribe("late", "h", 10, func(*HookContext) error {
		calls = append(calls, "late")
		return nil
	})
	r.Subscribe("early", "h", -5, func(*HookContext) error {
		calls = append(calls, "early")
		return nil
	})
	r.Fire("h", nil)
	if len(calls) != 2 || calls[0] != "early" || calls[1] != "late" {
		t.Fatalf("expected [early late], got %v", calls)
	}
}

func TestRegistryPayloadMutationVisibleToLaterHandlers(t *testing.T) {
	r := NewRegistry()
	r.Subscribe("a", "h", 0, func(ctx *HookContext) error {
		ctx.Payload["n"] = 1
		return nil
	})
	r.Subscribe("b", "h", 1, func(ctx *HookContext) error {
		if ctx.Payload["n"] != 1 {
			t.Errorf("expected payload from earlier handler, got %v", ctx.Payload["n"])
		}
		ctx.Payload["n"] = 2
		return nil
	})
	ctx, _ := r.Fire("h", nil)
	if ctx.Payload["n"] != 2 {
		t.Fatalf("expected final payload 2, got %v", ctx.Payload["n"])
	}
}

func TestRegistryErrorsDoNotStopOthers(t *testing.T) {
	r := NewRegistry()
	ran := false
	r.Subscribe("bad", "h", 0, func(*HookContext) error {
		return errors.New("boom")
	})
	r.Subscribe("good", "h", 1, func(*HookContext) error {
		ran = true
		return nil
	})
	_, errs := r.Fire("h", nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if !ran {
		t.Fatal("later handler should still run after an earlier error")
	}
}

func TestRegistryUnsubscribeAll(t *testing.T) {
	r := NewRegistry()
	r.Subscribe("p1", "h", 0, func(*HookContext) error { return nil })
	r.Subscribe("p1", "h2", 0, func(*HookContext) error { return nil })
	r.Subscribe("p2", "h", 0, func(*HookContext) error { return nil })

	r.UnsubscribeAll("p1")
	if got := len(r.Handlers("h")); got != 1 {
		t.Fatalf("expected 1 handler on h after unsubscribe, got %d", got)
	}
	if got := len(r.Handlers("h2")); got != 0 {
		t.Fatalf("expected 0 handlers on h2 after unsubscribe, got %d", got)
	}
}
