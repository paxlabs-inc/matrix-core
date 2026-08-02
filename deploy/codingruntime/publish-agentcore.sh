#!/usr/bin/env bash
set -euo pipefail
umask 077

runtime_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${runtime_dir}/../.." && pwd)"
pin_file="${runtime_dir}/agentcore.pin"
artifact_root="${repo_root}/.artifacts/codingruntime"

declare -A pin
while IFS='=' read -r key value; do
  [[ -z "${key}" ]] && continue
  pin["${key}"]="${value}"
done < "${pin_file}"

for key in source_revision runtime_image_repository runtime_image_digest runtime_oci_archive_sha256; do
  [[ -n "${pin[${key}]:-}" ]] || {
    printf 'missing %s in %s\n' "${key}" "${pin_file}" >&2
    exit 1
  }
done

command -v skopeo >/dev/null 2>&1 || {
  printf 'skopeo is required to promote the attested OCI archive\n' >&2
  exit 1
}

archive="${artifact_root}/${pin[source_revision]}.oci.tar"
[[ -f "${archive}" ]] || {
  printf 'missing OCI archive %s; run deploy/codingruntime/build-agentcore.sh first\n' "${archive}" >&2
  exit 1
}

actual_archive="$(sha256sum "${archive}" | cut -d' ' -f1)"
[[ "${actual_archive}" == "${pin[runtime_oci_archive_sha256]}" ]] || {
  printf 'OCI archive digest mismatch: expected %s, found %s\n' \
    "${pin[runtime_oci_archive_sha256]}" "${actual_archive}" >&2
  exit 1
}

metadata="${artifact_root}/${pin[source_revision]}.metadata.json"
actual_index="$(python3 - "${metadata}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["containerimage.digest"])
PY
)"
[[ "${actual_index}" == "${pin[runtime_image_digest]}" ]] || {
  printf 'OCI index digest mismatch: expected %s, found %s\n' \
    "${pin[runtime_image_digest]}" "${actual_index}" >&2
  exit 1
}

destination="${pin[runtime_image_repository]}:${pin[source_revision]}"
printf 'Promoting attested runtime to %s\n' "${destination}"
printf 'Registry authentication must already be configured with skopeo login.\n'
skopeo copy --all --preserve-digests \
  "oci-archive:${archive}" \
  "docker://${destination}"

remote_raw="$(mktemp)"
trap 'rm -f "${remote_raw}"' EXIT
skopeo inspect --raw "docker://${destination}" > "${remote_raw}"
remote_digest="sha256:$(sha256sum "${remote_raw}" | cut -d' ' -f1)"
[[ "${remote_digest}" == "${pin[runtime_image_digest]}" ]] || {
  printf 'registry digest mismatch: expected %s, found %s\n' \
    "${pin[runtime_image_digest]}" "${remote_digest}" >&2
  exit 1
}

printf 'Published immutable runtime %s@%s\n' \
  "${pin[runtime_image_repository]}" "${remote_digest}"
