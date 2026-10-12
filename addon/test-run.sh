#!/bin/sh
# Exercises addon/run.sh in an alpine:3.24 container with a stub releases API
# and stub daemon binaries. The stub API is a file:// URL in FREEDO_RELEASES_URL
# and the assets are file:// URLs in the release JSON, so no network is needed.
# Requires docker. Run it from the repository root:
#   addon/test-run.sh
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

case "$(uname -m)" in
  aarch64|arm64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac

image=freedo-runsh-test
if ! docker image inspect "$image" > /dev/null 2>&1; then
  mkdir -p "$work/img"
  printf 'FROM alpine:3.24\nRUN apk add --no-cache jq curl\n' > "$work/img/Dockerfile"
  docker build -t "$image" "$work/img" > /dev/null
fi

cp "$repo/addon/run.sh" "$work/run.sh"
chmod +x "$work/run.sh"

fails=0
ok() { echo "ok - $1"; }
no() { echo "FAIL - $1"; fails=$((fails + 1)); }

# write_stub writes a stub daemon that answers --version with $2 and
# --check-state with 3 when the database mentions "newer", else 0.
write_stub() {
  out=$1 ver=$2
  {
    echo '#!/bin/sh'
    echo 'case "$1" in'
    echo "  --version) echo \"freedo $ver (stub stub)\"; exit 0 ;;"
    echo '  --check-state)'
    echo '    if [ -f "$2" ] && grep -q newer "$2" 2>/dev/null; then exit 3; fi'
    echo '    exit 0 ;;'
    echo 'esac'
    echo "echo \"RAN $ver \$*\""
    echo 'exit 0'
  } > "$out"
  chmod +x "$out"
}

# run_case runs run.sh for $1 with /data at $2 and the releases URL $3, prints
# the combined output and returns the container status.
run_case() {
  data=$1 releases=$2 bundled=$3
  docker run --rm --user "$(id -u):$(id -g)" \
    -v "$data:/data:Z" \
    -v "$work/run.sh:/run.sh:ro,Z" \
    -v "$bundled:/usr/local/bin/freedo:ro,Z" \
    -e "FREEDO_RELEASES_URL=$releases" \
    -e "SUPERVISOR_TOKEN=stub" \
    "$image" /run.sh 2>&1
}

contains() { echo "$2" | grep -qF "$1"; }

# --- stable: the bundled daemon, no download ---------------------------------
d="$work/stable/data"; mkdir -p "$d"
write_stub "$work/stable/bundled" 0.3.2
printf '{"api_token":"tok","transmit":false,"channel":"stable"}\n' > "$d/options.json"
out=$(run_case "$d" "file:///data/none.json" "$work/stable/bundled") || true
if contains "running the bundled daemon (channel stable)" "$out" && contains "RAN 0.3.2" "$out"; then
  ok "stable runs the bundled daemon"
else no "stable runs the bundled daemon: $out"; fi
if [ ! -d "$d/channel" ]; then ok "stable created no channel cache"; else no "stable created a channel cache"; fi

# --- beta: a newer pre-release is downloaded, verified and run, cache pruned --
d="$work/beta/data"; mkdir -p "$d/assets" "$d/channel/0.1.0-beta.1" "$d/channel/0.2.0-beta.1"
write_stub "$work/beta/bundled" 0.3.2
write_stub "$d/assets/freedo-linux-$arch" 0.9.0-beta.1
(cd "$d/assets" && sha256sum "freedo-linux-$arch" > SHA256SUMS)
write_stub "$d/channel/0.1.0-beta.1/freedo-linux-$arch" 0.1.0-beta.1
write_stub "$d/channel/0.2.0-beta.1/freedo-linux-$arch" 0.2.0-beta.1
for v in 0.1.0-beta.1 0.2.0-beta.1 0.3.0-beta.1; do
  mkdir -p "$d/backup/before-$v"; printf 'db %s\n' "$v" > "$d/backup/before-$v/freedo-state.db"
done
printf '[{"tag_name":"v0.9.0-beta.1","prerelease":true,"assets":[{"name":"freedo-linux-%s","browser_download_url":"file:///data/assets/freedo-linux-%s"},{"name":"SHA256SUMS","browser_download_url":"file:///data/assets/SHA256SUMS"}]}]\n' "$arch" "$arch" > "$d/releases.json"
printf '{"api_token":"tok","channel":"beta"}\n' > "$d/options.json"
out=$(run_case "$d" "file:///data/releases.json" "$work/beta/bundled") || true
if contains "beta 0.9.0-beta.1 downloaded" "$out" && contains "RAN 0.9.0-beta.1" "$out"; then
  ok "beta downloads and runs the newer pre-release"
else no "beta downloads and runs the newer pre-release: $out"; fi
cache=$(ls "$d/channel" 2>/dev/null | sort | tr '\n' ' ')
if [ "$cache" = "0.2.0-beta.1 0.9.0-beta.1 " ]; then
  ok "beta keeps the two newest cached versions"
else no "beta keeps the two newest cached versions: got '$cache'"; fi
if [ -d "$d/backup/before-0.9.0-beta.1" ]; then ok "beta backs up before the first start"; else no "beta backs up before the first start"; fi
backups=$(ls "$d/backup" 2>/dev/null | sort | tr '\n' ' ')
if [ "$backups" = "before-0.2.0-beta.1 before-0.3.0-beta.1 before-0.9.0-beta.1 " ]; then
  ok "beta keeps the three newest backups"
else no "beta keeps the three newest backups: got '$backups'"; fi

# --- beta: the network is down, the cache holds a newer beta -----------------
d="$work/down/data"; mkdir -p "$d/channel/0.5.0-beta.1"
write_stub "$work/down/bundled" 0.3.2
write_stub "$d/channel/0.5.0-beta.1/freedo-linux-$arch" 0.5.0-beta.1
printf '{"api_token":"tok","channel":"beta"}\n' > "$d/options.json"
out=$(run_case "$d" "file:///data/absent.json" "$work/down/bundled") || true
if contains "beta 0.5.0-beta.1 from the channel cache" "$out" && contains "RAN 0.5.0-beta.1" "$out"; then
  ok "beta falls back to the cache when the network is down"
else no "beta falls back to the cache when the network is down: $out"; fi

# --- beta: no newer pre-release, the bundled daemon runs ---------------------
d="$work/old/data"; mkdir -p "$d/assets"
write_stub "$work/old/bundled" 0.3.2
printf '[{"tag_name":"v0.2.0-beta.1","prerelease":true,"assets":[{"name":"freedo-linux-%s","browser_download_url":"file:///data/assets/x"},{"name":"SHA256SUMS","browser_download_url":"file:///data/assets/x"}]}]\n' "$arch" > "$d/releases.json"
printf '{"api_token":"tok","channel":"beta"}\n' > "$d/options.json"
out=$(run_case "$d" "file:///data/releases.json" "$work/old/bundled") || true
if contains "no pre-release newer than 0.3.2" "$out" && contains "RAN 0.3.2" "$out"; then
  ok "beta with no newer pre-release runs the bundled daemon"
else no "beta with no newer pre-release runs the bundled daemon: $out"; fi

# --- beta: a checksum mismatch is refused, the bundled daemon runs -----------
d="$work/sum/data"; mkdir -p "$d/assets"
write_stub "$work/sum/bundled" 0.3.2
write_stub "$d/assets/freedo-linux-$arch" 0.9.0-beta.1
printf '%s  freedo-linux-%s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$arch" > "$d/assets/SHA256SUMS"
printf '[{"tag_name":"v0.9.0-beta.1","prerelease":true,"assets":[{"name":"freedo-linux-%s","browser_download_url":"file:///data/assets/freedo-linux-%s"},{"name":"SHA256SUMS","browser_download_url":"file:///data/assets/SHA256SUMS"}]}]\n' "$arch" "$arch" > "$d/releases.json"
printf '{"api_token":"tok","channel":"beta"}\n' > "$d/options.json"
out=$(run_case "$d" "file:///data/releases.json" "$work/sum/bundled") || true
if contains "RAN 0.3.2" "$out" && [ ! -d "$d/channel/0.9.0-beta.1" ]; then
  ok "beta refuses a checksum mismatch and runs the bundled daemon"
else no "beta refuses a checksum mismatch and runs the bundled daemon: $out"; fi

# --- stable restore: a newer database is set aside, a backup is restored -----
d="$work/restore/data"; mkdir -p "$d/backup/before-0.9.0-beta.1"
write_stub "$work/restore/bundled" 0.3.2
printf 'beta wrote this: newer\n' > "$d/freedo-state.db"
printf 'stable database\n' > "$d/backup/before-0.9.0-beta.1/freedo-state.db"
printf '{"api_token":"tok","channel":"stable"}\n' > "$d/options.json"
out=$(run_case "$d" "file:///data/none.json" "$work/restore/bundled") || true
aside=$(ls -d "$d"/backup/beta-* 2>/dev/null | head -1)
if contains "set aside" "$out" && contains "RAN 0.3.2" "$out" \
   && [ -n "$aside" ] && grep -q newer "$aside/freedo-state.db" \
   && ! grep -q newer "$d/freedo-state.db"; then
  ok "stable restores the backup and sets the beta database aside"
else no "stable restores the backup and sets the beta database aside: $out"; fi

# --- stable: a newer database with no backup stops the start -----------------
d="$work/nobkp/data"; mkdir -p "$d"
write_stub "$work/nobkp/bundled" 0.3.2
printf 'beta wrote this: newer\n' > "$d/freedo-state.db"
printf '{"api_token":"tok","channel":"stable"}\n' > "$d/options.json"
if out=$(run_case "$d" "file:///data/none.json" "$work/nobkp/bundled") && contains "RAN" "$out"; then
  no "stable stops when a newer database has no backup: $out"
elif contains "ERROR" "$out"; then
  ok "stable stops when a newer database has no backup"
else no "stable stops when a newer database has no backup: $out"; fi

echo
if [ "$fails" = 0 ]; then echo "all run.sh tests passed"; else echo "$fails run.sh test(s) failed"; exit 1; fi
