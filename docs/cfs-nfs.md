# cfs over NFS (`openlist cfs serve nfs`)

A cfs volume has one writer, so a host that wants to see it as a directory
mounts it from the one OpenList process that owns it. `serve nfs` puts rclone's
VFS in front of the volume and exports it over NFSv3; the host mounts it with
its kernel NFS client.

```
openlist cfs serve nfs /cfs --addr 127.0.0.1:2049 --portmap
mount_nfs -o nfsv3,tcp,port=2049,soft 127.0.0.1:/ /mnt/cfs      # FreeBSD
mount -t nfs -o vers=3,proto=tcp,port=2049,mountport=2049 127.0.0.1:/ /mnt/cfs   # Linux
```

Code: `cmd/cfs_nfs.go` (unix only), `pkg/portmap/`. CI: `.github/workflows/cfs-nfs.yml`
with the VM script `.github/scripts/cfs-nfs-freebsd.sh`.

| Flag | Default | |
|------|---------|-|
| `--addr` | `127.0.0.1:2049` | Address of the NFS server. |
| `--portmap` | off | Also serve portmap on port 111 (UDP and TCP) of the IP of `--addr`. |
| `--vfs-cache-mode` | `writes` | As for the other `cfs` commands. |
| `--vfs-cache-max-size` | `10G` | Bound of the VFS cache on local disk. |
| `--read-only` | off | |

**No authentication.** NFSv3 as served here trusts every client that can reach
the address. Bind an address only the mounting host can reach and do not
forward the port.

## File handles are kept on disk

The handles the server gives out live in `<data>/cfs-nfs/<storage path>-<hash>`
(`nfs_cache_type=disk`). With rclone's default in-memory cache a restart of the
process makes every handle of a client that stays mounted stale
("Stale file handle"); with the disk cache the mount keeps working. The CI job
restarts the server under a live mount to check this.

## Why a portmap

`mount_nfs` first needs the root file handle, which comes from the MOUNT
protocol (100005 v3). Its port is not the NFS port in general, so the client
asks the server's portmapper (RFC 1833). rclone's NFS server answers MOUNT and
NFS on one TCP port but registers with no portmapper.

- Linux `mount.nfs` takes `mountport=`. FreeBSD's `mount_nfs` has no such
  option before 14.x (`port=` is the NFS port only): 12.4, 13.1 and 13.5 all
  call `clnt_tp_create(host, MOUNTPROG, 3, ...)`, which asks rpcbind.
- libc is built with `-DPORTMAP`, so for IPv4 the first thing it sends is a
  portmap v2 `PMAPPROC_GETPORT {prog=100005, vers=3, prot=TCP}`. On 13.1 and
  12.4 that goes over UDP port 111, from 13.2 on over TCP port 111.

`pkg/portmap` answers exactly that, on both transports: MOUNT v3 and NFS v3
(TCP) map to the NFS port, everything else to 0. `SET` and `UNSET` always
return false (honouring them would let any client redirect the NFS traffic),
`CALLIT` is `PROC_UNAVAIL`, `DUMP` lists the two mappings, other versions of
the portmap program get `PROG_MISMATCH` with the supported range 2..2. Binding
port 111 needs privileges and the port must be free (no rpcbind running).

The XDR types are the generated ones of `github.com/zeldovich/go-rpcgen`
(copied into `pkg/portmap/rfc1057` and `pkg/portmap/xdr`); the request
handling (decode the call, dispatch, encode the reply) is that library's, split
from the transport so UDP datagrams and TCP records share it. The mapping
handlers follow the portmap of e2b-dev/infra.
