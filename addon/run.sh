#!/bin/sh
# One script serves both add-ons. The beta add-on sets FREEDO_BETA in its
# config.yaml.
set -eu
umask 077

options=/data/options.json
api=http://supervisor
data=/data
state="$data/freedo-state.db"
backup_dir="$data/backup"
bin=/usr/local/bin/freedo
# The folder both add-ons reach. The stable add-on exports its state there and
# the beta add-on imports it.
shared=/share/freedo
export_dir="$shared/stable"
owner="$shared/owner.json"
me=$(hostname)
# A device keeps the CoAP Message IDs it saw for EXCHANGE_LIFETIME (247 s). The
# two add-ons resume the Message ID from their own databases, so the second one
# could repeat a Message ID of the first inside that time. A test shortens this.
mid_wait=${FREEDO_MID_WAIT:-250}

token=$(jq -r '.api_token // empty' "$options")
if [ -z "$token" ]; then
  [ -s /data/api_token ] || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /data/api_token
  token=$(cat /data/api_token)
fi
export FREEDO_API_TOKEN="$token"

# --ingress lets the Home Assistant gateway (172.30.32.2) reach the API without the token.
# Only this script sets it: Home Assistant has already authenticated the user.
set -- --addr :18080 --ingress --state /data/freedo-state.db --unhandled /data/freedo-unhandled.log

port=$(jq -r '.serial_port // empty' "$options")
if [ -n "$port" ]; then
  set -- "$@" --port "$port"
else
  echo "no serial port selected: the daemon runs without a radio"
fi

transmit=$(jq -r '.transmit' "$options")
if [ "$transmit" = true ]; then
  set -- "$@" --tx
fi

# Tell Home Assistant where the API is, so the integration needs no address.
# The Supervisor pushes a message only when its config changed, and the stable
# and the beta add-on take turns: the start time makes every start a change, so
# the integration follows the add-on that started last.
announce() {
  host=$(curl -fsS -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" "$api/addons/self/info" | jq -er '.data.hostname') &&
    jq -n --arg host "$host" --arg token "$token" --arg started "$(date +%s)" \
      '{service: "freedo", config: {host: $host, port: 18080, token: $token, started: $started}}' |
    curl -fsS -X POST -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" -H "Content-Type: application/json" \
      -d @- "$api/discovery" > /dev/null
}

# refuse_running stops the start when the add-on $other answers on its API port.
refuse_running() {
  if curl -s -o /dev/null --max-time 3 "http://$other:18080/"; then
    echo "ERROR: the add-on $other is running: stop it before you start this one. One coordinator serves a radio network"
    exit 1
  fi
}

# guard_owner stops the start while the other add-on answers on its API port:
# one coordinator per radio network. It then waits until the Message IDs of the
# other add-on left the devices.
guard_owner() {
  other=$(jq -r '.host // empty' "$owner" 2> /dev/null) || other=
  if [ -z "$other" ] || [ "$other" = "$me" ]; then
    return 0
  fi
  refuse_running
  [ "$transmit" = true ] || return 0
  # No stop time means the other add-on did not stop in order: wait the full time.
  stopped=$(jq -r '.stopped_at | numbers' "$owner" 2> /dev/null) || stopped=
  left=$mid_wait
  if [ -n "$stopped" ]; then
    left=$((stopped + mid_wait - $(date +%s)))
    if [ "$left" -gt "$mid_wait" ]; then left=$mid_wait; fi
  fi
  if [ "$left" -gt 0 ]; then
    echo "freedo: the add-on $other stopped less than $mid_wait s ago: waiting $left s before the start, so that no device still holds one of its Message IDs"
    sleep "$left"
    # The other add-on can start inside the wait, for example when both start at boot.
    refuse_running
  fi
}

# set_owner records which add-on runs, or when it stopped ($1, unix seconds).
set_owner() {
  jq -n --arg host "$me" --arg at "${1:-}" \
    '{host: $host, stopped_at: (if $at == "" then null else ($at | tonumber) end)}' > "$owner"
}

# export_state copies the database of the stable add-on to $export_dir. No
# process has the database open, so the file and its -wal file are a
# consistent copy. The copy replaces the last one in one rename.
export_state() {
  [ -f "$state" ] || return 0
  line=$("$bin" --check-state "$state") || return 1
  schema=$(printf '%s\n' "$line" | sed -n 's/.*schema version \([0-9][0-9]*\) is readable.*/\1/p')
  new="$shared/.stable.new" old="$shared/.stable.old"
  rm -rf "$new" "$old" || return 1
  mkdir "$new" || return 1
  for f in "$state" "$state-wal"; do
    [ -f "$f" ] || continue
    cp "$f" "$new/" || return 1
  done
  jq -n --arg version "$version" --arg schema "$schema" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{version: $version, schema: $schema, exported_at: $at}' > "$new/export.json" || return 1
  if [ -d "$export_dir" ]; then
    mv "$export_dir" "$old" || return 1
  fi
  mv "$new" "$export_dir" || return 1
  rm -rf "$old"
}

# keep_backups keeps the three newest before-import-* copies. Each removal
# names one directory.
keep_backups() {
  ls -1d "$backup_dir"/before-import-*/ 2> /dev/null | sort -r | tail -n +4 | while read -r d; do
    rm -rf "$d"
  done
}

# import_state replaces the state of the beta add-on with the export of the
# stable add-on. $1 is "first" when the beta add-on has no database and "again"
# when the option import_stable_state asks for it.
import_state() {
  if [ ! -f "$export_dir/freedo-state.db" ] || [ ! -f "$export_dir/export.json" ]; then
    if [ "$1" = first ]; then
      echo "freedo: no export of the stable add-on in $export_dir: starting with an empty installation"
    else
      echo "WARNING: import_stable_state is on and $export_dir holds no export of the stable add-on: nothing imported"
    fi
    return 0
  fi
  stamp=$(jq -r '.exported_at' "$export_dir/export.json")
  if [ "$1" = again ] && [ "$stamp" = "$(cat "$data/imported" 2> /dev/null)" ]; then
    echo "freedo: the export of $stamp is already imported: turn import_stable_state off"
    return 0
  fi
  staging="$data/.import"
  rm -rf "$staging"
  mkdir "$staging"
  for f in freedo-state.db freedo-state.db-wal; do
    [ -f "$export_dir/$f" ] || continue
    cp "$export_dir/$f" "$staging/"
  done
  status=0
  "$bin" --check-state "$staging/freedo-state.db" > /dev/null || status=$?
  if [ "$status" != 0 ]; then
    rm -rf "$staging"
    echo "ERROR: this daemon does not read the export in $export_dir (status $status, 3 means a newer schema): nothing imported"
    # A beta add-on with no state must not start as a new, empty installation
    # when the user expects the plant of the stable add-on.
    if [ "$1" = first ]; then exit 1; fi
    return 0
  fi
  rm -f "$staging/freedo-state.db-shm"
  if [ -f "$state" ]; then
    keep="$backup_dir/before-import-$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir -p "$keep"
    for f in "$state" "$state-wal" "$state-shm"; do
      [ -f "$f" ] || continue
      mv "$f" "$keep/"
    done
    keep_backups
    echo "freedo: the state of the beta add-on before this import is in $keep"
  fi
  for f in freedo-state.db freedo-state.db-wal; do
    [ -f "$staging/$f" ] || continue
    mv "$staging/$f" "$data/$f"
  done
  rmdir "$staging"
  printf '%s' "$stamp" > "$data/imported"
  echo "freedo: imported the state of the stable add-on: freedo $(jq -r '.version' "$export_dir/export.json"), schema $(jq -r '.schema' "$export_dir/export.json"), exported at $stamp"
}

version=$("$bin" --version | awk '{print $2}')
# A fault of the shared folder must not keep the heating off: the start goes on.
mkdir -p "$shared" && chmod 700 "$shared" || echo "warning: the folder $shared is not usable"

guard_owner
set_owner || echo "warning: $owner was not written"

if [ -n "${FREEDO_BETA:-}" ]; then
  name="beta add-on"
  if [ ! -f "$state" ]; then
    import_state first
  elif [ "$(jq -r '.import_stable_state // false' "$options")" = true ]; then
    import_state again
  fi
else
  name="stable add-on"
  # Releases that had a beta channel inside this add-on cached daemon binaries here.
  rm -rf "$data/channel"
  # A start after a power loss has no export of the last stop: export now.
  export_state || echo "warning: the state was not exported to $export_dir"
fi

announce || echo "warning: the discovery message was not sent: add the integration by hand"

echo "freedo: running freedo $version ($name)"
"$bin" "$@" &
pid=$!
trap 'kill -TERM "$pid" 2> /dev/null' TERM INT
status=0
# wait returns when the trap runs: wait again until the daemon is gone.
while kill -0 "$pid" 2> /dev/null; do
  status=0
  wait "$pid" || status=$?
done

if [ -z "${FREEDO_BETA:-}" ]; then
  export_state || echo "warning: the state was not exported to $export_dir"
fi
set_owner "$(date +%s)" || echo "warning: $owner was not written"
exit "$status"
