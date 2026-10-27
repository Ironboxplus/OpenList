package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
	cfsdrv "github.com/OpenListTeam/OpenList/v4/drivers/cfs"
	"github.com/OpenListTeam/OpenList/v4/internal/bootstrap"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/cmd/mountlib"
	_ "github.com/rclone/rclone/cmd/serve/webdav"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/rc"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// A cfs volume has exactly one writer, so the mount and the WebDAV server
// below run inside the one process that also serves the web UI and the
// storage: they take the volume of the cfs storage the server loaded (the same
// instance, not a second one) and put rclone's VFS in front of it.

var cfsFlags struct {
	cacheMode string
	addr      string
	user      string
	pass      string
	readOnly  bool
}

// cfsFsName is the name the shared volume goes by in rclone's fs cache.
const cfsFsName = "openlist-cfs"

var cfsCmd = &cobra.Command{
	Use:   "cfs",
	Short: "Mount or serve a cfs storage",
	Long: `Mount or serve a cfs storage (small files packed on another storage).

The server itself is started too: the volume has a single writer, so the web
UI, WebDAV of OpenList and the mount all work on the one instance in this
process.`,
}

var cfsMountCmd = &cobra.Command{
	Use:   "mount <storage-mount-path> <mount-point>",
	Short: "Mount a cfs storage as a drive or directory",
	Long: `Mount a cfs storage with rclone's VFS (WinFsp on Windows, FUSE elsewhere).

Needs a build with the cmount tag ("go build -tags cmount"), and WinFsp
(https://winfsp.dev) installed on Windows. On Windows the mount point is a
drive letter (X:) or a not yet existing directory.`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		if _, fn := mountlib.ResolveMountMethod(""); fn == nil {
			fmt.Fprintln(os.Stderr, "this openlist binary was built without mount support; rebuild with -tags cmount (Windows also needs WinFsp)")
			os.Exit(2)
		}
		runCfs(args[0], func(ctx context.Context) (func(), error) {
			out, err := rc.Calls.Get("mount/mount").Fn(ctx, rc.Params{
				"fs": cfsFsName, "mountPoint": args[1],
				"vfs_cache_mode": cfsFlags.cacheMode,
				"mountOpt":       rc.Params{"VolumeName": "cfs"},
			})
			if err != nil {
				return nil, fmt.Errorf("mount %s: %w", args[1], err)
			}
			_ = out
			fmt.Printf("mounted %s at %s\n", args[0], args[1])
			return func() {
				_, err := rc.Calls.Get("mount/unmount").Fn(context.Background(), rc.Params{"mountPoint": args[1]})
				if err != nil {
					log.Warnf("unmount %s: %v", args[1], err)
				}
			}, nil
		})
	},
}

var cfsWebdavCmd = &cobra.Command{
	Use:   "webdav <storage-mount-path>",
	Short: "Serve a cfs storage over WebDAV with rclone's VFS (the same layer a mount uses)",
	Long: `Serve a cfs storage over WebDAV through rclone's VFS. It is the same VFS
layer a mount puts in front of the volume, without needing WinFsp or FUSE,
and is what the cfs black-box tests use when no mount driver is installed.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runCfs(args[0], func(ctx context.Context) (func(), error) {
			p := rc.Params{
				"type": "webdav", "fs": cfsFsName, "addr": cfsFlags.addr,
				"vfs_cache_mode": cfsFlags.cacheMode,
				"read_only":      cfsFlags.readOnly,
			}
			if cfsFlags.user != "" {
				p["user"], p["pass"] = cfsFlags.user, cfsFlags.pass
			}
			out, err := rc.Calls.Get("serve/start").Fn(ctx, p)
			if err != nil {
				return nil, fmt.Errorf("serve webdav: %w", err)
			}
			fmt.Printf("webdav of %s on %v\n", args[0], out["addr"])
			id, _ := out["id"].(string)
			return func() {
				if _, err := rc.Calls.Get("serve/stop").Fn(context.Background(), rc.Params{"id": id}); err != nil {
					log.Warnf("stop webdav: %v", err)
				}
			}, nil
		})
	},
}

// runCfs starts the server, waits for the cfs storage mounted at mountPath to
// have its volume open, runs attach with the volume registered in rclone's fs
// cache under cfsFsName, and tears everything down on SIGINT/SIGTERM.
func runCfs(mountPath string, attach func(ctx context.Context) (detach func(), err error)) {
	bootstrap.Init()
	defer bootstrap.Release()
	bootstrap.Start()

	key := cfsdrv.ShareKey(mountPath)
	var sh *cfsrclone.Shared
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if sh = cfsrclone.Lookup(key); sh != nil {
			break
		}
	}
	if sh == nil {
		log.Fatalf("no cfs storage is loaded at %s (does it exist, is it enabled, did it start?)", utils.FixAndCleanPath(mountPath))
	}
	ctx := context.Background()
	f, err := sh.NewFs(ctx, "cfs", "")
	if err != nil {
		log.Fatalf("cfs: %v", err)
	}
	cache.Put(cfsFsName, f)
	detach, err := attach(ctx)
	if err != nil {
		_ = f.(fs.Shutdowner).Shutdown(ctx)
		_ = sh.Release(ctx)
		log.Fatalf("%v", err)
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	detach()
	cache.Clear()
	if err := f.(fs.Shutdowner).Shutdown(ctx); err != nil {
		log.Warnf("cfs: flush: %v", err)
	}
	_ = sh.Release(ctx)
	bootstrap.Shutdown(1 * time.Second)
}

func init() {
	RootCmd.AddCommand(cfsCmd)
	serve := &cobra.Command{Use: "serve", Short: "Serve a cfs storage"}
	cfsCmd.AddCommand(cfsMountCmd, serve)
	serve.AddCommand(cfsWebdavCmd)
	for _, c := range []*cobra.Command{cfsMountCmd, cfsWebdavCmd} {
		c.Flags().StringVar(&cfsFlags.cacheMode, "vfs-cache-mode", "writes", "VFS cache mode: off, minimal, writes, full")
	}
	cfsWebdavCmd.Flags().StringVar(&cfsFlags.addr, "addr", "127.0.0.1:8085", "address to serve on")
	cfsWebdavCmd.Flags().StringVar(&cfsFlags.user, "user", "", "WebDAV user (none: anonymous)")
	cfsWebdavCmd.Flags().StringVar(&cfsFlags.pass, "pass", "", "WebDAV password")
	cfsWebdavCmd.Flags().BoolVar(&cfsFlags.readOnly, "read-only", false, "serve read-only")
}
