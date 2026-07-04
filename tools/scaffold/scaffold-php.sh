#!/usr/bin/env bash
# scaffold-php.sh — production PHP 8.3 package.
# Tooling: Composer (PSR-4) · PHPUnit · PHPStan (max) · Laravel Pint · Docker.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
source "$HERE/_common.sh"

common_parse_args "php" "$@"
common_init_target
step "PHP package → $PROJECT_SLUG"

VENDOR="$(identify "$SCAFFOLD_VCS_ORG")"
NS="$(pascalize "$PROJECT_SLUG")"

mkdir -p src tests

write_if_absent composer.json <<EOF
{
  "name": "${SCAFFOLD_VCS_ORG}/${PROJECT_SLUG}",
  "description": "",
  "type": "library",
  "license": "${SCAFFOLD_LICENSE}",
  "authors": [
    { "name": "${SCAFFOLD_AUTHOR}", "email": "${SCAFFOLD_EMAIL}" }
  ],
  "require": {
    "php": ">=8.3"
  },
  "require-dev": {
    "phpunit/phpunit": "^11.5",
    "phpstan/phpstan": "^2.1",
    "laravel/pint": "^1.20"
  },
  "autoload": {
    "psr-4": { "Paxeer\\\\${NS}\\\\": "src/" }
  },
  "autoload-dev": {
    "psr-4": { "Paxeer\\\\${NS}\\\\Tests\\\\": "tests/" }
  },
  "scripts": {
    "test": "phpunit",
    "analyse": "phpstan analyse",
    "lint": "pint --test",
    "fmt": "pint"
  },
  "config": {
    "sort-packages": true,
    "optimize-autoloader": true
  },
  "minimum-stability": "stable",
  "prefer-stable": true
}
EOF

write_if_absent src/Greeter.php <<EOF
<?php

declare(strict_types=1);

namespace Paxeer\\${NS};

final class Greeter
{
    public function greet(string \$name, bool \$loud = false): string
    {
        \$msg = "Hello, {\$name}";

        return \$loud ? strtoupper(\$msg) . '!' : \$msg;
    }
}
EOF

write_if_absent tests/GreeterTest.php <<EOF
<?php

declare(strict_types=1);

namespace Paxeer\\${NS}\\Tests;

use Paxeer\\${NS}\\Greeter;
use PHPUnit\\Framework\\TestCase;

final class GreeterTest extends TestCase
{
    public function test_greet(): void
    {
        \$this->assertSame('Hello, world', (new Greeter())->greet('world'));
    }

    public function test_greet_loud(): void
    {
        \$this->assertSame('HELLO, WORLD!', (new Greeter())->greet('world', true));
    }
}
EOF

write_if_absent phpunit.xml <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<phpunit xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:noNamespaceSchemaLocation="vendor/phpunit/phpunit/phpunit.xsd"
         bootstrap="vendor/autoload.php"
         colors="true"
         failOnWarning="true"
         failOnRisky="true">
  <testsuites>
    <testsuite name="default">
      <directory>tests</directory>
    </testsuite>
  </testsuites>
  <source>
    <include>
      <directory>src</directory>
    </include>
  </source>
</phpunit>
EOF

write_if_absent phpstan.neon <<'EOF'
parameters:
  level: max
  paths:
    - src
    - tests
EOF

write_if_absent pint.json <<'EOF'
{
  "preset": "psr12"
}
EOF

write_if_absent Makefile <<'EOF'
.PHONY: install test analyse lint fmt clean
install: ; composer install
test:    ; composer test
analyse: ; composer analyse
lint:    ; composer lint
fmt:     ; composer fmt
clean:   ; rm -rf vendor .phpunit.cache
EOF

write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM composer:2 AS vendor
WORKDIR /app
COPY composer.json composer.lock* ./
RUN composer install --no-dev --no-scripts --prefer-dist --optimize-autoloader --no-interaction

FROM php:8.3-cli-alpine AS runtime
WORKDIR /app
COPY --from=vendor /app/vendor ./vendor
COPY . .
RUN adduser -D app && chown -R app /app
USER app
CMD ["php", "-a"]
EOF

gen_dockerignore

gen_gitignore_base
gitignore_add "php" "/vendor/
composer.phar
.phpunit.cache/
.phpunit.result.cache
.php-cs-fixer.cache"

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
      - uses: shivammathur/setup-php@v2
        with: { php-version: '8.3', tools: composer }
      - run: composer install --prefer-dist --no-interaction
      - run: composer lint
      - run: composer analyse
      - run: composer test
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "PHP package" \
  "composer install" "composer test" "composer install --no-dev" "composer test" "composer lint"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then
  if have_cmd composer; then info "composer install"; composer install --no-interaction || warn "composer install failed"; else warn "composer not installed"; fi
fi

finalize_git
common_done "PHP package · Paxeer\\${NS}"
