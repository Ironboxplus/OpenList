//go:build windows && cmount

package cmd

import (
	"testing"

	"github.com/rclone/rclone/cmd/mountlib"
)

// On Windows the build with the cmount tag (no cgo needed, WinFsp is loaded when
// the mount starts) must have a mount method, or `openlist cfs mount` refuses to
// start.
func TestWindowsCmountBuildHasMountMethod(t *testing.T) {
	name, fn := mountlib.ResolveMountMethod("")
	if fn == nil {
		t.Fatal("no mount method is registered in the Windows cmount build: `openlist cfs mount` would say it was built without mount support")
	}
	t.Logf("mount method: %q", name)
}
