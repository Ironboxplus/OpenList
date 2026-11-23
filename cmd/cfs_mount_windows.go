//go:build windows && cmount

package cmd

// rclone's Windows mount goes through cgofuse, which on Windows loads WinFsp's
// DLL when the mount starts and needs no cgo to build: importing it registers
// the "cmount" method that `openlist cfs mount` looks for. Without the cmount
// tag there is no such method.
import _ "github.com/rclone/rclone/cmd/cmount"
