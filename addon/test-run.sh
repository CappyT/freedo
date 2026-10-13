#!/bin/sh
# Exercises addon/run.sh in an alpine:3.24 container, as the stable and as the
# beta add-on: with stub daemon binaries for the start and the guard, and with
# a daemon built from this tree for the import of a state. The Supervisor is
# absent, so the discovery call fails with a warning. No network is needed.
# Requires docker and go (GO names the go command). Run it from the repository
# root:
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

# --- stable: nothing is copied to the share folder ----------------------------
if [ ! -e "$share/freedo/stable" ] && grep -q "ran 0.6.0" "$sd/freedo-state.db"; then
  ok "stable writes no copy of the state to the share folder"
else no "stable writes no copy of the state to the share folder: $(ls -la "$share/freedo" 2>&1)"; fi
if [ "$(stat -c '%a' "$share/freedo")" = 700 ]; then ok "the shared folder is private"; else no "the shared folder is private"; fi
if [ "$(jq -r '.host + " " + (.stopped_at | type)' "$share/freedo/owner.json")" = "$stable number" ]; then
  ok "stable records its stop"
else no "stable records its stop: $(cat "$share/freedo/owner.json" 2>&1)"; fi

# --- stable: SIGTERM reaches the daemon and the stop is recorded --------------
printf '{"api_token":"tok","transmit":false}\n' > "$sd/options.json"
cid=$(run_case "$sd" "$share" "$work/slow-bin" "$stable" -d --init | tail -1)
sleep 2
running=$(jq -r '.stopped_at | type' "$share/freedo/owner.json")
docker stop -t 20 "$cid" > /dev/null 2>&1 || true
if [ "$running" = null ] && grep -q stopped "$sd/freedo-state.db" \
   && [ "$(jq -r '.stopped_at | type' "$share/freedo/owner.json")" = number ] && [ ! -e "$share/freedo/stable" ]; then
  ok "SIGTERM reaches the daemon and the stop is recorded"
else no "SIGTERM reaches the daemon and the stop is recorded: running=$running"; fi

# --- a copy of an older release stays, and the log names it -------------------
mkdir -p "$share/freedo/stable"
printf 'schema 8\nold copy\n' > "$share/freedo/stable/freedo-state.db"
printf '{"version":"0.7.0","schema":"8","exported_at":"2026-01-01T00:00:00Z"}\n' > "$share/freedo/stable/export.json"
out=$(run_case "$sd" "$share" "$work/stable-bin" "$stable") || true
if contains "/share/freedo/stable is a copy of the state from an older release" "$out" && contains "RAN 0.6.0" "$out" \
   && [ "$(cat "$share/freedo/stable/freedo-state.db")" = "$(printf 'schema 8\nold copy')" ]; then
  ok "stable leaves the copy of an older release and names it"
else no "stable leaves the copy of an older release and names it: $out"; fi

# --- beta: no import at the first start, a leftover option is ignored ---------
bd="$work/beta-data"; mkdir -p "$bd"
printf '{"api_token":"tok","transmit":false,"import_stable_state":true}\n' > "$bd/options.json"
out=$(run_case "$bd" "$share" "$work/beta-bin" "$beta" -e FREEDO_BETA=1) || true
if contains "running freedo 0.7.0-beta.1 (beta add-on)" "$out" && ! contains "imported" "$out" \
   && contains "/share/freedo/stable is a copy" "$out" \
   && [ "$(cat "$bd/freedo-state.db")" = "ran 0.7.0-beta.1" ] && [ ! -d "$bd/backup" ] \
   && [ -f "$share/freedo/stable/export.json" ]; then
  ok "beta starts on its own state and imports nothing"
else no "beta starts on its own state and imports nothing: $out"; fi

# --- the exit status of the daemon is the exit status of the script -----------
write_stub "$work/fail-bin" 0.6.0 8
sed -i 's/^exit 0$/exit 7/' "$work/fail-bin"
d="$work/fail/data"; mkdir -p "$d" "$work/fail/share"
printf '{"api_token":"tok","transmit":false}\n' > "$d/options.json"
status=0
out=$(run_case "$d" "$work/fail/share" "$work/fail-bin" "$stable") || status=$?
if [ "$status" = 7 ]; then ok "the script exits with the status of the daemon"; else no "the script exits with the status of the daemon: $status $out"; fi

# --- the real daemon: export, import, restart and backup rotation -------------
# The daemon replaces itself after an import. These cases prove that under
# run.sh, with a daemon built from this tree, as the stable and as the beta
# add-on.
home_of() { docker exec "$1" curl -s -H "Authorization: Bearer tok" http://127.0.0.1:18080/api/install 2> /dev/null | jq -r '.home_id // empty' 2> /dev/null; }
# wait_home waits until the daemon in container $1 serves an installation, and
# with $2 until its home id is $2. It prints the home id.
wait_home() {
  n=0
  while [ "$n" -lt 100 ]; do
    h=$(home_of "$1") || h=
    if [ -n "$h" ] && { [ -z "${2:-}" ] || [ "$h" = "$2" ]; }; then echo "$h"; return 0; fi
    n=$((n + 1)); sleep 0.2
  done
  return 1
}
real="$work/freedo-real"
if (cd "$repo" && CGO_ENABLED=0 "${GO:-go}" build -o "$real" ./cmd/freedo) 2> "$work/build.log"; then
  for flavour in stable beta; do
    if [ "$flavour" = beta ]; then hn=$beta; set -- -e FREEDO_BETA=1; else hn=$stable; set --; fi
    src="$work/real-$flavour/src"; dst="$work/real-$flavour/dst"; s="$work/real-$flavour/share"
    mkdir -p "$src" "$dst" "$s"
    printf '{"api_token":"tok","transmit":false}\n' > "$src/options.json"
    cp "$src/options.json" "$dst/options.json"

    cid=$(run_case "$src" "$s" "$real" "src-$hn" -d --init "$@" | tail -1)
    want=$(wait_home "$cid") || want=
    docker exec "$cid" curl -s -H "Authorization: Bearer tok" -o /data/export.db http://127.0.0.1:18080/api/state/export
    docker stop -t 20 "$cid" > /dev/null 2>&1 || true
    if [ -n "$want" ] && [ "$(head -c 15 "$src/export.db")" = "SQLite format 3" ]; then
      ok "$flavour: the daemon exports its state"
    else no "$flavour: the daemon exports its state: home=$want"; fi
    cp "$src/export.db" "$dst/export.db"

    cid=$(docker run -d --init --user "$(id -u):$(id -g)" --hostname "$hn" \
      -v "$dst:/data:Z" -v "$s:/share:Z" -v "$work/run.sh:/run.sh:ro,Z" \
      -v "$real:/usr/local/bin/freedo:ro,Z" -e SUPERVISOR_TOKEN=stub "$@" "$image" /run.sh)
    own=$(wait_home "$cid") || own=
    refused=$(docker exec "$cid" curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer tok" \
      -X POST --data-binary @/data/export.db http://127.0.0.1:18080/api/state/import)
    if [ -n "$own" ] && [ "$own" != "$want" ] && [ "$refused" = 400 ] && [ ! -e "$dst/freedo-state.db.import" ]; then
      ok "$flavour: an import with no confirm changes nothing"
    else no "$flavour: an import with no confirm changes nothing: own=$own status=$refused"; fi

    i=1
    while [ "$i" -le 4 ]; do
      docker exec "$cid" curl -s -o /dev/null -H "Authorization: Bearer tok" \
        -X POST --data-binary @/data/export.db 'http://127.0.0.1:18080/api/state/import?confirm=true'
      n=0
      while [ "$(docker logs "$cid" 2>&1 | grep -c 'starts on the imported state')" -lt "$i" ] && [ "$n" -lt 100 ]; do
        n=$((n + 1)); sleep 0.2
      done
      wait_home "$cid" "$want" > /dev/null || true
      i=$((i + 1))
    done
    got=$(home_of "$cid") || got=
    log=$(docker logs "$cid" 2>&1)
    if [ "$got" = "$want" ] && [ "$(echo "$log" | grep -c 'starts on the imported state')" = 4 ] \
       && [ "$(echo "$log" | grep -c 'freedo: running freedo')" = 1 ]; then
      ok "$flavour: the daemon restarts on the imported state under one run of the script"
    else no "$flavour: the daemon restarts on the imported state under one run of the script: home=$got want=$want $log"; fi
    if [ "$(ls -1d "$dst"/backup/before-import-*/ 2> /dev/null | wc -l)" = 3 ] \
       && [ "$(ls -1 "$dst"/backup/before-import-*/freedo-state.db 2> /dev/null | wc -l)" = 3 ] \
       && [ ! -e "$dst/freedo-state.db.import" ]; then
      ok "$flavour: the three newest replaced states stay in the backup folder"
    else no "$flavour: the three newest replaced states stay in the backup folder: $(ls -la "$dst/backup" 2>&1)"; fi
    docker stop -t 20 "$cid" > /dev/null 2>&1 || true
    status=$(docker inspect -f '{{.State.ExitCode}}' "$cid" 2> /dev/null) || status=
    docker rm "$cid" > /dev/null 2>&1 || true
    if [ "$status" = 0 ] && [ "$(jq -r '.host + " " + (.stopped_at | type)' "$s/freedo/owner.json")" = "$hn number" ]; then
      ok "$flavour: SIGTERM stops the restarted daemon and the stop is recorded"
    else no "$flavour: SIGTERM stops the restarted daemon and the stop is recorded: status=$status"; fi
  done
else
  no "build the daemon for the import cases (set GO to the go command): $(cat "$work/build.log")"
fi

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

echo
if [ "$fails" = 0 ]; then echo "all run.sh tests passed"; else echo "$fails run.sh test(s) failed"; exit 1; fi
