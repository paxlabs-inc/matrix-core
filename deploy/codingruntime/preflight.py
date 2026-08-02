#!/usr/bin/env python3
from __future__ import annotations

import importlib.metadata
import os
import stat
import sys
from pathlib import Path
from urllib.parse import urlparse

import yaml


EXPECTED_TOOLS = {
    "web_search", "web_extract", "terminal", "process", "read_file",
    "write_file", "patch", "search_files", "vision_analyze", "skills_list",
    "skill_view", "browser_navigate", "browser_snapshot", "browser_click",
    "browser_type", "browser_scroll", "browser_back", "browser_press",
    "browser_get_images", "browser_vision", "browser_console", "browser_cdp",
    "browser_dialog", "todo", "clarify", "delegate_task",
}

FORBIDDEN_DISTRIBUTIONS = {
    "aiohttp", "anthropic", "azure-identity", "boto3", "daytona",
    "discord.py", "exa-py", "fal-client", "firecrawl-py", "google-auth",
    "mautrix", "mcp", "mistralai", "modal", "parallel-web",
    "python-telegram-bot", "slack-bolt", "slack-sdk", "vercel",
}


def fail(message: str) -> None:
    raise SystemExit(f"matrix-agentcore preflight: {message}")


def require_directory(name: str) -> Path:
    raw = os.environ.get(name, "").strip()
    if not raw:
        fail(f"{name} is required")
    path = Path(raw)
    if not path.is_absolute() or not path.is_dir():
        fail(f"{name} must be an existing absolute directory")
    return path.resolve()


def require_url(name: str) -> str:
    raw = os.environ.get(name, "").strip().rstrip("/")
    parsed = urlparse(raw)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        fail(f"{name} must be an HTTP(S) URL")
    if not parsed.path.endswith("/v1"):
        fail(f"{name} must end in /v1")
    return raw


def load_policy() -> tuple[Path, dict]:
    managed = require_directory("AGENTCORE_MANAGED_DIR")
    path = managed / "config.yaml"
    try:
        raw = path.read_text(encoding="utf-8")
        policy = yaml.safe_load(raw)
    except Exception as exc:
        fail(f"managed config is unreadable or malformed: {exc}")
    if not isinstance(policy, dict):
        fail("managed config must be a mapping")
    mode = path.stat().st_mode
    if mode & (stat.S_IWGRP | stat.S_IWOTH):
        fail("managed config cannot be group/world writable")
    return path, policy


def check_policy(policy: dict) -> None:
    model = policy.get("model") or {}
    agent = policy.get("agent") or {}
    delegation = policy.get("delegation") or {}
    if model.get("provider") != "xiaomi" or model.get("default") != "mimo-v2.5-pro":
        fail("managed model must be xiaomi/mimo-v2.5-pro")
    if model.get("api_mode") != "chat_completions":
        fail("managed model api_mode must be chat_completions")
    headers = model.get("default_headers") or {}
    if headers.get("X-Matrix-Slot") != "cody":
        fail("managed model must bind the cody slot")
    if agent.get("coding_context") != "focus" or agent.get("verify_on_stop") is not True:
        fail("managed coding focus and verify-on-stop are required")
    if delegation.get("max_concurrent_children") != 3:
        fail("delegation concurrency must be three")
    if delegation.get("max_spawn_depth") != 1 or delegation.get("orchestrator_enabled") is not False:
        fail("delegation must be leaf-only at depth one")
    if policy.get("mcp_servers") != {}:
        fail("MCP servers are not allowed in coding runtime v1")


def check_runtime() -> None:
    home = require_directory("AGENTCORE_HOME")
    workspace = require_directory("AGENTCORE_WRITE_SAFE_ROOT")
    if home == workspace or home.is_relative_to(workspace):
        fail("AGENTCORE_HOME must be outside the project write root")
    require_url("XIAOMI_BASE_URL")
    if not os.environ.get("XIAOMI_API_KEY", "").strip():
        fail("XIAOMI_API_KEY scoped Matrix credential is required")
    if not os.environ.get("MATRIX_ACTOR_DID", "").strip():
        fail("MATRIX_ACTOR_DID is required")
    token = os.environ.get("AGENTCORE_DASHBOARD_SESSION_TOKEN", "")
    if len(token) < 32:
        fail("AGENTCORE_DASHBOARD_SESSION_TOKEN must have at least 32 characters")
    if os.environ.get("AGENTCORE_SAFE_MODE") != "1":
        fail("AGENTCORE_SAFE_MODE=1 is required")
    if os.environ.get("AGENTCORE_TUI_TOOLSETS") != "matrix-coding-v1":
        fail("AGENTCORE_TUI_TOOLSETS must be matrix-coding-v1")
    if os.environ.get("AGENTCORE_VERIFY_ON_STOP") != "1":
        fail("AGENTCORE_VERIFY_ON_STOP=1 is required")

    from toolsets import resolve_toolset
    actual = set(resolve_toolset("matrix-coding-v1", include_registry=False))
    if actual != EXPECTED_TOOLS:
        fail(f"tool allowlist mismatch: missing={sorted(EXPECTED_TOOLS - actual)} extra={sorted(actual - EXPECTED_TOOLS)}")

    import providers
    if providers._user_plugins_dir() is not None:
        fail("safe mode did not suppress user model-provider plugins")

    installed = {
        dist.metadata["Name"].lower()
        for dist in importlib.metadata.distributions()
        if dist.metadata.get("Name")
    }
    forbidden = sorted(installed & FORBIDDEN_DISTRIBUTIONS)
    if forbidden:
        fail(f"forbidden optional distributions installed: {', '.join(forbidden)}")


def main() -> None:
    _, policy = load_policy()
    check_policy(policy)
    check_runtime()
    if sys.argv[1:] == ["--check-only"]:
        print("matrix-agentcore preflight ok", flush=True)
        return
    argv = sys.argv[1:] or [
        "agentcore", "serve", "--host", "127.0.0.1", "--port", "9119", "--isolated",
    ]
    os.execvp(argv[0], argv)


if __name__ == "__main__":
    main()

