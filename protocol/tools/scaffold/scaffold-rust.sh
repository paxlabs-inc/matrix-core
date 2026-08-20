#!/usr/bin/env bash
# scaffold-rust.sh — production Rust crate (bin + lib).
# Tooling: clippy · rustfmt · nextest · cargo-deny · criterion · cargo-chef Docker.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=protocol/tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "rust" "$@"
require_cmd cargo
common_init_target
step "Rust crate → $PROJECT_SLUG"

CRATE="$(identify "$PROJECT_SLUG")"

mkdir -p src tests benches examples

write_if_absent Cargo.toml <<EOF
[package]
name = "${PROJECT_SLUG}"
version = "0.1.0"
edition = "2021"
rust-version = "1.82"
license = "${SCAFFOLD_LICENSE}"
authors = ["${SCAFFOLD_AUTHOR} <${SCAFFOLD_EMAIL}>"]
description = ""
repository = "https://github.com/${SCAFFOLD_VCS_ORG}/${PROJECT_SLUG}"

[lib]
name = "${CRATE}"
path = "src/lib.rs"

[[bin]]
name = "${PROJECT_SLUG}"
path = "src/main.rs"

[dependencies]
anyhow = "1"
thiserror = "2"

[dev-dependencies]
criterion = { version = "0.5", features = ["html_reports"] }

[[bench]]
name = "greet"
harness = false

[profile.release]
lto = "thin"
codegen-units = 1
strip = true

[lints.clippy]
all = { level = "warn", priority = -1 }
pedantic = { level = "warn", priority = -1 }
EOF

write_if_absent src/lib.rs <<EOF
//! ${PROJECT_SLUG} — core library.

/// Greets \`name\`, shouting when \`loud\`.
#[must_use]
pub fn greet(name: &str, loud: bool) -> String {
    let msg = format!("Hello, {name}");
    if loud {
        format!("{}!", msg.to_uppercase())
    } else {
        msg
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn greets() {
        assert_eq!(greet("world", false), "Hello, world");
    }

    #[test]
    fn shouts() {
        assert_eq!(greet("world", true), "HELLO, WORLD!");
    }
}
EOF

write_if_absent src/main.rs <<EOF
use ${CRATE}::greet;

fn main() -> anyhow::Result<()> {
    let name = std::env::args().nth(1).unwrap_or_else(|| "world".to_string());
    println!("{}", greet(&name, false));
    Ok(())
}
EOF

write_if_absent tests/integration.rs <<EOF
use ${CRATE}::greet;

#[test]
fn greet_integration() {
    assert_eq!(greet("paxeer", false), "Hello, paxeer");
}
EOF

write_if_absent benches/greet.rs <<EOF
use criterion::{criterion_group, criterion_main, Criterion};
use ${CRATE}::greet;

fn bench_greet(c: &mut Criterion) {
    c.bench_function("greet", |b| b.iter(|| greet(std::hint::black_box("world"), false)));
}

criterion_group!(benches, bench_greet);
criterion_main!(benches);
EOF

# --- toolchain + lint/format config ----------------------------------------
write_if_absent rust-toolchain.toml <<'EOF'
[toolchain]
channel = "stable"
components = ["rustfmt", "clippy"]
EOF

write_if_absent rustfmt.toml <<'EOF'
edition = "2021"
max_width = 100
imports_granularity = "Module"
group_imports = "StdExternalCrate"
EOF

write_if_absent deny.toml <<'EOF'
[advisories]
yanked = "deny"

[bans]
multiple-versions = "warn"
wildcards = "deny"

[licenses]
allow = ["MIT", "Apache-2.0", "BSD-3-Clause", "ISC", "Unicode-3.0"]
confidence-threshold = 0.9
EOF

write_if_absent Makefile <<'EOF'
.PHONY: build run test lint fmt bench deny audit clean
build:  ; cargo build
run:    ; cargo run
test:   ; cargo nextest run || cargo test
lint:   ; cargo clippy --all-targets --all-features -- -D warnings
fmt:    ; cargo fmt --all
bench:  ; cargo bench
deny:   ; cargo deny check
clean:  ; cargo clean
EOF

# --- Dockerfile (cargo-chef) ------------------------------------------------
write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM rust:1-slim AS chef
RUN cargo install cargo-chef
WORKDIR /app

FROM chef AS planner
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS build
COPY --from=planner /app/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json
COPY . .
RUN cargo build --release --bin ${PROJECT_SLUG}

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build /app/target/release/${PROJECT_SLUG} /${PROJECT_SLUG}
USER nonroot:nonroot
ENTRYPOINT ["/${PROJECT_SLUG}"]
EOF

gen_dockerignore

gen_gitignore_base
gitignore_add "rust" "/target/
**/*.rs.bk
Cargo.lock.orig"

gen_github_ci "$(cat <<'YAML'
name: ci
on:
  push: { branches: [main] }
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
        with: { components: rustfmt, clippy }
      - uses: Swatinem/rust-cache@v2
      - run: cargo fmt --all --check
      - run: cargo clippy --all-targets --all-features -- -D warnings
      - run: cargo test --all-features
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "Rust crate" \
  "cargo fetch" "cargo run" "cargo build --release" "make test" "make lint"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then
  info "fetching crates"; cargo fetch || warn "cargo fetch failed (no network?)"
fi

finalize_git
common_done "Rust crate · ${CRATE}"
