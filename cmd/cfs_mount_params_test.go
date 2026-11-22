package cmd

import (
	"testing"
)

// The options of `cfs mount` must reach rclone where mount/mount reads them:
// flat at the top level of the parameters (VFS options and mount options alike).
func TestCfsMountParamsCarryTheFlags(t *testing.T) {
	cfsFlags.cacheMode = "full"
	cfsFlags.uid, cfsFlags.gid, cfsFlags.umask = "1000", "1001", "002"
	cfsMountFlags.cacheMaxSize, cfsMountFlags.cacheMaxDirty = "20G", "50G"
	cfsMountFlags.allowOther = true

	p := cfsMountParams("/srv/mnt")
	want := map[string]any{
		"fs": cfsFsName, "mountPoint": "/srv/mnt",
		"vfs_cache_mode":      "full",
		"vfs_cache_max_size":  "20G",
		"vfs_cache_max_dirty": "50G",
		"uid":                 "1000", "gid": "1001", "umask": "002",
		"allow_other": true,
	}
	for k, v := range want {
		if got, ok := p[k]; !ok || got != v {
			t.Errorf("param %s = %v (present %v), want %v", k, got, ok, v)
		}
	}
}
