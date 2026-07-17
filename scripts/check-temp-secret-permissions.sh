#!/usr/bin/env bash
set -euo pipefail

repo="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
failed=0

while IFS= read -r -d '' file; do
  mode="$(stat -c '%a' "$file")"
  permissions=$((8#$mode))
  if (( permissions & 0077 )); then
    printf 'unsafe temporary credential permissions: %s mode=%s (require 0600 or stricter)\n' "$file" "$mode" >&2
    failed=1
  fi
done < <(
  find "$repo/temp" "$repo/tmp" -maxdepth 4 -type f \
    \( -name '*.env' -o -name '.env' -o -iname '*credential*' -o -iname '*secret*' \) \
    -print0 2>/dev/null || true
)

exit "$failed"
