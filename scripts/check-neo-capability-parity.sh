#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
manifest="$repo_root/spec/neo-capability-parity/protected-runtime.sha256"
ledger="$repo_root/spec/neo-capability-parity/parity.tsv"

cd "$repo_root"
sha256sum --check --quiet "$manifest"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

awk -F '\t' '
NR == 1 {
  if ($0 != "feature\tcow_source\tneo_owner\tstatus\tverification\toutcome") {
    print "invalid parity ledger header" > "/dev/stderr"
    exit 1
  }
  next
}
NF != 6 {
  print "parity ledger row " NR " has " NF " fields, want 6" > "/dev/stderr"
  exit 1
}
$1 == "" || $2 == "" || $3 == "" || $4 == "" || $5 == "" || $6 == "" {
  print "parity ledger row " NR " contains an empty field" > "/dev/stderr"
  exit 1
}
$4 !~ /^(superior|existing|planned|rejected)$/ {
  print "parity ledger row " NR " has invalid status " $4 > "/dev/stderr"
  exit 1
}
seen[$1]++ {
  print "duplicate parity feature " $1 > "/dev/stderr"
  exit 1
}
{ print $1 }
' "$ledger" | sort > "$tmp_dir/actual"

cat > "$tmp_dir/expected" <<'EOF'
backup_recovery
capability_hub
channel_gateway
cloud_cli
core_runtime
desktop_pwa
enterprise_channels
knowledge_graph
machine_mail
mcp_control
memory_context
model_routing
native_tools
plugins_policies
research_windows
scheduling
session_operator_ux
slack_discord
telegram_channel
verified_improvement
voice_media
web_channel
wechat_qq_channels
EOF

if ! diff -u "$tmp_dir/expected" "$tmp_dir/actual"; then
  echo "parity ledger capability set is incomplete" >&2
  exit 1
fi

while IFS=$'\t' read -r feature cow_source neo_owner status verification outcome; do
  if [[ "$feature" == "feature" ]]; then
    continue
  fi
  if [[ ! -e "$repo_root/$cow_source" ]]; then
    echo "parity ledger Cow source does not exist: $cow_source" >&2
    exit 1
  fi
  if [[ ! -e "$repo_root/$neo_owner" ]]; then
    echo "parity ledger Neo owner does not exist: $neo_owner" >&2
    exit 1
  fi
  if [[ "$feature" == "model_routing" && "$status" != "rejected" ]]; then
    echo "user-facing model routing must remain rejected" >&2
    exit 1
  fi
  : "$verification" "$outcome"
done < "$ledger"

echo "neo capability parity gate: protected runtime unchanged; 23 capability families accounted for"
