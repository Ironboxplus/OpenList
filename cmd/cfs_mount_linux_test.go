//go:build linux

package cmd

import (
	"testing"

	"github.com/rclone/rclone/cmd/mountlib"
)

// On Linux the ordinary build (no cgo, no cmount tag) must have a mount method,
// or `openlist cfs mount` refuses to start.
func TestLinuxBuildHasMountMethod(t *testing.T) {
	name, fn := mountlib.ResolveMountMethod("")
	if fn == nil {
		t.Fatal("no mount method is registered in the Linux build: `openlist cfs mount` would say it was built without mount support")
	}
	t.Logf("mount method: %q", name)
}
