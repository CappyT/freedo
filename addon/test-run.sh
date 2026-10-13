#!/bin/sh
# Exercises addon/run.sh in an alpine:3.24 container with stub daemon binaries,
# as the stable and as the beta add-on. The Supervisor is absent, so the
# discovery call fails with a warning. No network is needed.
# Requires docker. Run it from the repository root:
#   addon/test-run.sh
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

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

# write_stub writes a stub daemon of version $2 to $1. --check-state reads the
# schema from the database text: "schema N", status 3 when N is above $3, status
# 1 when the text says "broken". A run appends "ran <version>" to the database.
# With $4 = slow, the run waits for SIGTERM and then appends "stopped".
write_stub() {
  out=$1 ver=$2 reads=$3 mode=${4:-}
  {
    echo '#!/bin/sh'
    echo 'case "$1" in'
    echo "  --version) echo \"freedo $ver (stub stub)\"; exit 0 ;;"
    echo '  --check-state)'
    echo '    [ -f "$2" ] || { echo "check-state: $2: no state database"; exit 0; }'
    echo '    if grep -q broken "$2"; then exit 1; fi'
    echo '    n=$(sed -n "s/^schema \([0-9]*\)$/\1/p" "$2" | tail -1)'
    echo "    if [ \"\$n\" -gt $reads ]; then echo \"check-state: \$2: schema version \$n is newer than this binary reads ($reads)\"; exit 3; fi"
    echo '    echo "check-state: $2: schema version $n is readable"; exit 0 ;;'
    echo 'esac'
    echo "echo \"RAN $ver \$*\""
    echo "echo \"ran $ver\" >> /data/freedo-state.db"
    if [ "$mode" = slow ]; then
      echo 'trap "echo stopped >> /data/freedo-state.db; exit 0" TERM'
      echo 'sleep 30 & wait'
    fi
    echo 'exit 0'
  } > "$out"
  chmod +x "$out"
}

# run_case runs run.sh with /data at $1, /share at $2, the daemon $3 and the
# container host name $4. Every further argument is a docker run option. It
# prints the combined output and returns the container status.
run_case() {
  data=$1 share=$2 bundled=$3 host=$4
  shift 4
  docker run --rm --user "$(id -u):$(id -g)" --hostname "$host" \
    -v "$data:/data:Z" \
    -v "$share:/share:Z" \
    -v "$work/run.sh:/run.sh:ro,Z" \
    -v "$bundled:/usr/local/bin/freedo:ro,Z" \
    -e "SUPERVISOR_TOKEN=stub" \
    "$@" "$image" /run.sh 2>&1
}

contains() { echo "$2" | grep -qF "$1"; }

stable=abc-freedo
beta=abc-freedo-beta
write_stub "$work/stable-bin" 0.6.0 8
write_stub "$work/beta-bin" 0.7.0-beta.1 9
write_stub "$work/slow-bin" 0.6.0 8 slow
share="$work/share"; mkdir -p "$share"

# --- stable: a leftover channel option starts, the old cache goes -------------
sd="$work/stable-data"; mkdir -p "$sd/channel/0.5.0-beta.1"
printf 'schema 8\n' > "$sd/freedo-state.db"
printf 'wal\n' > "$sd/freedo-state.db-wal"
printf '{"api_token":"tok","transmit":false,"channel":"beta"}\n' > "$sd/options.json"
out=$(run_case "$sd" "$share" "$work/stable-bin" "$stable") || true
if contains "running freedo 0.6.0 (stable add-on)" "$out" && contains "RAN 0.6.0" "$out"; then
  ok "stable starts with a leftover channel option"
else no "stable starts with a leftover channel option: $out"; fi
if [ ! -d "$sd/channel" ]; then ok "stable removes the old channel cache"; else no "stable removes the old channel cache"; fi

# --- stable: the export holds the database, the -wal file and the manifest ---
if grep -q "ran 0.6.0" "$share/freedo/stable/freedo-state.db" && [ -f "$share/freedo/stable/freedo-state.db-wal" ] \
   && [ "$(jq -r '.version + " " + .schema' "$share/freedo/stable/export.json")" = "0.6.0 8" ]; then
  ok "stable exports the state after the daemon stops"
else no "stable exports the state after the daemon stops: $(ls -la "$share/freedo/stable" 2>&1)"; fi
modes=$(stat -c '%a' "$share/freedo" "$share/freedo/stable/freedo-state.db" | tr '\n' ' ')
if [ "$modes" = "700 600 " ]; then ok "the export is private"; else no "the export is private: got '$modes'"; fi
if [ "$(jq -r '.host + " " + (.stopped_at | type)' "$share/freedo/owner.json")" = "$stable number" ]; then
  ok "stable records its stop"
else no "stable records its stop: $(cat "$share/freedo/owner.json" 2>&1)"; fi

# --- stable: SIGTERM reaches the daemon and the export follows its exit ------
printf '{"api_token":"tok","transmit":false}\n' > "$sd/options.json"
cid=$(run_case "$sd" "$share" "$work/slow-bin" "$stable" -d --init | tail -1)
sleep 2
before=$(grep -c stopped "$share/freedo/stable/freedo-state.db" || true)
docker stop -t 20 "$cid" > /dev/null 2>&1 || true
if [ "$before" = 0 ] && grep -q stopped "$share/freedo/stable/freedo-state.db"; then
  ok "stable exports at start and again after SIGTERM"
else no "stable exports at start and again after SIGTERM: before=$before"; fi

# --- beta: the first start imports the export ---------------------------------
bd="$work/beta-data"; mkdir -p "$bd"
printf '{"api_token":"tok","transmit":false,"import_stable_state":false}\n' > "$bd/options.json"
out=$(run_case "$bd" "$share" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) || true
if contains "imported the state of the stable add-on: freedo 0.6.0, schema 8" "$out" \
   && contains "running freedo 0.7.0-beta.1 (beta add-on)" "$out" && grep -q stopped "$bd/freedo-state.db"; then
  ok "beta imports the export at its first start"
else no "beta imports the export at its first start: $out"; fi
if ! grep -q "ran 0.7.0-beta.1" "$share/freedo/stable/freedo-state.db"; then
  ok "beta leaves the export alone"
else no "beta leaves the export alone"; fi

# --- beta: the second start keeps its own state --------------------------------
out=$(run_case "$bd" "$share" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) || true
if ! contains "imported" "$out" && [ "$(grep -c "ran 0.7.0-beta.1" "$bd/freedo-state.db")" = 2 ]; then
  ok "beta does not import at the second start"
else no "beta does not import at the second start: $out"; fi

# --- beta: the option does not import the same export twice --------------------
printf '{"api_token":"tok","transmit":false,"import_stable_state":true}\n' > "$bd/options.json"
out=$(run_case "$bd" "$share" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) || true
if contains "is already imported" "$out" && [ "$(grep -c "ran 0.7.0-beta.1" "$bd/freedo-state.db")" = 3 ] \
   && [ ! -d "$bd/backup" ]; then
  ok "beta with the option on skips an export it already imported"
else no "beta with the option on skips an export it already imported: $out"; fi

# --- beta: the option imports a new export and keeps the replaced state ------
out=$(run_case "$sd" "$share" "$work/stable-bin" "$stable") || true
sleep 1
out=$(run_case "$bd" "$share" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) || true
kept=$(ls -d "$bd"/backup/before-import-* 2>/dev/null | head -1)
if contains "imported the state of the stable add-on" "$out" && contains "before this import is in" "$out" \
   && [ -n "$kept" ] && [ "$(grep -c "ran 0.7.0-beta.1" "$kept/freedo-state.db")" = 3 ] \
   && [ "$(grep -c "ran 0.7.0-beta.1" "$bd/freedo-state.db")" = 1 ]; then
  ok "beta with the option on imports a new export and keeps a copy"
else no "beta with the option on imports a new export and keeps a copy: $out"; fi

# --- beta: no export, an empty installation starts ----------------------------
d="$work/empty/data"; mkdir -p "$d" "$work/empty/share"
printf '{"api_token":"tok","transmit":false,"import_stable_state":false}\n' > "$d/options.json"
out=$(run_case "$d" "$work/empty/share" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) || true
if contains "starting with an empty installation" "$out" && contains "RAN 0.7.0-beta.1" "$out"; then
  ok "beta with no export starts empty"
else no "beta with no export starts empty: $out"; fi

# --- beta: an export of a newer schema stops the first start -----------------
d="$work/newer/data"; s="$work/newer/share"; mkdir -p "$d" "$s/freedo/stable"
printf 'schema 12\n' > "$s/freedo/stable/freedo-state.db"
printf '{"version":"0.9.0","schema":"12","exported_at":"2026-01-01T00:00:00Z"}\n' > "$s/freedo/stable/export.json"
printf '{"api_token":"tok","transmit":false,"import_stable_state":false}\n' > "$d/options.json"
if out=$(run_case "$d" "$s" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) && contains "RAN" "$out"; then
  no "beta stops when it cannot read the export: $out"
elif contains "ERROR" "$out" && [ ! -f "$d/freedo-state.db" ]; then
  ok "beta stops when it cannot read the export"
else no "beta stops when it cannot read the export: $out"; fi

# --- guard: the other add-on answers, the start stops -------------------------
d="$work/guard/data"; s="$work/guard/share"; mkdir -p "$d" "$s/freedo"
printf '{"host":"127.0.0.1","stopped_at":null}\n' > "$s/freedo/owner.json"
printf '{"api_token":"tok","transmit":true}\n' > "$d/options.json"
if out=$(docker run --rm --user "$(id -u):$(id -g)" --hostname "$stable" \
    -v "$d:/data:Z" -v "$s:/share:Z" -v "$work/run.sh:/run.sh:ro,Z" \
    -v "$work/stable-bin:/usr/local/bin/freedo:ro,Z" -e SUPERVISOR_TOKEN=stub "$image" \
    sh -c 'while :; do printf "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n" | nc -l -p 18080 > /dev/null; done & sleep 1; exec /run.sh' 2>&1) \
   && contains "RAN" "$out"; then
  no "the start stops while the other add-on runs: $out"
elif contains "is running: stop it" "$out" \
   && [ "$(jq -r '.host' "$s/freedo/owner.json")" = "127.0.0.1" ]; then
  ok "the start stops while the other add-on runs"
else no "the start stops while the other add-on runs: $out"; fi

# --- guard: the other add-on stopped a moment ago, a transmitting start waits -
printf '{"host":"127.0.0.1","stopped_at":%s}\n' "$(date +%s)" > "$s/freedo/owner.json"
t0=$(date +%s)
out=$(run_case "$d" "$s" "$work/stable-bin" "$stable" -e FREEDO_MID_WAIT=4) || true
t1=$(date +%s)
if contains "waiting" "$out" && contains "RAN 0.6.0" "$out" && [ $((t1 - t0)) -ge 3 ]; then
  ok "a transmitting start waits after the other add-on stopped"
else no "a transmitting start waits after the other add-on stopped ($((t1 - t0)) s): $out"; fi

# --- guard: a dry-run start and a restart of the same add-on do not wait -----
printf '{"host":"127.0.0.1","stopped_at":%s}\n' "$(date +%s)" > "$s/freedo/owner.json"
printf '{"api_token":"tok","transmit":false}\n' > "$d/options.json"
out=$(run_case "$d" "$s" "$work/stable-bin" "$stable") || true
printf '{"api_token":"tok","transmit":true}\n' > "$d/options.json"
out2=$(run_case "$d" "$s" "$work/stable-bin" "$stable") || true
if ! contains "waiting" "$out" && contains "RAN 0.6.0" "$out" && ! contains "waiting" "$out2" && contains "RAN 0.6.0" "$out2"; then
  ok "a dry-run start and a restart of the same add-on do not wait"
else no "a dry-run start and a restart of the same add-on do not wait: $out $out2"; fi

# --- stable: a database the daemon cannot check still starts ------------------
d="$work/broken/data"; mkdir -p "$d" "$work/broken/share"
printf 'broken\n' > "$d/freedo-state.db"
printf '{"api_token":"tok","transmit":false}\n' > "$d/options.json"
out=$(run_case "$d" "$work/broken/share" "$work/stable-bin" "$stable") || true
if contains "was not exported" "$out" && contains "RAN 0.6.0" "$out" && [ ! -d "$work/broken/share/freedo/stable" ]; then
  ok "stable starts and exports nothing when the check fails"
else no "stable starts and exports nothing when the check fails: $out"; fi

echo
if [ "$fails" = 0 ]; then echo "all run.sh tests passed"; else echo "$fails run.sh test(s) failed"; exit 1; fi
