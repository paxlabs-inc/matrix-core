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

## ¿Qué es Matrix?

Matrix es la capa de cognición y de experiencia de usuario que se sitúa sobre [Paxeer Network](https://paxeer.app).
Convierte las solicitudes en lenguaje natural de personas no desarrolladoras en una
**Intent IR** (representación intermedia de intención) tipada, inspeccionable y corregible
que un agente puede ejecutar de verdad — sin los cuatro modos de fallo clásicos
que hoy rompen los flujos de trabajo entre no-desarrolladores y agentes:

1. **Fragilidad del prompt** — pequeñas reformulaciones producen salidas radicalmente distintas.
2. **Pérdida de intención** — el lenguaje natural no sobrevive a la ejecución de varios pasos.
3. **Falta de ontología compartida** — usuario y agente discrepan sobre a qué entidad se refieren.
4. **Sin corrección estructurada** — la desviación obliga al usuario a reescribir desde cero.

Matrix ofrece **dos rieles de agente** sobre un único sustrato compartido de memoria y ejecución:

- **Neo** — el agente conversacional de llamada a herramientas *por defecto*: familiar, robusto,
  totalmente permisivo en trabajo reversible (shell, código, fetch, web). Delega cualquier
  tarea monetaria o irreversible al riel riguroso.
- **Pipeline MCL** — el riel *riguroso*: lenguaje natural → Intent IR tipada →
  plan → recorrido reproducible, para el trabajo de alto riesgo / on-chain / irreversible.

La pila está dispuesta en capas:

| Capa         | Función                                                                                        |
| ------------ | ---------------------------------------------------------------------------------------------- |
| **MCL**      | Protocolo que convierte NL → Intent IR tipada. Verbos cerrados (10), objetos cerrados (8 tipos). |
| **cortex**   | Grafo de memoria tipado por actor sobre Pebble. Journal de solo anexado, snapshots anclados con Merkle. |
| **bridge**   | Pegamento que conecta la interfaz `Cortex` del compilador MCL con una instancia viva de cortex. |
| **executor** | Recorredor de planes, máquina de ciclo de vida, despacho de herramientas MCP, daemon por usuario, narrador Liaison, suite e2e. |
| **neo**      | El agente conversacional por defecto — bucle de llamada a herramientas con memoria cortex paginada. |
| **gateway**  | Proxy de LLM medido + libro mayor de créditos PAX (lista blanca de capa gratuita + tarifario). |
| **router**   | Aprovisionamiento de Fly Machine por usuario + puerta de entrada wake-then-reverse-proxy.       |
| **deus**     | Mercado de servicios de agentes: registro, descubrimiento, invocación medida, recibos EIP-712, hosting. |
| **uwac**     | Universal Web Agent Connector — bóveda OAuth → herramientas MCP por usuario.                    |
| **tachyon**  | Motor de Solidity/EVM nativo para agentes — compilar / probar / simular / desplegar.            |
| **agents**   | Manifiestos enlazados a DID. Protocolo, no personalidad.                                         |
| **tools**    | Servidores MCP que los agentes invocan (incluidos los que tocan la cadena).                      |


---

## Estructura del repositorio

```text
matrix/
├── cortex/        Grafo de memoria tipado por actor (Pebble) + invariante de replay + snapshots Merkle
├── MCL/           Compilador MatrixScript — lexer/parser/validador/canonical/intérprete + Intent IR + envelopes + cliente LLM
├── bridge/        Adaptador MCL ↔ cortex (módulo Go separado; enlazado por directiva replace)
├── executor/      Máquina de ciclo de vida, recorredor en runtime, cliente MCP + registro de herramientas, daemon por usuario (+ narrador Liaison), suite e2e
├── neo/           Neo — el agente conversacional de llamada a herramientas por defecto (delega lo monetario/irreversible a MCL)
├── gateway/       Proxy de LLM medido + libro mayor de créditos PAX (lista blanca de capa gratuita + tarifario)
├── router/        Aprovisionamiento de Fly Machine por usuario + puerta wake-then-reverse-proxy
├── deus/          Mercado de servicios de agentes: registro, descubrimiento, invocación medida, recibos EIP-712, ejecución alojada
├── uwac/          Universal Web Agent Connector — bóveda OAuth → herramientas MCP por usuario (en construcción)
├── tachyon/       Motor de Solidity/EVM nativo para agentes — compilar/probar/simular/desplegar (submódulo git)
├── chronos/       Planificador/sistema de wake-up centralizado de agentes (diseño congelado)
├── agents/        Manifiestos de agente enlazados a DID (default.json, neo.json) + plantillas de servidor MCP
├── tools/         Servidores MCP — paxeer, browser, tachyon, deus, uwac, web-search, media, cortex
├── skills/        Manifiestos de capacidad SKILL.mtx + cuerpos en prosa SKILL.md
├── client/        App de consumo de Matrix (Next.js / React)
├── marketplace/   Mercado Deus + panel para desarrolladores (React Router sobre Cloudflare Workers)
├── deploy/        Imagen del daemon, despliegue en Fly Machine, imágenes de servicios compartidos, scripts de instalación box
├── rules/         Identidad + reglas de codificación por lenguaje
├── knowledge/     Referencias canónicas (estado del proyecto matrix.kvx, modelos)
└── runs/          Salida transitoria de la suite (gitignored)
```

### Los módulos Go

El `Makefile` raíz impulsa **nueve** módulos Go hermanos — MCL, bridge, executor, gateway,
router, cortex, tachyon, deus, neo — junto a **uwac** (y **chronos**, en curso).
Cada uno se puede `go build`/`go test` de forma independiente con su propio `go.mod`; las
importaciones entre módulos usan directivas `replace` durante el desarrollo y versiones
explícitas al publicar.

```text
cortex   → matrix/cortex                    grafo de memoria tipado, invariante de replay, snapshots/Merkle
MCL      → matrix/mcl                       compilador + Intent IR + envelopes + cliente LLM
bridge   → matrix/bridge                    adaptador MCL ↔ cortex
executor → matrix/executor                  recorredor de planes, ciclo de vida, despacho MCP, daemon, Liaison
neo      → matrix/neo                       agente conversacional por defecto
gateway  → matrix/gateway                   proxy de LLM medido + libro mayor de créditos PAX
router   → matrix/router                    enrutamiento de Fly Machine por usuario
deus     → github.com/paxlabs-inc/deus      mercado de servicios de agentes
uwac     → github.com/paxlabs-inc/uwac      conectores de apps externas
tachyon  → github.com/paxlabs-inc/tachyon-tools   motor Solidity/EVM (submódulo)
```

---

## Inicio rápido

### Requisitos previos

- Go **1.22+** (toolchain fijado en todos los módulos).
- `make` (GNU make 4.x).
- Para los flujos impulsados por servidores MCP: `node` ≥ 20, `npx`, `python3` ≥ 3.11, `uv`.
- Para la imagen del daemon: `docker` con buildx.

### Clonar y compilar

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core
make build           # compila los nueve módulos Go
make install         # deja los CLI ejecutables en ./bin
make test            # `go test -count=1 -race ./...` por módulo
make vet             # `go vet ./...` por módulo
make ci              # gofmt-check + vet + tests (refleja GitHub Actions)
```

Si necesitas `golangci-lint` en local:

```bash
make lint-install    # fijado a v1.61.0
make lint
```

### Configurar los secretos

```bash
cp .env.example .env
# rellena FIREWORKS_API_KEY / TOGETHER_API_KEY para cualquier compilación que no sea dry-run
# rellena MATRIX_DAEMON_TOKEN si ejecutas el daemon con autenticación
```

`.env` está en gitignore; `.env.example` documenta cada variable que lee Matrix.

### Compila tu primera intención

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb  build
```

Con `FIREWORKS_API_KEY` definido, el compilador emite un Intent Frame real (verbo, objetos
tipados, incógnitas bloqueantes). Sin claves, recurre al modo dry-run e imprime la
estructura del prompt totalmente interpolada.

### Ejecuta un recorrido de extremo a extremo

```bash
./bin/mcl-execute walk \
  -prose       "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

Esto carga el manifiesto del agente, lanza los servidores MCP que declara, compila la prosa
en un Intent + PlanTree, recorre el plan, registra cada paso como un Event de cortex y
termina con `cortex.Attest` escribiendo `KindAttest` + `KindLearnWeights` de forma atómica.

### Ejecuta el daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

Rutas:

| Método | Ruta             | Propósito                                |
| ------ | ---------------- | ---------------------------------------- |
| GET    | `/healthz`       | Liveness + estadísticas del broker SSE   |
| POST   | `/chat`          | Conversar con el agente (puerta de Neo)  |
| GET    | `/events`        | Cola de Server-Sent Events (transcripción) |
| POST   | `/messages`      | Enviar un mensaje en prosa (riel riguroso) |
| GET    | `/intents/{id}`  | Leer la cadena de envelopes de intención por ID |
| GET    | `/me`            | Ajustes + identidad por usuario          |
| POST   | `/shutdown`      | Drenaje ordenado                         |

---

## Documentación

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — mapa del sistema, límites de módulos, invariantes clave
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — configuración de desarrollo, disciplina de tests, estilo de commits
- [`SECURITY.md`](./SECURITY.md) — divulgación de vulnerabilidades
- [`CHANGELOG.md`](./CHANGELOG.md) — notas de versión estilo Keep-a-Changelog
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — construcción de imagen + despliegue en Fly
---

## Licencia

Disponible en código fuente bajo la **Matrix-Protocol License** — consulta [`LICENSE.md`](./LICENSE.md).

Versión corta: lee, usa, despliega e integra con libertad. Si **modificas y redistribuyes**,
publica tus cambios bajo la misma licencia. Se requiere una licencia comercial una vez que
superas los umbrales de activación comercial (Tarifas cobradas > 100k USD / 12 meses **o**
Liquidez bajo control > 10M USD). El resumen no vinculante de `LICENSE.md` es por comodidad;
el cuerpo de la licencia es el texto autoritativo.

---

<p align="center">
  Creado por <a href="https://labs.paxeer.app">Paxlabs Inc.</a> · <code>SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol</code>
</p>