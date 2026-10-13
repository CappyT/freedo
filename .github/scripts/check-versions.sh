#!/bin/sh
# Checks the release version rules of CONTRIBUTING.md. With a tag as $1 it also
# checks that the tag names the version of the integration.
#   .github/scripts/check-versions.sh [vX.Y.Z | vX.Y.Z-beta.N]
set -eu

version_of() { sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$1"; }
fail() { echo "::error::$1"; exit 1; }

stable=$(version_of addon/config.yaml)
beta=$(version_of addon-beta/config.yaml)
integration=$(jq -r .version custom_components/freedo/manifest.json)

if [ -n "${1:-}" ] && [ "v$integration" != "$1" ]; then
  fail "tag '$1', manifest.json '$integration'"
fi
# The beta add-on carries every release, so a beta user never stays behind a stable release.
if [ "$beta" != "$integration" ]; then
  fail "addon-beta/config.yaml '$beta', manifest.json '$integration'"
fi
case "$integration" in
  *-*)
    # A pre-release leaves the stable add-on on the last stable version.
    case "$stable" in
      ""|*-*) fail "manifest.json '$integration', addon/config.yaml '$stable' must stay on a stable version" ;;
    esac
    ;;
  *)
    if [ "$stable" != "$integration" ]; then
      fail "addon/config.yaml '$stable', manifest.json '$integration'"
    fi
    ;;
esac

# The Supervisor reads each add-on folder on its own, so the beta add-on holds copies.
for f in icon.png logo.png; do
  cmp -s "addon/$f" "addon-beta/$f" || fail "addon/$f and addon-beta/$f differ"
done
echo "versions: stable add-on $stable, beta add-on $beta, integration $integration"
