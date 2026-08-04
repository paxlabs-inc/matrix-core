#!/usr/bin/env python3
from __future__ import annotations

from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RUNTIME = ROOT / "deploy" / "codingruntime"


def pins() -> dict[str, str]:
    values: dict[str, str] = {}
    for line in (RUNTIME / "agentcore.pin").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            key, value = line.split("=", 1)
            values[key] = value
    return values


def require(text: str, needle: str, source: str) -> None:
    if needle not in text:
        raise SystemExit(f"{source}: missing {needle!r}")


def main() -> None:
    pin = pins()
    required = {
        "source_revision",
        "runtime_image_repository",
        "runtime_image_digest",
        "runtime_image_id",
        "runtime_oci_archive_sha256",
    }
    missing = sorted(required - pin.keys())
    if missing:
        raise SystemExit(f"agentcore.pin: missing {', '.join(missing)}")
    if not pin["runtime_image_digest"].startswith("sha256:"):
        raise SystemExit("agentcore.pin: runtime_image_digest must be a sha256 digest")

    dockerfile = (ROOT / "deploy" / "railway" / "Dockerfile").read_text(encoding="utf-8")
    neo_serve = (ROOT / "neo" / "cmd" / "neo" / "serve.go").read_text(encoding="utf-8")
    agentcore_dockerignore = (RUNTIME / "Dockerfile.dockerignore").read_text(encoding="utf-8")
    image = f'{pin["runtime_image_repository"]}@{pin["runtime_image_digest"]}'
    require(dockerfile, f"ARG AGENTCORE_RUNTIME_IMAGE={image}", "Railway Dockerfile")
    require(dockerfile, f'test "$(cat /opt/agentcore/.agentcore_build_sha)" = "{pin["source_revision"]}"', "Railway Dockerfile")
    for path in (
        "/opt/agentcore/.venv/bin/agentcore",
        "/opt/agentcore/node_modules/",
        "/opt/agentcore/.agent-browser/",
        "/opt/matrix/coding-bin/git",
        "/etc/matrix-agentcore/config.yaml",
        "/usr/share/licenses/matrix-agentcore/THIRD_PARTY_NOTICES.txt",
        "/usr/share/licenses/matrix-agentcore/python.cdx.json",
        "/usr/share/licenses/matrix-agentcore/node.cdx.json",
    ):
        require(dockerfile, path, "Railway Dockerfile")
    if "COPY --from=agentcore-runtime /opt/agentcore/ /opt/agentcore/" in dockerfile:
        raise SystemExit("Railway Dockerfile: private AgentCore source tree must not be copied into the final image")
    require(dockerfile, "source=/opt/agentcore,target=/run/matrix-agentcore-source,readonly", "Railway Dockerfile")
    require(dockerfile, "pip install", "Railway Dockerfile")
    require(dockerfile, "--reinstall --no-deps --no-cache --compile-bytecode", "Railway Dockerfile")
    require(dockerfile, "-name '__editable__*'", "Railway Dockerfile")
    require(dockerfile, "test ! -e /opt/agentcore/pyproject.toml", "Railway Dockerfile")
    require(dockerfile, "test ! -e /opt/agentcore/uv.lock", "Railway Dockerfile")
    require(dockerfile, "is_relative_to(site)", "Railway Dockerfile")
    require(dockerfile, 'find_spec("plugins.browser").origin', "Railway Dockerfile")
    require(agentcore_dockerignore, "!plugins/browser", "AgentCore Docker build context")
    require(agentcore_dockerignore, "!plugins/browser/**", "AgentCore Docker build context")
    require(agentcore_dockerignore, "!gateway/session_context.py", "AgentCore Docker build context")
    require(
        neo_serve,
        'strings.EqualFold(strings.TrimSpace(os.Getenv("NEO_CODING_RUNTIME_ENABLED")), "true")',
        "Neo coding-runtime activation",
    )
    for setting in (
        "NEO_CODING_RUNTIME_ENABLED=false",
        "NEO_CODING_RUNTIME_REQUIRED=false",
        "NEO_AGENTCORE_BINARY=/opt/agentcore/.venv/bin/agentcore",
        "NEO_AGENTCORE_MANAGED_DIR=/etc/matrix-agentcore",
        "NEO_AGENTCORE_HOME=/data/agentcore",
        "NEO_BUILD_JOBS_DIR=/data/build-jobs",
    ):
        require(dockerfile, setting, "Railway Dockerfile")

    entrypoint = (ROOT / "deploy" / "railway" / "entrypoint.sh").read_text(encoding="utf-8")
    require(entrypoint, '"${DATA_DIR}/agentcore"', "Railway entrypoint")
    require(entrypoint, '"${DATA_DIR}/build-jobs"', "Railway entrypoint")
    require(entrypoint, 'chmod 0700 "${DATA_DIR}/build-jobs"', "Railway entrypoint")

    workflow = (ROOT / ".github" / "workflows" / "docker.yml").read_text(encoding="utf-8")
    if workflow.count('"deploy/codingruntime/**"') < 3:
        raise SystemExit("docker workflow: coding runtime must trigger push, PR, and Railway image selection")
    require(workflow, "python3 deploy/codingruntime/verify-packaging.py", "docker workflow")
    if "name: registry login\n        if:" in workflow:
        raise SystemExit("docker workflow: private runtime login cannot be skipped on PR builds")

    print(f"coding runtime packaging ok: {image}")


if __name__ == "__main__":
    main()
