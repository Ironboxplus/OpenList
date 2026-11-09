//go:build unix

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/pkg/portmap"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	_ "github.com/rclone/rclone/cmd/serve/nfs"
	"github.com/rclone/rclone/fs/rc"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var cfsNfsFlags struct {
	addr         string
	portmap      bool
	cacheMaxSize string
	readOnly     bool
}

var cfsNfsCmd = &cobra.Command{
	Use:   "nfs <storage-mount-path>",
	Short: "Serve a cfs storage over NFSv3 (mountable by the kernel NFS client)",
	Long: `Serve a cfs storage over NFSv3 through rclone's VFS, for a host that mounts it
with its own NFS client (mount_nfs on FreeBSD, mount -t nfs on Linux).

NFS has NO AUTHENTICATION: anyone who can reach the address can read and write
the volume. Bind it to an address only the mounting host can reach (the default
is loopback), and do not forward the port.

--portmap serves a read-only portmap (UDP and TCP port 111 on the same IP as
--addr) that points MOUNT and NFS v3 at the NFS port. It is needed when the
client cannot be given the mount port: FreeBSD's mount_nfs has no mountport
option before 14.x and always asks the server's portmapper first. Binding port
111 needs privileges on most systems and the port must be free (no rpcbind).

The NFS file handles are kept on disk under <data>/cfs-nfs, so that a client
that stays mounted keeps working across a restart of this process (with the
default in-memory handle cache every handle goes stale on restart).

--uid, --gid and --umask set the owner and the permission bits the client
sees (NFS carries no users of its own here): every file and directory belongs
to that uid/gid, with 0666/0777 minus the umask. For a share used by a group,
e.g. --gid 1000 --umask 002 gives 664 for files and 775 for directories.

Example:
  openlist cfs serve nfs /cfs --addr 127.0.0.1:2049 --portmap
  mount_nfs -o nfsv3,tcp,port=2049,soft 127.0.0.1:/ /mnt/cfs`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runCfs(args[0], func(ctx context.Context) (func(), error) {
			p := rc.Params{
				"type": "nfs", "fs": cfsFsName, "addr": cfsNfsFlags.addr,
				"vfs_cache_mode":     cfsFlags.cacheMode,
				"vfs_cache_max_size": cfsNfsFlags.cacheMaxSize,
				"read_only":          cfsNfsFlags.readOnly,
				"nfs_cache_type":     "disk",
				"nfs_cache_dir":      filepath.Join(flags.DataDir, "cfs-nfs", cfsNfsDirName(args[0])),
			}
			cfsOwnership(p)
			out, err := rc.Calls.Get("serve/start").Fn(ctx, p)
			if err != nil {
				return nil, fmt.Errorf("serve nfs: %w", err)
			}
			addr, _ := out["addr"].(string)
			id, _ := out["id"].(string)
			stopNfs := func() {
				if _, err := rc.Calls.Get("serve/stop").Fn(context.Background(), rc.Params{"id": id}); err != nil {
					log.Warnf("stop nfs: %v", err)
				}
			}
			fmt.Printf("nfs of %s on %s\n", args[0], addr)

			stopPortmap := func() {}
			if cfsNfsFlags.portmap {
				host, portStr, err := net.SplitHostPort(addr)
				port, perr := strconv.ParseUint(portStr, 10, 16)
				if err != nil || perr != nil {
					stopNfs()
					return nil, fmt.Errorf("portmap: cannot take the nfs port from %q", addr)
				}
				stopPortmap, err = portmap.Serve(ctx, host, uint32(port))
				if err != nil {
					stopNfs()
					return nil, fmt.Errorf("portmap on %s:111: %w", host, err)
				}
			}
			return func() {
				stopPortmap()
				stopNfs()
			}, nil
		})
	},
}

var cfsNfsUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// cfsNfsDirName is a directory name for the handle cache of the storage at
// mountPath: readable, and unique per path (the hash tells "/a/b" and "/a_b" apart).
func cfsNfsDirName(mountPath string) string {
	p := utils.FixAndCleanPath(mountPath)
	sum := sha256.Sum256([]byte(p))
	name := strings.Trim(cfsNfsUnsafe.ReplaceAllString(p, "_"), "_")
	if name == "" {
		name = "root"
	}
	return name + "-" + hex.EncodeToString(sum[:4])
}

func init() {
	cfsServeCmd.AddCommand(cfsNfsCmd)
	f := cfsNfsCmd.Flags()
	f.StringVar(&cfsFlags.cacheMode, "vfs-cache-mode", "writes", "VFS cache mode: off, minimal, writes, full")
	f.StringVar(&cfsNfsFlags.addr, "addr", "127.0.0.1:2049", "address to serve on (no authentication: keep it reachable by the mounting host only)")
	f.BoolVar(&cfsNfsFlags.portmap, "portmap", false, "also serve portmap (UDP+TCP port 111) on the IP of --addr, for clients that cannot be given the mount port")
	f.StringVar(&cfsNfsFlags.cacheMaxSize, "vfs-cache-max-size", "10G", "upper bound of the VFS cache on local disk")
	f.BoolVar(&cfsNfsFlags.readOnly, "read-only", false, "serve read-only")
	cfsOwnershipFlags(cfsNfsCmd)
}
