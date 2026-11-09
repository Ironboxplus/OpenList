#!/bin/sh
# Runs inside a FreeBSD VM (see .github/workflows/cfs-nfs.yml): serves a cfs
# storage over NFS with `openlist cfs serve nfs`, mounts it with the system's
# mount_nfs the way the NAS will (no mountport, so mount_nfs has to ask the
# portmap of the server), and checks data, renames and a restart of the server
# while the mount stays up. Expects ./openlist-freebsd (built for FreeBSD).
set -eu

D=/tmp/ol
MNT=/mnt/cfs
PORT=5244
BASE=http://127.0.0.1:$PORT
# The admin password never leaves this script's environment; nothing here echoes it.
PASS=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' 
')
[ ${#PASS} -ge 32 ] || { echo 'FAIL: no random password'; exit 1; }

rm -rf "$D"
mkdir -p "$D/data" "$D/back/vol" "$D/src" "$MNT"
cp ./openlist-freebsd "$D/openlist"
chmod +x "$D/openlist"
OL="$D/openlist --data $D/data --log-std"
printf '{"scheme":{"address":"127.0.0.1","http_port":%s,"https_port":-1}}' "$PORT" >"$D/data/config.json"

# Nothing may reach the internet from here (frontend fetcher etc.).
export HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 NO_PROXY=127.0.0.1,localhost
export OPENLIST_ADMIN_PASSWORD="$PASS"
# Where rclone's VFS keeps its cache: the write buffer test measures it.
export XDG_CACHE_HOME="$D/cache"
CACHE="$XDG_CACHE_HOME/rclone"

step() { echo; echo "=== $*"; }
fail() { echo "FAIL: $*"; exit 1; }

on_exit() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		echo; echo "##### FAILED (exit $rc), state for diagnosis"
		mount | grep "$MNT" || echo "(nothing mounted at $MNT)"
		sockstat -l | grep -E ':(111|2049|5244) ' || true
		for f in "$D"/*.log; do
			[ -f "$f" ] && { echo "----- tail of $f"; tail -n 150 "$f"; }
		done
	fi
	touch "$D/sampler.stop"; pkill smbd 2>/dev/null || true
	mount | grep -q " on $MNT " && umount -f "$MNT" 2>/dev/null || true
	[ -f "$D/pid" ] && kill -9 "$(cat "$D/pid")" 2>/dev/null || true
}
trap on_exit EXIT

# start LOG ARGS...: run openlist in the background, remember its pid.
start() {
	log=$1; shift
	( cd "$D" && $OL "$@" >>"$D/$log" 2>&1 & echo $! >"$D/pid" )
}

# stop_server: SIGTERM lets `cfs serve` flush the volume; wait for it.
stop_server() {
	pid=$(cat "$D/pid")
	kill -TERM "$pid" 2>/dev/null || return 0
	i=0
	while kill -0 "$pid" 2>/dev/null; do
		i=$((i + 1))
		[ "$i" -gt 90 ] && fail "openlist did not stop within 90s"
		sleep 1
	done
}

# wait_log LOG PATTERN: wait until the process wrote PATTERN (or died).
wait_log() {
	i=0
	until grep -q "$2" "$D/$1"; do
		kill -0 "$(cat "$D/pid")" 2>/dev/null || fail "openlist exited before logging '$2'"
		i=$((i + 1))
		[ "$i" -gt 180 ] && fail "no '$2' in $1 after 180s"
		sleep 1
	done
}

# The VM has no curl (and the 12.4 package repository is gone), so POST with nc.
post() { # post PATH JSON [TOKEN] -> response body
	len=$(printf '%s' "$2" | wc -c | tr -d ' ')
	printf 'POST %s HTTP/1.0\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nAuthorization: %s\r\nContent-Length: %s\r\n\r\n%s' \
		"$1" "${3:-}" "$len" "$2" | nc -w 30 127.0.0.1 "$PORT" | tr -d '\r' | sed '1,/^$/d'
}
api() { post "$1" "$2" "$TOKEN"; }

ok_code() { echo "$1" | grep -q '"code":200' || fail "$2: $(echo "$1" | head -c 300)"; }

# ---------------------------------------------------------------- 1. storages
step "first start: create the storages through the HTTP API"
start server.log server
i=0
until fetch -q -T 2 -o - "$BASE/ping" >/dev/null 2>&1; do
	i=$((i + 1)); [ "$i" -gt 60 ] && fail "server did not listen"; sleep 1
done
r=$(post /api/auth/login "{\"username\":\"admin\",\"password\":\"$PASS\"}")
ok_code "$r" login
TOKEN=$(echo "$r" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || fail "no token in login response"

r=$(api /api/admin/storage/create \
	"{\"mount_path\":\"/back\",\"driver\":\"Local\",\"order\":0,\"cache_expiration\":0,\"addition\":\"{\\\"root_folder_path\\\":\\\"$D/back\\\"}\"}")
ok_code "$r" "create /back"
r=$(api /api/admin/storage/create \
	'{"mount_path":"/cfs","driver":"Cfs","order":1,"cache_expiration":0,"addition":"{\"remote_path\":\"/back/vol\",\"transfers\":1,\"meta_inflight\":1}"}')
ok_code "$r" "create /cfs"
echo "storages created"
stop_server

# ------------------------------------------------- 2. no portmap: must not mount
step "without --portmap mount_nfs has to fail (nothing answers GETPORT for MOUNT)"
service rpcbind onestop 2>/dev/null || true
kldstat -q -m nfscl || kldload nfscl || true
start serve-noportmap.log cfs serve nfs /cfs --addr 127.0.0.1:2049
wait_log serve-noportmap.log '^nfs of /cfs on'
if timeout 120 mount_nfs -o nfsv3,tcp,port=2049,soft 127.0.0.1:/ "$MNT" >"$D/mount-noportmap.out" 2>&1; then
	umount -f "$MNT" || true
	fail "mount_nfs succeeded without a portmap; the test proves nothing"
fi
echo "mount_nfs failed as expected: $(head -c 300 "$D/mount-noportmap.out")"
mount | grep -q " on $MNT " && fail "something is mounted at $MNT after the failed mount"
stop_server

# ----------------------------------------------------------------- 3. mount
serve_with_portmap() { # serve_with_portmap LOG [ARGS...]: ARGS are more flags of `cfs serve nfs`
	log=$1; shift
	# --gid/--umask as the NAS will use them (group 1000 writes, others read).
	start "$log" cfs serve nfs /cfs --addr 127.0.0.1:2049 --portmap --gid 1000 --umask 002 "$@"
	wait_log "$log" '^nfs of /cfs on'
	wait_log "$log" 'portmap on tcp'
	# The service came up by itself: storages load concurrently, and the cfs
	# storage has to wait for the one under it instead of failing.
	if grep -q 'failed load storage' "$D/$log"; then
		grep 'failed load storage' "$D/$log" | cut -c1-300
		fail "a storage failed to load in $log (load-order race?)"
	fi
	grep -q 'success load storage: \[/cfs\]' "$D/$log" || fail "/cfs was not loaded in $log"
}
do_mount() {
	# The parameters of the NAS mount; no mountport on purpose.
	mount_nfs -o nfsv3,tcp,port=2049,soft 127.0.0.1:/ "$MNT" || fail "mount_nfs with --portmap failed"
	mount | grep " on $MNT "
}

step "with --portmap: mount"
serve_with_portmap serve-1.log
do_mount

# --------------------------------------------------------------------- 4. data
step "write 200 small files and one 30 MiB file, read them back"
mkdir -p "$D/src/small" "$D/src/big"
i=1
while [ "$i" -le 200 ]; do
	n=$(printf '%03d' "$i")
	dd if=/dev/urandom of="$D/src/small/$n" bs=1024 count=$((i % 40 + 1)) 2>/dev/null
	i=$((i + 1))
done
dd if=/dev/urandom of="$D/src/big/30m.bin" bs=1m count=30 2>/dev/null
cp -R "$D/src/small" "$D/src/big" "$MNT/"
sync

manifest() { (cd "$1" && find small big dir1 -type f 2>/dev/null | sort | xargs sha256 -r); }
verify() { # verify LABEL: the mount has exactly what src has
	manifest "$D/src" >"$D/expect.sha"
	manifest "$MNT" >"$D/got.sha"
	if ! diff "$D/expect.sha" "$D/got.sha" >"$D/diff.out"; then
		head -n 20 "$D/diff.out"; fail "$1: content differs from what was written"
	fi
	echo "$1: $(wc -l <"$D/got.sha" | tr -d ' ') files, sha256 identical"
}
verify "after write"

# Owner and mode as the client sees them: gid 1000, 0777/0666 minus umask 002.
check_perms() { # check_perms LABEL: stat of what exists at this point
	for d in small big "$@"; do
		[ "$(stat -f '%g %Lp' "$MNT/$d")" = "1000 775" ] || fail "$LABEL: $d is '$(stat -f '%g %Lp' "$MNT/$d")', want '1000 775' (gid, mode)"
	done
	for f in small/003 big/30m.bin; do
		[ "$(stat -f '%g %Lp' "$MNT/$f")" = "1000 664" ] || fail "$LABEL: $f is '$(stat -f '%g %Lp' "$MNT/$f")', want '1000 664' (gid, mode)"
	done
	echo "$LABEL: gid/mode of dirs 1000 775, of files 1000 664"
}
LABEL="after write"; check_perms

step "mv, rm, mkdir, ls -l"
mutate() {
	mkdir "$1/dir1"
	mv "$1/small/001" "$1/dir1/001.renamed"
	rm "$1/small/002"
	mkdir "$1/small/sub"
	mv "$1/small/sub" "$1/dir1/sub"
}
mutate "$MNT"
mutate "$D/src"
ls -l "$MNT" "$MNT/dir1"
[ "$(ls "$MNT/small" | wc -l | tr -d ' ')" = 198 ] || fail "small/ should hold 198 entries"
[ -d "$MNT/dir1/sub" ] || fail "dir1/sub is missing"
[ ! -e "$MNT/small/002" ] || fail "small/002 survived rm"
verify "after mv/rm"
mkdir "$MNT/newdir"; echo x >"$MNT/newdir/newfile"
[ "$(stat -f '%g %Lp' "$MNT/newdir")" = "1000 775" ] || fail "new dir: '$(stat -f '%g %Lp' "$MNT/newdir")', want '1000 775'"
[ "$(stat -f '%g %Lp' "$MNT/newdir/newfile")" = "1000 664" ] || fail "new file: '$(stat -f '%g %Lp' "$MNT/newdir/newfile")', want '1000 664'"
echo "created by the client: gid/mode 1000 775 (dir), 1000 664 (file)"
rm -r "$MNT/newdir"

# ----------------------------------------------------------- 5. server restart
step "restart openlist while the mount stays up"
ids() { for f in dir1 dir1/001.renamed small/003 big/30m.bin; do echo "$f $(stat -f %i "$MNT/$f")"; done; }
ids >"$D/ids-before.txt"
sleep 8 # past the VFS write-back delay
stop_server
echo "server stopped, mount still in place: $(mount | grep -c " on $MNT ") mount(s)"
serve_with_portmap serve-2.log
# Past the attribute cache (3s default), so that stat and ls really send
# GETATTR/LOOKUP with the file handles this client got from the first process.
sleep 8
echo "handle cache dir after restart:"; find "$D/data/cfs-nfs" -type f | head -n 12
tcpdump -l -i lo0 -nn -vv -c 200 'tcp port 2049' >"$D/nfs-after-restart.pcap.txt" 2>&1 &
sleep 1
if ! ls -l "$MNT/dir1" >/dev/null; then
	echo "--- fileids (stat %i) before the restart:"; cat "$D/ids-before.txt"
	umount -f "$MNT" || true
	do_mount || true
	echo "--- fileids after a fresh mount against the restarted server:"; ids || true
	echo "--- other lookups after the failure"
	ls "$MNT" || true
	ls "$MNT/big" || true
	stat "$MNT/dir1" || true
	sleep 2
	echo "--- nfs traffic of the failed ls"
	cut -c1-220 "$D/nfs-after-restart.pcap.txt" | head -n 80
	nfsstat -c | head -n 30 || true
	fail "ls after restart failed (stale file handle?)"
fi
verify "after restart, same mount"
LABEL="after restart"; check_perms dir1
echo "new file after restart" >"$MNT/after-restart.txt"
[ "$(cat "$MNT/after-restart.txt")" = "new file after restart" ] || fail "write after restart"
echo "new file after restart" >"$D/src/after-restart.txt"
sync

# ------------------------------------------------------------------ 6. remount
step "umount, mount again, compare once more"
sleep 8
umount "$MNT" || fail "umount"
do_mount
manifest_extra() { (cd "$1" && sha256 -r after-restart.txt); }
[ "$(manifest_extra "$MNT")" = "$(manifest_extra "$D/src")" ] || fail "after-restart.txt differs after remount"
verify "after remount"
LABEL="after remount"; check_perms dir1
ids >"$D/ids-after.txt"
diff "$D/ids-before.txt" "$D/ids-after.txt" || fail "fileids changed across the restart (clients that stay mounted get EIDRM)"
echo "fileids (stat %i) identical before the restart and after the remount:"; cat "$D/ids-after.txt"

# ------------------------------------------------------ 7. write buffer limit
dirty_bytes() { # size of the cache files that are not uploaded yet
	total=0
	for m in $(grep -rl '"Dirty": true' "$CACHE/vfsMeta" 2>/dev/null); do
		sz=$(stat -f %z "$CACHE/vfs/${m#"$CACHE"/vfsMeta/}" 2>/dev/null || echo 0)
		total=$((total + sz))
	done
	echo "$total"
}
wait_uploaded() { # wait_uploaded SECONDS: until no cache file is dirty for 3 checks in a row
	i=0; quiet=0
	while [ "$quiet" -lt 3 ]; do
		i=$((i + 1))
		[ "$i" -gt "$1" ] && fail "uploads not finished after ${1}s, $(dirty_bytes) bytes are still dirty"
		if [ "$(dirty_bytes)" = 0 ]; then quiet=$((quiet + 1)); else quiet=0; fi
		sleep 1
	done
}

step "write buffer: 64 MiB limit, uploads at 1 MiB/s, 4 files of 50 MiB"
umount "$MNT" || fail "umount"
stop_server
serve_with_portmap serve-3.log --vfs-cache-max-dirty 64M --bwlimit 1M
# hard, as the NAS will mount: a write that waits for the uploads must not
# turn into EIO the way it does on a soft mount once its timeout is over.
mount_nfs -o nfsv3,tcp,port=2049,hard 127.0.0.1:/ "$MNT" || fail "hard mount"
mount | grep " on $MNT "
LIMIT=67108864
FILE=52428800
mkdir -p "$D/src/bp" "$MNT/bp"
for n in 1 2 3 4; do dd if=/dev/urandom of="$D/src/bp/f$n.bin" bs=1m count=50 2>/dev/null; done
echo 0 >"$D/dirty.max"
rm -f "$D/sampler.stop"
(
	while [ ! -e "$D/sampler.stop" ]; do
		b=$(dirty_bytes)
		[ "$b" -gt "$(cat "$D/dirty.max")" ] && echo "$b" >"$D/dirty.max"
		sleep 0.5
	done
) &
SAMPLER=$!
t0=$(date +%s)
for n in 1 2 3 4; do
	cp "$D/src/bp/f$n.bin" "$MNT/bp/f$n.bin" || fail "writing f$n.bin"
done
t1=$(date +%s)
wait_uploaded 900
t2=$(date +%s)
touch "$D/sampler.stop"
wait "$SAMPLER" || true
MAX=$(cat "$D/dirty.max")
echo "most bytes not uploaded at once: $MAX (limit $LIMIT, one file $FILE)"
echo "the writes took $((t1 - t0))s, everything was uploaded after $((t2 - t0))s; cache dir now $(du -sk "$CACHE/vfs" | cut -f1) KiB"
[ "$MAX" -gt 0 ] || fail "never saw a dirty cache file: the measurement does not work"
[ "$MAX" -le $((LIMIT + FILE)) ] || fail "the cache held $MAX bytes not uploaded, more than the limit plus one file"
[ $((t1 - t0)) -ge 60 ] || fail "200 MiB were written in $((t1 - t0))s with uploads at 1 MiB/s: the writes did not wait"
for n in 1 2 3 4; do
	want=$(sha256 -q "$D/src/bp/f$n.bin")
	[ "$(sha256 -q "$MNT/bp/f$n.bin")" = "$want" ] || fail "f$n.bin differs when read back"
	[ "$(sha256 -q "$D/back/vol/bp/f$n.bin")" = "$want" ] || fail "f$n.bin on the drive under cfs differs"
done
echo "4 files read back and on the drive: sha256 identical"

step "one file bigger than the limit: ENOSPC, and the mount keeps working"
dd if=/dev/zero of="$D/src/over.bin" bs=1m count=100 2>/dev/null
if dd if="$D/src/over.bin" of="$MNT/over.bin" bs=1m conv=fsync >"$D/over.out" 2>&1; then
	cat "$D/over.out"; fail "100 MiB were written with a 64 MiB limit"
fi
cat "$D/over.out"
grep -qi 'no space left on device' "$D/over.out" || fail "the write failed, but not with ENOSPC"
rm -f "$MNT/over.bin" || true
echo "still works" >"$MNT/after-enospc.txt"
[ "$(cat "$MNT/after-enospc.txt")" = "still works" ] || fail "write after ENOSPC"
ls "$MNT/bp" >/dev/null || fail "ls after ENOSPC"
[ "$(sha256 -q "$MNT/bp/f1.bin")" = "$(sha256 -q "$D/src/bp/f1.bin")" ] || fail "read after ENOSPC"
echo "ENOSPC as expected, writes and reads work afterwards"
wait_uploaded 300

# ------------------------------------------------------------------ 8. SMB
# What broke on the NAS on 11-09: smbd writing to this mount. Samba shares the
# NFS mount the way it will be shared there (no VFS modules, no ACLs, no DOS
# attributes: none of them can be stored on this mount).
smb_step() {
	step "SMB: Samba shares the NFS mount; smbclient writes a file and reads it back"
	# The package repository is on the internet, which the proxy variables above shut.
	pkgenv="env -u HTTP_PROXY -u HTTPS_PROXY -u NO_PROXY ASSUME_ALWAYS_YES=yes"
	$pkgenv pkg update -f || fail "pkg update"
	smbpkg=$($pkgenv pkg rquery -x '%n' '^samba[0-9]+$' | sort | tail -n 1)
	[ -n "$smbpkg" ] || fail "no samba package in the repository"
	echo "installing $smbpkg"
	$pkgenv pkg install -y "$smbpkg" || fail "pkg install $smbpkg"
	mkdir -p "$D/smb/ncalrpc"
	cat >"$D/smb.conf" <<EOF
[global]
	workgroup = WORKGROUP
	server role = standalone server
	security = user
	map to guest = Bad User
	disable netbios = yes
	smb ports = 445
	bind interfaces only = yes
	interfaces = 127.0.0.1
	load printers = no
	printing = bsd
	printcap name = /dev/null
	disable spoolss = yes
	log file = $D/smb-%m.log
	log level = 1
	pid directory = $D/smb
	lock directory = $D/smb
	state directory = $D/smb
	cache directory = $D/smb
	private dir = $D/smb
	ncalrpc dir = $D/smb/ncalrpc
	vfs objects =
	nt acl support = no
	store dos attributes = no

[cfs]
	path = $MNT
	read only = no
	guest ok = yes
	force user = root
	create mask = 0664
	directory mask = 0775
EOF
	smbd -D -s "$D/smb.conf" || fail "smbd did not start"
	i=0
	until sockstat -l -4 | grep -q ':445 '; do
		i=$((i + 1)); [ "$i" -gt 30 ] && fail "smbd does not listen on 445"; sleep 1
	done
	mkdir -p "$D/src/smb"
	dd if=/dev/urandom of="$D/src/smb/smb.bin" bs=1m count=20 2>/dev/null
	want=$(sha256 -q "$D/src/smb/smb.bin")
	smbclient //127.0.0.1/cfs -N -c "put $D/src/smb/smb.bin smb.bin" || fail "smbclient put"
	smbclient //127.0.0.1/cfs -N -c "ls smb.bin" || fail "smbclient ls"
	smbclient //127.0.0.1/cfs -N -c "get smb.bin $D/smb-back.bin" || fail "smbclient get"
	[ "$(sha256 -q "$D/smb-back.bin")" = "$want" ] || fail "the file read back over SMB differs"
	[ "$(sha256 -q "$MNT/smb.bin")" = "$want" ] || fail "the file written over SMB differs on the NFS mount"
	echo "SMB write and read back: sha256 identical"
	pkill smbd || true
}
case $(uname -r) in
12.*) echo "SMB step skipped: the package repository of FreeBSD 12.4 is gone" ;;
*) smb_step ;;
esac
wait_uploaded 300

umount "$MNT"
stop_server
echo
echo "cfs serve nfs: all checks passed"
