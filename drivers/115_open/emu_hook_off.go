//go:build !emu115

package _115_open

import sdk "github.com/OpenListTeam/115-sdk-go"

// EmuHook is false in every build made without -tags emu115: the driver always
// talks to the real 115 endpoints.
const EmuHook = false

func testBaseURLOptions() []sdk.Option { return nil }
