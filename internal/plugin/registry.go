// Package plugin provides a cross-platform, hot-reloadable plugin system.
//
// The architecture has two halves:
//   - A hook Registry (this file): host code fires named hooks at well-known
//     points; plugins subscribe to the hooks they care about. The set of hook
//     points is host-provided, but which hooks a plugin subscribes to — and the
//     plugin's logic — is fully dynamic and reloadable.
//   - A wazero-based WASM Runtime (wasm.go): runs plugin logic in-process,
//     sandboxed, on every platform, and can be reloaded without restarting.
package plugin

import (
	"sort"
	"sync"
)

// HookContext carries the data for a fired hook. Handlers may read and mutate
// Payload; mutations are visible to later handlers and to the host.
type HookContext struct {
	Hook    string
	Payload map[string]any
}

// Handler reacts to a fired hook. Returning an error is logged but does not stop
// other handlers from running.
type Handler func(ctx *HookContext) error

type subscription struct {
	pluginID string
	hook     string
	order    int
	handler  Handler
}

// Registry is a concurrency-safe hook bus. The zero value is not usable; call
// NewRegistry.
type Registry struct {
	mu   sync.RWMutex
	subs []subscription
}

func NewRegistry() *Registry {
	return &Registry{}
}

// Subscribe registers handler for hook on behalf of pluginID. Lower order runs
// first. A plugin may subscribe to the same hook multiple times.
func (r *Registry) Subscribe(pluginID, hook string, order int, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, subscription{pluginID, hook, order, handler})
}

// UnsubscribeAll removes every subscription owned by pluginID. This is what makes
// hot-reload safe: drop a plugin's hooks before loading its new version.
func (r *Registry) UnsubscribeAll(pluginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.subs[:0]
	for _, s := range r.subs {
		if s.pluginID != pluginID {
			kept = append(kept, s)
		}
	}
	// Avoid retaining handlers of removed plugins in the backing array.
	for i := len(kept); i < len(r.subs); i++ {
		r.subs[i] = subscription{}
	}
	r.subs = kept
}

// HasHandlers reports whether at least one handler is subscribed to a hook. It
// is cheap and used on hot paths to avoid building a payload when nothing listens.
func (r *Registry) HasHandlers(hook string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.subs {
		if s.hook == hook {
			return true
		}
	}
	return false
}

// Handlers returns the ordered handlers subscribed to a hook (mainly for tests).
func (r *Registry) Handlers(hook string) []Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched []subscription
	for _, s := range r.subs {
		if s.hook == hook {
			matched = append(matched, s)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].order < matched[j].order
	})
	out := make([]Handler, len(matched))
	for i, s := range matched {
		out[i] = s.handler
	}
	return out
}

// Fire invokes every handler subscribed to hook, in order, passing a shared
// context. It returns the (possibly mutated) context and a slice of any errors
// handlers returned. One failing handler never blocks the others.
func (r *Registry) Fire(hook string, payload map[string]any) (*HookContext, []error) {
	if payload == nil {
		payload = map[string]any{}
	}
	ctx := &HookContext{Hook: hook, Payload: payload}
	var errs []error
	for _, h := range r.Handlers(hook) {
		if err := h(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return ctx, errs
}
