<!--
parent:
  order: false
-->
<p align="center">
  <img src="https://zezsqawedbikldiedlse.supabase.co/storage/v1/object/public/cdn.deus.paxeer.app/bcd6e442-788a-4377-b605-42ce2896d32e.png" alt="Paxeer Network" width="1200">
</p>
<p align="center">
<p align="center">
  <img src="https://img.shields.io/badge/Project-Matrix-FFFFFF?style=for-the-badge&labelColor=004CED" alt="Project: Matrix" />
  <img src="https://img.shields.io/badge/Built_by-PaxLabs-004CED?style=for-the-badge&labelColor=000000" alt="Built by PaxLabs" />
  <img src="https://img.shields.io/badge/License-Matrix--Protocol-004CED?style=for-the-badge&labelColor=000000" alt="License: Matrix-Protocol" />
  <img src="https://img.shields.io/badge/Status-Active-00C896?style=for-the-badge&labelColor=000000" alt="Status: Active" />
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Chain-HyperPaxeer-004CED?style=for-the-badge&labelColor=000000" alt="Chain: HyperPaxeer" />
  <img src="https://img.shields.io/badge/Chain_ID-125-FFFFFF?style=for-the-badge&labelColor=004CED" alt="Chain ID: 125" />
  <img src="https://img.shields.io/badge/Block_Time-400ms-00C896?style=for-the-badge&labelColor=000000" alt="Block Time: 400ms" />
  <img src="https://img.shields.io/badge/Finality-400ms-00C896?style=for-the-badge&labelColor=000000" alt="Finality: 400ms" />
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/ci.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/ci.yml/badge.svg?branch=main" alt="ci" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/lint.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/lint.yml/badge.svg?branch=main" alt="lint" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/docker.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/docker.yml/badge.svg?branch=main" alt="docker" /></a>
  <img src="https://img.shields.io/badge/Go-1.22-004CED?logo=go&logoColor=white" alt="Go 1.22" />
  <img src="https://img.shields.io/badge/Modules-9-004CED" alt="9 Go modules" />
</p>

---

## Что такое Matrix?

Matrix — это слой когниции и UX поверх [Paxeer Network](https://paxeer.app).
Он превращает запросы на естественном языке от непрограммистов в типизированную,
проверяемую и исправляемую **Intent IR** (промежуточное представление намерения),
которую агент действительно может исполнить — без четырёх классических режимов отказа,
ломающих сегодня рабочие процессы «непрограммист ↔ агент»:

1. **Хрупкость промпта** — небольшие переформулировки дают совершенно разный вывод.
2. **Потеря намерения** — естественный язык не переживает многошаговое исполнение.
3. **Отсутствие общей онтологии** — пользователь и агент расходятся в том, о какой сущности речь.
4. **Нет структурной коррекции** — при сносе пользователь вынужден переписывать всё с нуля.

Matrix предоставляет **две агентские «колеи»** поверх единого общего субстрата памяти и исполнения:

- **Neo** — *стандартный* разговорный агент с вызовом инструментов: привычный, надёжный,
  полностью разрешающий обратимую работу (shell, код, fetch, web). Любую денежную или
  необратимую работу он делегирует строгой колее.
- **Конвейер MCL** — *строгая* колея: естественный язык → типизированная Intent IR →
  план → воспроизводимый прогон, для высокорисковой / on-chain / необратимой работы.

Стек организован по слоям:

| Слой         | Роль                                                                                          |
| ------------ | --------------------------------------------------------------------------------------------- |
| **MCL**      | Протокол, превращающий NL → типизированную Intent IR. Глаголы закрыты (10), объекты закрыты (8 видов). |
| **cortex**   | Типизированный граф памяти по актору на Pebble. Журнал только на добавление, снимки с привязкой по Merkle. |
| **bridge**   | Связующий слой, подключающий интерфейс `Cortex` компилятора MCL к живому экземпляру cortex.    |
| **executor** | Обходчик планов, машина жизненного цикла, диспетчеризация инструментов MCP, демон на пользователя, рассказчик Liaison, e2e-стенд. |
| **neo**      | Стандартный разговорный агент — цикл вызова инструментов с постраничной памятью cortex.        |
| **gateway**  | Учётный LLM-прокси + реестр кредитов PAX (белый список бесплатного уровня + тарифная сетка).    |
| **router**   | Провижининг Fly Machine на пользователя + входная точка wake-then-reverse-proxy.               |
| **deus**     | Маркетплейс агентских сервисов: регистрация, обнаружение, учётный вызов, чеки EIP-712, хостинг. |
| **uwac**     | Universal Web Agent Connector — OAuth-хранилище → инструменты MCP на пользователя.              |
| **tachyon**  | Агентно-нативный движок Solidity/EVM — компиляция / тесты / симуляция / деплой.                 |
| **agents**   | Манифесты, привязанные к DID. Протокол, а не личность.                                          |
| **tools**    | MCP-серверы, которые вызывают агенты (включая те, что обращаются к цепочке).                    |


---

## Структура репозитория

```text
matrix/
├── cortex/        Типизированный граф памяти по актору (Pebble) + инвариант реплея + снимки Merkle
├── MCL/           Компилятор MatrixScript — lexer/parser/validator/canonical/interpreter + Intent IR + конверты + LLM-клиент
├── bridge/        Адаптер MCL ↔ cortex (отдельный Go-модуль; связан директивой replace)
├── executor/      Машина жизненного цикла, обходчик в рантайме, MCP-клиент + реестр инструментов, демон на пользователя (+ рассказчик Liaison), e2e-стенд
├── neo/           Neo — стандартный разговорный агент с вызовом инструментов (делегирует денежное/необратимое в MCL)
├── gateway/       Учётный LLM-прокси + реестр кредитов PAX (белый список бесплатного уровня + тарифная сетка)
├── router/        Провижининг Fly Machine на пользователя + входная точка wake-then-reverse-proxy
├── deus/          Маркетплейс агентских сервисов: регистрация, обнаружение, учётный вызов, чеки EIP-712, хостинг исполнения
├── uwac/          Universal Web Agent Connector — OAuth-хранилище → инструменты MCP на пользователя (в разработке)
├── tachyon/       Агентно-нативный движок Solidity/EVM — компиляция/тесты/симуляция/деплой (git-сабмодуль)
├── chronos/       Централизованный планировщик/система пробуждения агентов (дизайн заморожен)
├── agents/        Манифесты агентов, привязанные к DID (default.json, neo.json) + шаблоны MCP-серверов
├── tools/         MCP-серверы — paxeer, browser, tachyon, deus, uwac, web-search, media, cortex
├── skills/        Манифесты возможностей SKILL.mtx + прозаические тела SKILL.md
├── client/        Клиентское приложение Matrix (Next.js / React)
├── marketplace/   Маркетплейс Deus + панель разработчика (React Router на Cloudflare Workers)
├── deploy/        Образ демона, деплой Fly Machine, образы общих сервисов, скрипты установки box
├── rules/         Идентичность + правила кодирования по языкам
├── knowledge/     Канонические справочники (состояние проекта matrix.kvx, модели)
└── runs/          Временный вывод стенда (в gitignore)
```

### Go-модули

Корневой `Makefile` управляет **девятью** соседними Go-модулями — MCL, bridge, executor, gateway,
router, cortex, tachyon, deus, neo — наряду с **uwac** (и **chronos**, в работе).
Каждый можно независимо собрать `go build`/протестировать `go test` со своим `go.mod`;
межмодульные импорты во время разработки используют директивы `replace`, а при публикации —
явные версии.

```text
cortex   → matrix/cortex                    типизированный граф памяти, инвариант реплея, снимки/Merkle
MCL      → matrix/mcl                       компилятор + Intent IR + конверты + LLM-клиент
bridge   → matrix/bridge                    адаптер MCL ↔ cortex
executor → matrix/executor                  обходчик планов, жизненный цикл, диспетчеризация MCP, демон, Liaison
neo      → matrix/neo                       стандартный разговорный агент
gateway  → matrix/gateway                   учётный LLM-прокси + реестр кредитов PAX
router   → matrix/router                    маршрутизация Fly Machine на пользователя
deus     → github.com/paxlabs-inc/deus      маркетплейс агентских сервисов
uwac     → github.com/paxlabs-inc/uwac      коннекторы внешних приложений
tachyon  → github.com/paxlabs-inc/tachyon-tools   движок Solidity/EVM (сабмодуль)
```

---

## Быстрый старт

### Предварительные требования

- Go **1.22+** (toolchain зафиксирован во всех модулях).
- `make` (GNU make 4.x).
- Для процессов на MCP-серверах: `node` ≥ 20, `npx`, `python3` ≥ 3.11, `uv`.
- Для образа демона: `docker` с buildx.

### Клонирование и сборка

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core
make build           # собирает все девять Go-модулей
make install         # кладёт исполняемые CLI в ./bin
make test            # `go test -count=1 -race ./...` для каждого модуля
make vet             # `go vet ./...` для каждого модуля
make ci              # gofmt-check + vet + тесты (повторяет GitHub Actions)
```

Если нужен `golangci-lint` локально:

```bash
make lint-install    # зафиксировано на v1.61.0
make lint
```

### Настройка секретов

```bash
cp .env.example .env
# впишите FIREWORKS_API_KEY / TOGETHER_API_KEY для любой компиляции не в dry-run
# впишите MATRIX_DAEMON_TOKEN, если запускаете демон с аутентификацией
```

`.env` в gitignore; `.env.example` документирует каждую переменную, которую читает Matrix.

### Скомпилируйте своё первое намерение

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb  build
```

При заданном `FIREWORKS_API_KEY` компилятор выдаёт реальный Intent Frame (глагол,
типизированные объекты, блокирующие неизвестные). Без ключей он переходит в режим dry-run
и печатает полностью интерполированную структуру промпта.

### Запустите сквозной прогон

```bash
./bin/mcl-execute walk \
  -prose       "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

Это загружает манифест агента, поднимает объявленные им MCP-серверы, компилирует prose
в Intent + PlanTree, проходит план, журналирует каждый шаг как Event в cortex и завершается
тем, что `cortex.Attest` атомарно записывает `KindAttest` + `KindLearnWeights`.

### Запустите демон

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

Маршруты:

| Метод  | Путь             | Назначение                               |
| ------ | ---------------- | ---------------------------------------- |
| GET    | `/healthz`       | Liveness + статистика брокера SSE        |
| POST   | `/chat`          | Беседа с агентом (входная точка Neo)     |
| GET    | `/events`        | Поток Server-Sent Events (транскрипт)    |
| POST   | `/messages`      | Отправить prose-сообщение (строгая колея) |
| GET    | `/intents/{id}`  | Прочитать цепочку конвертов намерения по ID |
| GET    | `/me`            | Настройки + идентичность пользователя    |
| POST   | `/shutdown`      | Плавный дренаж                           |

---

## Документация

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — карта системы, границы модулей, ключевые инварианты
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — настройка разработки, дисциплина тестов, стиль коммитов
- [`SECURITY.md`](./SECURITY.md) — раскрытие уязвимостей
- [`CHANGELOG.md`](./CHANGELOG.md) — заметки о релизах в стиле Keep-a-Changelog
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — сборка образа + деплой на Fly
---

## Лицензия

Доступно в исходниках под **Matrix-Protocol License** — см. [`LICENSE.md`](./LICENSE.md).

Кратко: читайте, используйте, разворачивайте и интегрируйте свободно. Если вы **изменяете
и распространяете**, публикуйте свои изменения под той же лицензией. Коммерческая лицензия
требуется, как только вы пересекаете пороги коммерческого триггера (Взимаемые платежи >
100k USD / 12 месяцев **или** Ликвидность под контролем > 10M USD). Необязывающее резюме
в `LICENSE.md` приведено для удобства; авторитетным является текст самой лицензии.

---

<p align="center">
  Создано <a href="https://labs.paxeer.app">Paxlabs Inc.</a> · <code>SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol</code>
</p>