#!/bin/sh
set -eu
umask 077

options=/data/options.json
api=http://supervisor
data=/data
state="$data/freedo-state.db"
channel_dir="$data/channel"
backup_dir="$data/backup"
bundled=/usr/local/bin/freedo
# The releases API the beta channel reads. A test points this at a stub.
releases_url=${FREEDO_RELEASES_URL:-https://api.github.com/repos/CappyT/freedo/releases?per_page=20}

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

# The channel: stable runs the daemon of the image, beta runs the newest
# pre-release from the GitHub releases of the repository, with a fallback to the
# cache and then to the image.

# version_cmp prints -1, 0 or 1 when $1 is older, equal or newer than $2. The
# project uses X.Y.Z and X.Y.Z-beta.N, so a stable release is newer than its own
# pre-releases.
version_cmp() {
  awk -v a="$1" -v b="$2" '
  BEGIN {
    na = split(a, am, "-"); nb = split(b, bm, "-")
    ca = split(am[1], av, "."); cb = split(bm[1], bv, ".")
    for (i = 1; i <= 3; i++) {
      x = (i <= ca) ? av[i] + 0 : 0
      y = (i <= cb) ? bv[i] + 0 : 0
      if (x > y) { print 1; exit }
      if (x < y) { print -1; exit }
    }
    if (na == 1 && nb == 1) { print 0; exit }
    if (na == 1) { print 1; exit }
    if (nb == 1) { print -1; exit }
    pa = split(am[2], ap, "."); pb = split(bm[2], bp, ".")
    for (i = 1; i <= (pa > pb ? pa : pb); i++) {
      x = (i <= pa) ? ap[i] + 0 : 0
      y = (i <= pb) ? bp[i] + 0 : 0
      if (x > y) { print 1; exit }
      if (x < y) { print -1; exit }
    }
    print 0
  }'
}

version_gt() { [ "$(version_cmp "$1" "$2")" = 1 ]; }

# arch is the release asset suffix of this platform. Empty means no release
# binary runs here.
case "$(uname -m)" in
  aarch64|arm64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) arch= ;;
esac

# keep_channel removes every cached version but the two newest. Each removal
# names one directory.
keep_channel() {
  while :; do
    count=0
    for d in "$channel_dir"/*/; do
      [ -e "$d" ] || continue
      count=$((count + 1))
    done
    [ "$count" -le 2 ] && return 0
    oldest="" oldest_v=""
    for d in "$channel_dir"/*/; do
      [ -e "$d" ] || continue
      v=${d#"$channel_dir"/}; v=${v%/}
      if [ -z "$oldest" ] || version_gt "$oldest_v" "$v"; then
        oldest=$d oldest_v=$v
      fi
    done
    rm -rf "$oldest"
  done
}

# keep_backups keeps the three newest before-* copies and removes the rest.
keep_backups() {
  while :; do
    count=0
    for d in "$backup_dir"/before-*/; do
      [ -e "$d" ] || continue
      count=$((count + 1))
    done
    [ "$count" -le 3 ] && return 0
    oldest="" oldest_v=""
    for d in "$backup_dir"/before-*/; do
      [ -e "$d" ] || continue
      v=${d#"$backup_dir/before-"}; v=${v%/}
      if [ -z "$oldest" ] || version_gt "$oldest_v" "$v"; then
        oldest=$d oldest_v=$v
      fi
    done
    rm -rf "$oldest"
  done
}

# download_beta installs version from the two asset URLs under
# $channel_dir/<version>/ and returns non-zero when the download, the checksum
# or the binary check fails.
download_beta() {
  version=$1 binurl=$2 sumsurl=$3
  staging="$channel_dir/.staging.$$"
  rm -rf "$staging"
  mkdir -p "$staging"
  if ! curl -fsS --max-time 60 -o "$staging/freedo-linux-$arch" "$binurl"; then
    rm -rf "$staging"; return 1
  fi
  if ! curl -fsS --max-time 60 -o "$staging/SHA256SUMS" "$sumsurl"; then
    rm -rf "$staging"; return 1
  fi
  want=$(grep " freedo-linux-$arch\$" "$staging/SHA256SUMS" | awk '{print $1}')
  if [ -z "$want" ]; then
    rm -rf "$staging"; return 1
  fi
  got=$(sha256sum "$staging/freedo-linux-$arch" | awk '{print $1}')
  if [ "$want" != "$got" ]; then
    rm -rf "$staging"; return 1
  fi
  chmod +x "$staging/freedo-linux-$arch"
  if ! "$staging/freedo-linux-$arch" --version > /dev/null 2>&1; then
    rm -rf "$staging"; return 1
  fi
  rm -rf "$channel_dir/$version"
  mv "$staging" "$channel_dir/$version"
}

# newest_cached prints the newest cached beta version newer than $1, or nothing.
newest_cached() {
  best=""
  for d in "$channel_dir"/*/; do
    [ -e "$d" ] || continue
    v=${d#"$channel_dir"/}; v=${v%/}
    [ -e "$d/freedo-linux-$arch" ] || continue
    version_gt "$v" "$1" || continue
    if [ -z "$best" ] || version_gt "$v" "$best"; then best=$v; fi
  done
  [ -n "$best" ] && printf '%s' "$best"
}

# backup_before copies the state database into /data/backup/before-<version>/
# before the first start of a beta version. The daemon is not running yet.
backup_before() {
  dest="$backup_dir/before-$1"
  [ -d "$dest" ] && return 0
  mkdir -p "$dest"
  for f in "$state" "$state-wal" "$state-shm"; do
    [ -f "$f" ] || continue
    cp "$f" "$dest/"
  done
  keep_backups
}

# restore_backup moves the beta-written database aside and puts the newest
# before-* copy back. It stops the start when no copy exists.
restore_backup() {
  best="" best_v=""
  for d in "$backup_dir"/before-*/; do
    [ -e "$d" ] || continue
    v=${d#"$backup_dir/before-"}; v=${v%/}
    if [ -z "$best" ] || version_gt "$v" "$best_v"; then
      best=$d best_v=$v
    fi
  done
  if [ -z "$best" ]; then
    echo "ERROR: the state database is newer than the daemon and no /data/backup/before-* copy exists: stopping"
    exit 1
  fi
  aside="$backup_dir/beta-$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$aside"
  for f in "$state" "$state-wal" "$state-shm"; do
    [ -f "$f" ] || continue
    mv "$f" "$aside/"
  done
  echo "WARNING: the data written under the beta is set aside in $aside: restore it only with the same or a newer daemon"
  for f in freedo-state.db freedo-state.db-wal freedo-state.db-shm; do
    [ -f "$best/$f" ] || continue
    cp "$best/$f" "$data/$f"
  done
}

# ensure_state_readable runs the chosen binary against the state database and
# restores a backup when the database holds a newer schema. It stops the start
# when the database stays unreadable.
ensure_state_readable() {
  status=0
  "$bin" --check-state "$state" || status=$?
  if [ "$status" = 0 ]; then
    return 0
  fi
  if [ "$status" != 3 ]; then
    echo "ERROR: $bin refused the state database $state (status $status): stopping"
    exit 1
  fi
  echo "WARNING: the state database holds a schema newer than $bin reads: restoring a backup"
  restore_backup
  status=0
  "$bin" --check-state "$state" || status=$?
  if [ "$status" != 0 ]; then
    echo "ERROR: the state database is still unreadable after the restore (status $status): stopping"
    exit 1
  fi
}

bin=$bundled
run_msg="the bundled daemon (channel stable)"

if [ "$(jq -r '.channel // "stable"' "$options")" = beta ]; then
  if [ -z "$arch" ]; then
    run_msg="the bundled daemon (channel beta, no release binary for $(uname -m))"
  else
    bundled_version=$("$bundled" --version | awk '{print $2}')
    json=$(curl -fsS --max-time 15 "$releases_url" 2> /dev/null) || json=
    best="" best_bin="" best_sums=""
    if [ -n "$json" ]; then
      candidates=$(printf '%s' "$json" | jq -r --arg arch "$arch" '
        .[] | select(.prerelease)
        | select(.tag_name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+-beta\\.[0-9]+$"))
        | .tag_name as $tag
        | [ $tag[1:],
            ([.assets[]? | select(.name == ("freedo-linux-" + $arch)) | .browser_download_url][0] // ""),
            ([.assets[]? | select(.name == "SHA256SUMS") | .browser_download_url][0] // "")
          ] | @tsv') || candidates=
      tab=$(printf '\t')
      while IFS="$tab" read -r v binurl sumsurl; do
        [ -n "$v" ] || continue
        [ -n "$binurl" ] || continue
        [ -n "$sumsurl" ] || continue
        version_gt "$v" "$bundled_version" || continue
        if [ -z "$best" ] || version_gt "$v" "$best"; then
          best=$v best_bin=$binurl best_sums=$sumsurl
        fi
      done <<EOF
$candidates
EOF
    fi
    if [ -n "$best" ]; then
      if download_beta "$best" "$best_bin" "$best_sums"; then
        keep_channel
        bin="$channel_dir/$best/freedo-linux-$arch"
        beta_version=$best
        run_msg="beta $best downloaded from the GitHub releases of the repository"
      fi
    fi
    if [ "$bin" = "$bundled" ]; then
      cached=$(newest_cached "$bundled_version") || cached=
      if [ -n "$cached" ]; then
        bin="$channel_dir/$cached/freedo-linux-$arch"
        beta_version=$cached
        run_msg="beta $cached from the channel cache"
      else
        run_msg="the bundled daemon (channel beta, no pre-release newer than $bundled_version)"
      fi
    fi
  fi
fi

if [ "$bin" != "$bundled" ]; then
  backup_before "$beta_version"
else
  ensure_state_readable
fi

echo "freedo: running $run_msg"
exec "$bin" "$@"
