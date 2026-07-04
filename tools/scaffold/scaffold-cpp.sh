#!/usr/bin/env bash
# scaffold-cpp.sh — production C++20 project (modern CMake).
# Tooling: CMake + presets · Catch2 (FetchContent) · clang-format · clang-tidy · sanitizers.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
source "$HERE/_common.sh"

common_parse_args "cpp" "$@"
common_init_target
step "C++ project → $PROJECT_SLUG"

NS="$(identify "$PROJECT_SLUG")"

mkdir -p "src" "include/${NS}" tests cmake

write_if_absent CMakeLists.txt <<EOF
cmake_minimum_required(VERSION 3.24)
project(${PROJECT_SLUG} VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Debug)
endif()

option(${NS^^}_BUILD_TESTS "Build tests" ON)
option(${NS^^}_ENABLE_ASAN "Enable AddressSanitizer" OFF)

add_library(${NS}_lib src/${NS}.cpp)
target_include_directories(${NS}_lib PUBLIC \${CMAKE_CURRENT_SOURCE_DIR}/include)
target_compile_options(${NS}_lib PRIVATE -Wall -Wextra -Wpedantic -Wconversion)

if(${NS^^}_ENABLE_ASAN)
  target_compile_options(${NS}_lib PUBLIC -fsanitize=address,undefined -fno-omit-frame-pointer)
  target_link_options(${NS}_lib PUBLIC -fsanitize=address,undefined)
endif()

add_executable(${PROJECT_SLUG} src/main.cpp)
target_link_libraries(${PROJECT_SLUG} PRIVATE ${NS}_lib)

if(${NS^^}_BUILD_TESTS)
  enable_testing()
  add_subdirectory(tests)
endif()
EOF

write_if_absent "include/${NS}/${NS}.hpp" <<EOF
#pragma once
#include <string>
#include <string_view>

namespace ${NS} {

/// Greet \`name\`; shout when \`loud\`.
[[nodiscard]] std::string greet(std::string_view name, bool loud = false);

}  // namespace ${NS}
EOF

write_if_absent "src/${NS}.cpp" <<EOF
#include "${NS}/${NS}.hpp"

#include <algorithm>
#include <cctype>

namespace ${NS} {

std::string greet(std::string_view name, bool loud) {
    std::string msg = "Hello, ";
    msg += name;
    if (loud) {
        std::transform(msg.begin(), msg.end(), msg.begin(),
                       [](unsigned char c) { return static_cast<char>(std::toupper(c)); });
        msg += '!';
    }
    return msg;
}

}  // namespace ${NS}
EOF

write_if_absent src/main.cpp <<EOF
#include <iostream>
#include <string>

#include "${NS}/${NS}.hpp"

int main(int argc, char** argv) {
    const std::string name = argc > 1 ? argv[1] : "world";
    std::cout << ${NS}::greet(name) << '\n';
    return 0;
}
EOF

write_if_absent tests/CMakeLists.txt <<EOF
include(FetchContent)
FetchContent_Declare(
  Catch2
  GIT_REPOSITORY https://github.com/catchorg/Catch2.git
  GIT_TAG v3.7.1
)
FetchContent_MakeAvailable(Catch2)

add_executable(unit_tests test_${NS}.cpp)
target_link_libraries(unit_tests PRIVATE ${NS}_lib Catch2::Catch2WithMain)

list(APPEND CMAKE_MODULE_PATH \${catch2_SOURCE_DIR}/extras)
include(Catch)
catch_discover_tests(unit_tests)
EOF

write_if_absent "tests/test_${NS}.cpp" <<EOF
#include <catch2/catch_test_macros.hpp>

#include "${NS}/${NS}.hpp"

TEST_CASE("greet default") {
    REQUIRE(${NS}::greet("world") == "Hello, world");
}

TEST_CASE("greet loud") {
    REQUIRE(${NS}::greet("world", true) == "HELLO, WORLD!");
}
EOF

# --- CMake presets ----------------------------------------------------------
write_if_absent CMakePresets.json <<EOF
{
  "version": 6,
  "cmakeMinimumRequired": { "major": 3, "minor": 24, "patch": 0 },
  "configurePresets": [
    {
      "name": "debug",
      "displayName": "Debug",
      "binaryDir": "\${sourceDir}/build/debug",
      "cacheVariables": { "CMAKE_BUILD_TYPE": "Debug" }
    },
    {
      "name": "release",
      "displayName": "Release",
      "binaryDir": "\${sourceDir}/build/release",
      "cacheVariables": { "CMAKE_BUILD_TYPE": "Release" }
    },
    {
      "name": "asan",
      "inherits": "debug",
      "binaryDir": "\${sourceDir}/build/asan",
      "cacheVariables": { "${NS^^}_ENABLE_ASAN": "ON" }
    }
  ],
  "buildPresets": [
    { "name": "debug", "configurePreset": "debug" },
    { "name": "release", "configurePreset": "release" },
    { "name": "asan", "configurePreset": "asan" }
  ],
  "testPresets": [
    { "name": "debug", "configurePreset": "debug", "output": { "outputOnFailure": true } }
  ]
}
EOF

write_if_absent .clang-format <<'EOF'
---
BasedOnStyle: Google
IndentWidth: 4
ColumnLimit: 100
DerivePointerAlignment: false
PointerAlignment: Left
EOF

write_if_absent .clang-tidy <<'EOF'
---
Checks: >
  bugprone-*,
  cppcoreguidelines-*,
  modernize-*,
  performance-*,
  readability-*,
  -modernize-use-trailing-return-type,
  -readability-magic-numbers,
  -cppcoreguidelines-avoid-magic-numbers
WarningsAsErrors: ''
HeaderFilterRegex: '.*'
EOF

write_if_absent Makefile <<'EOF'
.PHONY: configure build test asan clean fmt tidy
configure: ; cmake --preset debug
build: configure ; cmake --build --preset debug
test: build ; ctest --preset debug
asan: ; cmake --preset asan && cmake --build --preset asan && ctest --test-dir build/asan --output-on-failure
fmt: ; find src include tests -name '*.[ch]pp' | xargs clang-format -i
tidy: build ; clang-tidy -p build/debug $$(find src -name '*.cpp')
clean: ; rm -rf build
EOF

write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM debian:bookworm-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends \\
    build-essential cmake git ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN cmake --preset release -D ${NS^^}_BUILD_TESTS=OFF && cmake --build --preset release

FROM debian:bookworm-slim AS runtime
RUN useradd -m app
COPY --from=build /src/build/release/${PROJECT_SLUG} /usr/local/bin/${PROJECT_SLUG}
USER app
ENTRYPOINT ["/usr/local/bin/${PROJECT_SLUG}"]
EOF

gen_dockerignore

gen_gitignore_base
gitignore_add "c++" "/build/
compile_commands.json
*.o
*.obj
*.a"

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
      - name: install toolchain
        run: sudo apt-get update && sudo apt-get install -y cmake ninja-build clang-tidy
      - run: cmake --preset debug
      - run: cmake --build --preset debug
      - run: ctest --preset debug
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "C++20 project" \
  "cmake --preset debug" "make build" "cmake --build --preset release" "make test" "make tidy"

finalize_git
common_done "C++ project · namespace ${NS}"
