#!/usr/bin/env bash
set -euo pipefail
umask 077

runtime_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${runtime_dir}/../.." && pwd)"
source_dir="${repo_root}/tmp/agentcore"
pin_file="${runtime_dir}/agentcore.pin"
artifact_root="${repo_root}/.artifacts/codingruntime"

declare -A pin
while IFS='=' read -r key value; do
  [[ -z "${key}" ]] && continue
  pin["${key}"]="${value}"
done < "${pin_file}"

source_snapshot="$(python3 "${runtime_dir}/source-digest.py" "${source_dir}")"
[[ "${source_snapshot}" == "${pin[source_snapshot_sha256]}" ]] || {
  printf 'AgentCore patched source digest mismatch: expected %s, found %s\n' \
    "${pin[source_snapshot_sha256]}" "${source_snapshot}" >&2
  exit 1
}

vendor_patch="$(sha256sum "${runtime_dir}/matrix-agentcore.patch" | cut -d' ' -f1)"
[[ "${vendor_patch}" == "${pin[vendor_patch_sha256]}" ]] || {
  printf 'Centra AI vendor patch digest mismatch: expected %s, found %s\n' \
    "${pin[vendor_patch_sha256]}" "${vendor_patch}" >&2
  exit 1
}

head_ref="$(sed -n 's/^ref: //p' "${source_dir}/.git/HEAD")"
head_revision="$(tr -d '\r\n' < "${source_dir}/.git/${head_ref}")"
[[ "${head_revision}" == "${pin[source_revision]}" ]] || {
  printf 'AgentCore revision mismatch: expected %s, found %s\n' "${pin[source_revision]}" "${head_revision}" >&2
  exit 1
}

for item in \
  "uv.lock:${pin[uv_lock_sha256]}" \
  "package-lock.json:${pin[package_lock_sha256]}" \
  "LICENSE:${pin[license_sha256]}"; do
  file="${item%%:*}"
  expected="${item#*:}"
  actual="$(sha256sum "${source_dir}/${file}" | cut -d' ' -f1)"
  [[ "${actual}" == "${expected}" ]] || {
    printf '%s digest mismatch: expected %s, found %s\n' "${file}" "${expected}" "${actual}" >&2
    exit 1
  }
done

mkdir -p "${artifact_root}"
tag="matrix-agentcore:${pin[source_revision]}"
metadata="${artifact_root}/${pin[source_revision]}.metadata.json"
oci_archive="${artifact_root}/${pin[source_revision]}.oci.tar"

build_args=(
  --platform linux/amd64
  --progress plain
  --build-context "matrix-runtime=${runtime_dir}"
  --build-arg "AGENTCORE_GIT_SHA=${pin[source_revision]}"
  --build-arg "AGENTCORE_UV_LOCK_SHA256=${pin[uv_lock_sha256]}"
  --build-arg "AGENTCORE_PACKAGE_LOCK_SHA256=${pin[package_lock_sha256]}"
  --build-arg "AGENTCORE_LICENSE_SHA256=${pin[license_sha256]}"
  --tag "${tag}"
  --file "${runtime_dir}/Dockerfile"
)

docker buildx build \
  "${build_args[@]}" \
  --sbom=true \
  --provenance=mode=max \
  --metadata-file "${metadata}" \
  --output "type=oci,dest=${oci_archive},oci-mediatypes=true" \
  "${source_dir}"

docker buildx build \
  "${build_args[@]}" \
  --load \
  "${source_dir}"

image_id="$(docker image inspect "${tag}" --format '{{.Id}}')"
printf '%s  %s\n' "${image_id#sha256:}" "${tag}" > "${artifact_root}/${pin[source_revision]}.sha256"
sha256sum "${oci_archive}" > "${oci_archive}.sha256"
printf 'Built %s (%s)\nAttested OCI: %s\nMetadata: %s\n' \
  "${tag}" "${image_id}" "${oci_archive}" "${metadata}"
