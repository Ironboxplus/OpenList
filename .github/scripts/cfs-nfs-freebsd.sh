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
serve_with_portmap() {
	start "$1" cfs serve nfs /cfs --addr 127.0.0.1:2049 --portmap
	wait_log "$1" '^nfs of /cfs on'
	wait_log "$1" 'portmap on tcp'
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
ids >"$D/ids-after.txt"
diff "$D/ids-before.txt" "$D/ids-after.txt" || fail "fileids changed across the restart (clients that stay mounted get EIDRM)"
echo "fileids (stat %i) identical before the restart and after the remount:"; cat "$D/ids-after.txt"

umount "$MNT"
stop_server
echo
echo "cfs serve nfs: all checks passed"
