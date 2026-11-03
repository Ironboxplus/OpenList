#!/usr/bin/env bash
# build.sh embeds the frontend release named by FRONTEND_TAG (default rolling)
# and leaves a public/dist/VERSION behind. curl and jq are stubbed: nothing
# leaves the host.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/repo/public" "$work/payload"
cp "$here/build.sh" "$work/repo/"
echo '<html>embedded</html>' > "$work/payload/index.html"
tar -czf "$work/frontend.tar.gz" -C "$work/payload" index.html

cat > "$work/bin/curl" <<STUB
#!/usr/bin/env bash
echo "\$*" >> "$work/curl.log"
out=""; url=""
while [ \$# -gt 0 ]; do case "\$1" in -o) out=\$2; shift;; http*) url=\$1;; esac; shift; done
case "\$url" in
  *releases/tags/*) echo '{"tag_name":"x","published_at":"2026-11-03T00:00:00Z","assets":[{"browser_download_url":"http://stub/openlist-frontend-dist.tar.gz"}]}';;
  *.tar.gz) cp "$work/frontend.tar.gz" "\$out";;
esac
STUB
chmod +x "$work/bin/curl"

# the two jq filters build.sh uses, answered with sed so the test needs no jq
cat > "$work/bin/jq" <<'JQ'
#!/usr/bin/env bash
case "$*" in
  *browser_download_url*) sed -n 's/.*"browser_download_url":"\([^"]*\)".*/\1/p';;
  *published_at*) sed -n 's/.*"published_at":"\([^"]*\)".*/\1/p';;
esac
JQ
chmod +x "$work/bin/jq"

run() { # run <FRONTEND_TAG, or empty for the default>
  : > "$work/curl.log"
  git -C "$work/repo" init -q . 2>/dev/null || true
  git -C "$work/repo" -c user.name=t -c user.email=t@t commit -q --allow-empty -m t 2>/dev/null || true
  if ! (cd "$work/repo" && env ${1:+FRONTEND_TAG=$1} PATH="$work/bin:$PATH" bash build.sh dev web >"$work/out.log" 2>&1); then
    tail -15 "$work/out.log"; echo "FAIL: build.sh failed"; exit 1
  fi
}

fail=0
run canary
[[ $(<"$work/curl.log") == *releases/tags/canary* ]] || { echo "FAIL: FRONTEND_TAG=canary did not ask for the canary release:"; cat "$work/curl.log"; fail=1; }
[[ $(<"$work/repo/public/dist/VERSION") == canary@2026-11-03* ]] || { echo "FAIL: public/dist/VERSION is missing or wrong"; fail=1; }
run ""
[[ $(<"$work/curl.log") == *releases/tags/rolling* ]] || { echo "FAIL: the default is not rolling"; cat "$work/curl.log"; fail=1; }
[ $fail = 0 ] && echo "PASS"
exit $fail
