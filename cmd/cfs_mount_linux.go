//go:build linux

package cmd

// rclone's Linux mount is pure Go (bazil.org/fuse, no cgo, no libfuse): importing
// it registers the "mount" method that `openlist cfs mount` looks for. Other
// systems have no such mount and need the cmount build.
import _ "github.com/rclone/rclone/cmd/mount"
