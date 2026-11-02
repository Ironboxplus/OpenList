//go:build (linux || darwin || windows) && (amd64 || arm64)

package drivers

import (
	_ "github.com/OpenListTeam/OpenList/v4/drivers/lark"
)
