//go:build !emu115

package _115_open

import "testing"

// A release build must ignore the emulator environment variables entirely.
func TestReleaseBuildIgnoresEmulatorEnv(t *testing.T) {
	t.Setenv("OPENLIST_TEST_115_API_BASE", "http://127.0.0.1:1")
	t.Setenv("OPENLIST_TEST_115_PASSPORT_BASE", "http://127.0.0.1:1")
	if EmuHook || len(testBaseURLOptions()) != 0 {
		t.Fatal("the emulator override exists in a build without -tags emu115")
	}
}
