#!/bin/sh
set -eu
umask 077

options=/data/options.json
api=http://supervisor

token=$(jq -r '.api_token // empty' "$options")
if [ -z "$token" ]; then
  [ -s /data/api_token ] || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /data/api_token
  token=$(cat /data/api_token)
fi
export FREEDO_API_TOKEN="$token"

set -- --addr :18080 --state /data/freedo-state.db --unhandled /data/freedo-unhandled.log

port=$(jq -r '.serial_port // empty' "$options")
if [ -n "$port" ]; then
  set -- "$@" --port "$port"
else
  echo "no serial port selected: the daemon runs without a radio"
fi

if [ "$(jq -r '.transmit' "$options")" = true ]; then
  set -- "$@" --tx
fi

# Tell Home Assistant where the API is, so the integration needs no address.
announce() {
  host=$(curl -fsS -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" "$api/addons/self/info" | jq -er '.data.hostname') &&
    jq -n --arg host "$host" --arg token "$token" \
      '{service: "freedo", config: {host: $host, port: 18080, token: $token}}' |
    curl -fsS -X POST -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" -H "Content-Type: application/json" \
      -d @- "$api/discovery" > /dev/null
}
announce || echo "warning: the discovery message was not sent: add the integration by hand"

exec freedo "$@"
