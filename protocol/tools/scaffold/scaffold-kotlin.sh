#!/usr/bin/env bash
# scaffold-kotlin.sh — production Kotlin/JVM application (Gradle Kotlin DSL).
# Tooling: ktlint · detekt · JUnit5 + kotest · Gradle wrapper · JRE Docker.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=protocol/tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "kotlin" "$@"
common_init_target
step "Kotlin/JVM app → $PROJECT_SLUG"

GROUP="io.${SCAFFOLD_VCS_ORG//-/}"
PKGPATH="$(echo "$GROUP.$(identify "$PROJECT_SLUG")" | tr '.' '/')"
MAINCLASS="$(pascalize "$PROJECT_SLUG")App"

mkdir -p "src/main/kotlin/${PKGPATH}" "src/test/kotlin/${PKGPATH}" gradle

write_if_absent settings.gradle.kts <<EOF
rootProject.name = "${PROJECT_SLUG}"
EOF

write_if_absent build.gradle.kts <<EOF
plugins {
    kotlin("jvm") version "2.1.0"
    application
    id("org.jlleitschuh.gradle.ktlint") version "12.1.2"
    id("io.gitlab.arturbosch.detekt") version "1.23.7"
}

group = "${GROUP}"
version = "0.1.0"

repositories { mavenCentral() }

dependencies {
    testImplementation(kotlin("test"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testImplementation("io.kotest:kotest-assertions-core:5.9.1")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

kotlin { jvmToolchain(21) }

application { mainClass.set("${GROUP}.$(identify "$PROJECT_SLUG").${MAINCLASS}Kt") }

tasks.test { useJUnitPlatform() }

detekt { buildUponDefaultConfig = true }
EOF

write_if_absent "src/main/kotlin/${PKGPATH}/${MAINCLASS}.kt" <<EOF
package ${GROUP}.$(identify "$PROJECT_SLUG")

fun greet(name: String, loud: Boolean = false): String {
    val msg = "Hello, \$name"
    return if (loud) "\${msg.uppercase()}!" else msg
}

fun main(args: Array<String>) {
    val name = args.firstOrNull() ?: "world"
    println(greet(name))
}
EOF

write_if_absent "src/test/kotlin/${PKGPATH}/GreetTest.kt" <<EOF
package ${GROUP}.$(identify "$PROJECT_SLUG")

import io.kotest.matchers.shouldBe
import kotlin.test.Test

class GreetTest {
    @Test
    fun greets() {
        greet("world") shouldBe "Hello, world"
    }

    @Test
    fun shouts() {
        greet("world", loud = true) shouldBe "HELLO, WORLD!"
    }
}
EOF

write_if_absent gradle.properties <<'EOF'
org.gradle.caching=true
org.gradle.parallel=true
org.gradle.configuration-cache=true
kotlin.code.style=official
EOF

write_if_absent detekt.yml <<'EOF'
build:
  maxIssues: 0
formatting:
  active: true
EOF

write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM eclipse-temurin:21-jdk AS build
WORKDIR /app
COPY . .
RUN ./gradlew --no-daemon installDist

FROM eclipse-temurin:21-jre-jammy AS runtime
WORKDIR /app
COPY --from=build /app/build/install/${PROJECT_SLUG} /app
RUN useradd -m app && chown -R app /app
USER app
ENTRYPOINT ["/app/bin/${PROJECT_SLUG}"]
EOF

gen_dockerignore

gen_gitignore_base
gitignore_add "gradle / jvm" ".gradle/
build/
!gradle/wrapper/gradle-wrapper.jar
*.class"

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
      - uses: actions/setup-java@v4
        with: { distribution: temurin, java-version: '21' }
      - uses: gradle/actions/setup-gradle@v4
      - run: ./gradlew ktlintCheck detekt test build
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "Kotlin/JVM application" \
  "./gradlew build" "./gradlew run" "./gradlew installDist" "./gradlew test" "./gradlew ktlintCheck detekt"

# Generate the Gradle wrapper if a gradle binary is available.
if have_cmd gradle; then
  info "generating gradle wrapper"
  gradle wrapper --gradle-version 8.12 -q || warn "gradle wrapper generation failed"
else
  warn "gradle not on PATH — run 'gradle wrapper' once to add ./gradlew"
fi

if [[ "$SCAFFOLD_INSTALL" == "1" && -x ./gradlew ]]; then
  info "gradle build"; ./gradlew --no-daemon build || warn "gradle build failed"
fi

finalize_git
common_done "Kotlin/JVM app · ${GROUP}"
