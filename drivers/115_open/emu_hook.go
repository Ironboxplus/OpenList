//go:build emu115

package _115_open

import (
	"os"
	"strings"

	sdk "github.com/OpenListTeam/115-sdk-go"
)

// EmuHook reports whether this binary can be pointed at a local 115 emulator.
// It is true only in builds made with -tags emu115 (tests and the cluster
// black-box harness). Release builds do not contain the override at all, so an
// environment variable can never redirect real tokens to another host.
const EmuHook = true

const (
	envTestAPIBase      = "OPENLIST_TEST_115_API_BASE"
	envTestPassportBase = "OPENLIST_TEST_115_PASSPORT_BASE"
)

func testBaseURLOptions() []sdk.Option {
	var opts []sdk.Option
	if v := strings.TrimSpace(os.Getenv(envTestAPIBase)); v != "" {
		opts = append(opts, sdk.WithAPIBaseURL(v))
	}
	if v := strings.TrimSpace(os.Getenv(envTestPassportBase)); v != "" {
		opts = append(opts, sdk.WithPassportBaseURL(v))
	}
	return opts
}
