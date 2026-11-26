#!/usr/bin/env bash
# Atomic Schema v1 usage snapshot deployment helper (docs/23-usage-snapshot.md §7).
#
# Usage:
#   atomic_deploy.sh <source-file-or-dash> <dest-path> [quota-fleet-bin]
#
# Reads a candidate Schema v1 JSON snapshot, acquires an exclusive flock in the
# destination directory, validates it with `--format v1 --check-deploy --reject-older-than`,
# skips replacement when SHA-256 is identical, and atomically replaces <dest-path> via mv.

set -euo pipefail

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "Usage: $0 <source-file|-> <dest-path> [quota-fleet-bin]" >&2
  exit 2
fi

SRC="$1"
DEST="$2"
QF_BIN="${3:-quota-fleet}"

case "$DEST" in
  ""|*/)
    echo "atomic_deploy: destination must be a file path, not a directory: $DEST" >&2
    exit 1
    ;;
esac
if [ -d "$DEST" ]; then
  echo "atomic_deploy: destination is a directory: $DEST" >&2
  exit 1
fi

DEST_DIR="$(dirname "$DEST")"
DEST_BASE="$(basename "$DEST")"
mkdir -p "$DEST_DIR"

LOCK="${DEST_DIR}/.${DEST_BASE}.lock"
TMP="$(mktemp "${DEST_DIR}/.${DEST_BASE}.XXXXXX")"
cleanup() {
  rm -f "$TMP"
}
trap cleanup EXIT

if [ "$SRC" = "-" ]; then
  cat > "$TMP"
else
  cp -- "$SRC" "$TMP"
fi
chmod 0644 "$TMP"

exec 9>"$LOCK"
flock -x 9

if [ -d "$DEST" ]; then
  echo "atomic_deploy: destination is a directory: $DEST" >&2
  exit 1
fi

"$QF_BIN" usage validate \
  --file "$TMP" \
  --format v1 \
  --check-deploy \
  --reject-older-than "$DEST"

if [ -f "$DEST" ] && cmp -s "$TMP" "$DEST"; then
  exit 0
fi

mv -fT "$TMP" "$DEST"
trap - EXIT
