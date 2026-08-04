FROM node:22-bookworm-slim@sha256:7af03b14a13c8cdd38e45058fd957bf00a72bbe17feac43b1c15a689c029c732 AS node

FROM ghcr.io/astral-sh/uv:0.11.6-python3.13-trixie@sha256:b3c543b6c4f23a5f2df22866bd7857e5d304b67a564f4feab6ac22044dde719b

ARG AGENTCORE_GIT_SHA
ARG AGENTCORE_UV_LOCK_SHA256
ARG AGENTCORE_PACKAGE_LOCK_SHA256
ARG AGENTCORE_LICENSE_SHA256

LABEL org.opencontainers.image.title="Matrix AgentCore coding runtime" \
      org.opencontainers.image.source="https://github.com/paxlabs-inc/agentcore.git" \
      org.opencontainers.image.revision="${AGENTCORE_GIT_SHA}" \
      org.opencontainers.image.licenses="MIT"

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    UV_PROJECT_ENVIRONMENT=/opt/agentcore/.venv \
    AGENTCORE_HOME=/var/lib/matrix-agentcore \
    AGENTCORE_MANAGED_DIR=/etc/matrix-agentcore \
    AGENTCORE_SAFE_MODE=1 \
    AGENTCORE_TUI_TOOLSETS=matrix-coding-v1 \
    AGENTCORE_TUI_TOOL_PROGRESS=all \
    AGENTCORE_VERIFY_ON_STOP=1 \
    AGENTCORE_DISABLE_LAZY_INSTALLS=1 \
    TERMINAL_ENV=local \
    PLAYWRIGHT_BROWSERS_PATH=/opt/agentcore/.playwright \
    PATH=/opt/agentcore/.venv/bin:/opt/agentcore/node_modules/.bin:/usr/local/bin:/usr/bin:/bin

COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
      ca-certificates git openssh-client passwd procps ripgrep \
      libxcb-shm0 libx11-xcb1 libx11-6 libxcb1 libxext6 libxrandr2 \
      libxcomposite1 libxcursor1 libxdamage1 libxfixes3 libxi6 libgtk-3-0 \
      libpangocairo-1.0-0 libpango-1.0-0 libatk1.0-0 libcairo-gobject2 \
      libcairo2 libgdk-pixbuf-2.0-0 libxrender1 libasound2 libfreetype6 \
      libfontconfig1 libdbus-1-3 libnss3 libnspr4 libatk-bridge2.0-0 \
      libdrm2 libxkbcommon0 libatspi2.0-0 libcups2 libxshmfence1 libgbm1 \
      fonts-noto-color-emoji fonts-noto-cjk fonts-freefont-ttf && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /opt/agentcore

COPY pyproject.toml uv.lock package.json package-lock.json LICENSE ./
RUN test "$(sha256sum uv.lock | cut -d' ' -f1)" = "${AGENTCORE_UV_LOCK_SHA256}" && \
    test "$(sha256sum package-lock.json | cut -d' ' -f1)" = "${AGENTCORE_PACKAGE_LOCK_SHA256}" && \
    test "$(sha256sum LICENSE | cut -d' ' -f1)" = "${AGENTCORE_LICENSE_SHA256}" && \
    touch README.md && \
    uv sync --frozen --no-dev --extra web --no-install-project && \
    npm ci --omit=dev --workspaces=false && \
    npx --no-install agent-browser install && \
    mkdir -p /opt/agentcore/.agent-browser && \
    mv /root/.agent-browser/browsers /opt/agentcore/.agent-browser/browsers && \
    browser_binary="$(find /opt/agentcore/.agent-browser/browsers -type f -name chrome -perm -0100 -print -quit)" && \
    test -n "${browser_binary}" && \
    ln -s "${browser_binary}" /usr/local/bin/google-chrome && \
    npm cache clean --force

COPY . .
RUN uv sync --frozen --no-dev --extra web && \
    /opt/agentcore/.venv/bin/python -c 'from importlib.util import find_spec; from pathlib import Path; source = Path("/opt/agentcore").resolve(); names = ("plugins.browser", "gateway.session_context"); specs = tuple(find_spec(name) for name in names); assert all(spec is not None and spec.origin is not None and Path(spec.origin).resolve().is_relative_to(source) for spec in specs)' && \
    printf '%s\n' "${AGENTCORE_GIT_SHA}" > /opt/agentcore/.agentcore_build_sha && \
    uv export --frozen --no-dev --extra web --no-emit-project \
      --format cyclonedx1.5 --output-file /opt/agentcore/python.cdx.json && \
    npm pkg delete devDependencies && \
    npm sbom --omit=dev --workspaces=false --sbom-format=cyclonedx \
      > /opt/agentcore/node.cdx.json

COPY --from=matrix-runtime managed-config.yaml /etc/matrix-agentcore/config.yaml
COPY --from=matrix-runtime preflight.py /opt/matrix-runtime/preflight.py
COPY --from=matrix-runtime notices.py /opt/matrix-runtime/notices.py
COPY --from=matrix-runtime smoke.py /opt/matrix-runtime/smoke.py
COPY --from=matrix-runtime git-readonly /opt/matrix/coding-bin/git

RUN python /opt/matrix-runtime/notices.py /opt/agentcore /opt/agentcore/THIRD_PARTY_NOTICES.txt && \
    mkdir -p /usr/share/licenses/matrix-agentcore && \
    cp LICENSE /usr/share/licenses/matrix-agentcore/AGENTCORE-LICENSE && \
    cp THIRD_PARTY_NOTICES.txt python.cdx.json node.cdx.json \
      /usr/share/licenses/matrix-agentcore/ && \
    chmod 0444 /etc/matrix-agentcore/config.yaml \
      /usr/share/licenses/matrix-agentcore/* && \
    /usr/sbin/useradd --uid 10000 --create-home --home-dir /var/lib/matrix-agentcore \
      --shell /usr/sbin/nologin matrix-agentcore && \
    mkdir -p /workspace /var/lib/matrix-agentcore && \
    chown matrix-agentcore:matrix-agentcore /workspace /var/lib/matrix-agentcore && \
    chmod 0755 /opt/matrix-runtime/preflight.py /opt/matrix-runtime/smoke.py \
      /opt/matrix/coding-bin/git

USER 10000:10000
WORKDIR /workspace
EXPOSE 9119
ENTRYPOINT ["python", "/opt/matrix-runtime/preflight.py"]
CMD ["agentcore", "serve", "--host", "127.0.0.1", "--port", "9119", "--isolated"]
