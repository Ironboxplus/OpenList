package plugin

// Well-known hook points the host fires. Plugins Subscribe to these by name.
// Handlers may read and mutate ctx.Payload; mutations are visible to later
// handlers and to the host.
const (
	// Storage lifecycle. Payload: {"mount_path": string, "driver": string}.
	HookStorageCreated = "storage.created"
	HookStorageUpdated = "storage.updated"
	HookStorageDeleted = "storage.deleted"

	// HookFsObjsUpdated fires after a directory's contents change (upload, move,
	// rename, copy, delete, mkdir). Payload: {"path": string, "count": int}.
	HookFsObjsUpdated = "fs.objs.updated"

	// HookFsListAfter fires after a directory is listed.
	// Payload: {"path": string, "count": int, "provider": string}.
	HookFsListAfter = "fs.list.after"

	// HookFsLinkAfter fires after a download link is generated.
	// Payload: {"path": string}.
	HookFsLinkAfter = "fs.link.after"

	// HookUserLoginAfter fires after a successful login.
	// Payload: {"username": string}.
	HookUserLoginAfter = "user.login.after"
)

// Capability functions injected by bootstrap. They live here (rather than the
// plugin package importing internal/op directly) to avoid an import cycle: op
// fires plugin hooks, so plugin must not import op. Nil-safe — an unwired
// capability is simply unavailable to plugins.
var (
	SettingGetter func(key string) string
	SettingSetter func(key, value string) error
)

// FireHook fires a hook on the process-wide plugin registry, if one exists and
// actually has subscribers. It is a cheap no-op otherwise, so callers on hot
// paths (list, link) can invoke it unconditionally without building a payload
// when nothing is listening — guard the payload construction with HasSubscribers.
func FireHook(hook string, payload map[string]any) {
	if Default == nil {
		return
	}
	reg := Default.Registry()
	if !reg.HasHandlers(hook) {
		return
	}
	_, _ = reg.Fire(hook, payload)
}

// HasSubscribers reports whether any plugin is listening on a hook. Callers on
// hot paths use it to skip building a payload when there are no subscribers.
func HasSubscribers(hook string) bool {
	if Default == nil {
		return false
	}
	return Default.Registry().HasHandlers(hook)
}
