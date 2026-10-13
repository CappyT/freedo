#!/bin/sh
# One script serves both add-ons. The beta add-on sets FREEDO_BETA in its
# config.yaml.
set -eu
umask 077

options=/data/options.json
api=http://supervisor
data=/data
bin=/usr/local/bin/freedo
# The folder both add-ons reach. It holds the record of the add-on that runs.
shared=/share/freedo
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

version=$("$bin" --version | awk '{print $2}')
# A fault of the shared folder must not keep the heating off: the start goes on.
mkdir -p "$shared" && chmod 700 "$shared" || echo "warning: the folder $shared is not usable"

guard_owner
set_owner || echo "warning: $owner was not written"

if [ -n "${FREEDO_BETA:-}" ]; then
  name="beta add-on"
else
  name="stable add-on"
  # Releases that had a beta channel inside this add-on cached daemon binaries here.
  rm -rf "$data/channel"
fi
# Releases before the export and import of the dashboard copied the database
# here. The copy is a file of the user: the script names it and leaves it.
if [ -e "$shared/stable" ]; then
  echo "freedo: $shared/stable is a copy of the state from an older release. It holds the network key and nothing reads it: delete it"
fi

announce || echo "warning: the discovery message was not sent: add the integration by hand"

echo "freedo: running freedo $version ($name)"
# After an import of the state the daemon replaces itself and keeps this
# process id, so the wait below also covers that restart.
"$bin" "$@" &
pid=$!
trap 'kill -TERM "$pid" 2> /dev/null' TERM INT
status=0
# wait returns when the trap runs: wait again until the daemon is gone.
while kill -0 "$pid" 2> /dev/null; do
  status=0
  wait "$pid" || status=$?
done

set_owner "$(date +%s)" || echo "warning: $owner was not written"
exit "$status"
