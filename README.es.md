<p align="center">
  <img src="https://cdn.redixusercontent.ocfstudio.com/matrix.png" alt="Matrix" />
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxNiIgaGVpZ2h0PSIxNiIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIiBzdHJva2U9IiNmZmZmZmYiIHN0cm9rZS13aWR0aD0iMiIgc3Ryb2tlLWxpbmVjYXA9InJvdW5kIiBzdHJva2UtbGluZWpvaW49InJvdW5kIj48Y2lyY2xlIGN4PSIxMiIgY3k9IjEyIiByPSIxMCIvPjxwYXRoIGQ9Ik0xMiAxNnYtNCIvPjxwYXRoIGQ9Ik0xMiA4aC4wMSIvPjwvc3ZnPg==&logoColor=white" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square&logoColor=white" alt="Built by PaxLabs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Status-Active-0A0A0A?style=flat-square" alt="Status: Active" /></a>
  <a href="https://paxeer.app"><img src="https://img.shields.io/badge/Layer-Paxeer%20Network-0A0A0A?style=flat-square" alt="Paxeer Network" /></a>
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/stargazers"><img src="https://img.shields.io/github/stars/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Stars" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/network/members"><img src="https://img.shields.io/github/forks/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Forks" /></a>
  <a href="https://docs.matrixmcl.com"><img src="https://img.shields.io/badge/Docs-docs.matrixmcl.com-0A0A0A?style=flat-square" alt="Documentation" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-38.7%25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/Solidity-26.3%25-363636?style=flat-square&logo=solidity&logoColor=white" alt="Solidity" />
  <img src="https://img.shields.io/badge/JavaScript-16.9%25-F7DF1E?style=flat-square&logo=javascript&logoColor=black" alt="JavaScript" />
  <img src="https://img.shields.io/badge/TypeScript-11.1%25-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" />
  <img src="https://img.shields.io/badge/HTML-5.5%25-E34F26?style=flat-square&logo=html5&logoColor=white" alt="HTML" />
  <img src="https://img.shields.io/badge/Python-0.5%25-3776AB?style=flat-square&logo=python&logoColor=white" alt="Python" />
</p>

---

<h2 align="center">Un framework de agentes y una capa de cognición para LLMs.</h2>

<p align="center">
  Matrix lleva un LLM más allá del chat y hacia la ejecución en todo el entorno digital <br/>
  permitiendo que humanos y máquinas coordinen el trabajo que debe ser exacto.
</p>

---

## ¿Qué es Matrix?

Matrix es una capa de cognición creada para la visión de Machine Economy de Paxeer Network. Extiende un modelo de lenguaje más allá de la conversación y lo lleva a la ejecución real: coordinación financiera on-chain entre agentes, ejecución de tareas de alto riesgo y manejo seguro de trabajo crítico y confidencial.

La razón por la que la mayoría de los stacks de agentes fallan en esta clase de trabajo es que llevan el lenguaje natural hasta las capas más profundas. El lenguaje humano es un canal con fugas: la forma biológica en la que razonamos, percibimos y aproximamos se filtra en cada frase que producimos. Eso está bien para el chat. No está bien cuando un agente mueve fondos, realiza una escritura irreversible o custodia información confidencial. Matrix ofrece a humanos y máquinas una forma de coordinar exactamente ese trabajo *sin* permitir que la ambigüedad del razonamiento humano y del lenguaje humano alcance las partes que no pueden ser ambiguas.

Lo hace mediante tres capas.

## Las tres capas

### 1 — The Matrix Compiler (MCL)

**El nivel superior de mando del stack.** MCL es un grupo de tres agentes rigurosos que se comunican, planifican y actúan entre sí mediante un protocolo de verbos cerrados, libre de las restricciones y la ambigüedad del lenguaje humano y de la entrada humana. Se coordinan con la exactitud de una máquina y la aplican a tareas reales, sensibles y de alto riesgo: dinero, operaciones irreversibles y manejo confidencial.

Cuando una tarea cruza la línea y pasa a tener consecuencias relevantes, llega aquí. Los tres agentes calculan el espacio de resultados, confirman que poseen todas las entradas que requiere el trabajo, solicitan aclaraciones cuando no es así y ejecutan una sola vez, conforme a la especificación. Nada se ejecuta basándose en una suposición.

### 2 — The Cortex

**Un motor completo de memoria, contexto y estado inmutable.** Cortex proporciona a cada agente una memoria persistente y duradera: una línea temporal de eventos por actor, atención activa y estado tipado, todo ello de solo anexado y reproducible de forma determinista a nivel de bytes. La continuidad deja de ser una ilusión que el modelo tiene que fingir en cada sesión; es real para el usuario e irrompible para el agente. Un agente que se ejecuta sobre Matrix no despierta vacío.

### 3 — The Loop Manager

**Un motor de bucle por agente.** Para cada agente, Loop Manager coordina el flujo y el intercambio constantes entre el usuario, el LLM y Cortex, y escala al pipeline de MCL en el momento en que el trabajo pasa a tener consecuencias relevantes. Es el runtime que mantiene a un agente coherente entre turnos, herramientas y tiempo, y sabe exactamente cuándo elevar una decisión en lugar de seguir improvisando más allá de ese punto.

## Cómo encaja todo

```
                        +-----------------------------+
                        |            User             |
                        +--------------+--------------+
                                       |
                                       v
                    +------------------+------------------+
                    |           Loop Manager              |
                    |     per-agent coordination loop     |
                    |     user  <->  LLM  <->  Cortex     |
                    +----+---------------+-----------+----+
                         |               |           |
                 reversible work         |       escalation
                         |               |           |
                         v               v           v
                    +---------+    +-----------+  +------------------+
                    |   LLM   |    |  Cortex   |  |  Matrix Compiler |
                    | (chat,  |    |  memory   |  |  (MCL)           |
                    |  tools) |    |  context  |  |  3 rigorous      |
                    +---------+    |  immutable|  |  closed-verb     |
                                   +-----------+  |  agents          |
                                                  +------------------+
                                                    money / on-chain /
                                                    irreversible /
                                                    confidential
```

El agente conversacional predeterminado (**Neo**) se ejecuta dentro de Loop Manager con herramientas de shell, code, fetch y web disponibles para trabajo reversible. En el instante en que aumenta el nivel de riesgo, Loop Manager escala a MCL y el control vuelve a Neo cuando ese rigor adicional deja de ser necesario.

## Los módulos

El Makefile raíz gestiona varios módulos Go hermanos; cada uno puede ejecutar `go build` / `go test` de forma independiente y tiene su propio `go.mod`. Las tres capas anteriores se corresponden con ellos: **MCL** es el grupo del compilador, **cortex** es el motor de memoria y **executor** implementa Loop Manager.


  <img src="https://www.readmecodegen.com/api/file-tree-embed?repo=paxlabs-inc%2Fmatrix-core&branch=main&maxDepth=1&foldersOnly=true&transparentBg=true&showHeader=true" alt="Dynamic File Tree" />


| Módulo | Función |
|--------|------|
| **MCL** | El grupo de Matrix Compiler. Tres agentes rigurosos de verbos cerrados que planifican y actúan sobre tareas sensibles y de alto riesgo con exactitud de máquina. |
| **cortex** | Motor de memoria tipada por actor sobre Pebble. Journal de solo anexado, snapshots anclados en Merkle y replay determinista a nivel de bytes. Persistente, inmutable y duradero. |
| **bridge** | Adaptador de MCL a cortex. Módulo Go separado para mantener límites de interfaz limpios. |
| **executor** | Loop Manager. Motor de bucle por agente, máquina de estados del ciclo de vida, dispatch MCP, daemon por usuario, narrador Liaison y harness de pruebas end-to-end. |
| **neo** | Agente conversacional predeterminado que se ejecuta dentro del bucle, con escalado automático a MCL para operaciones con consecuencias relevantes. |
| **gateway** | Proxy LLM medido con ledger de créditos PAX, lista permitida del nivel gratuito y aplicación de la tabla de tarifas. |
| **router** | Aprovisionamiento de Fly Machine por usuario con una entrada que primero despierta la máquina y después actúa como reverse proxy. |
| **deus** | Marketplace de servicios de agentes: registro, descubrimiento, invocación medida, recibos EIP-712 y ejecución alojada. |
| **tachyon** | Motor Solidity/EVM nativo para agentes: compilar, probar, simular y desplegar. (git submodule) |
| **uwac** | Universal Web Agent Connector: bóveda OAuth que proporciona herramientas MCP por usuario. |
| **layerx** | Tejido de liquidación y columna vertebral de custodia para saldos de agentes. |
| **chronos** | Planificador centralizado de agentes y sistema de activación. |
| **atlas** | Capa adicional de orquestación de infraestructura. |
| **context** | Subsistema de gestión de contexto. |
| **journal** | Subsistema de journal de solo anexado para replay determinista del estado. |
| **knowledge** | Referencias canónicas: estado del proyecto matrix.kvx, modelos y definiciones de schema. |
| **skills** | Manifiestos de capacidades SKILL.mtx y descripciones textuales de capacidades en SKILL.md. |
| **tools** | Servidores MCP: paxeer, browser, tachyon, deus, uwac, web-search, media, cortex. |
| **agents** | Manifiestos de agentes vinculados a DID (default.json, neo.json) y plantillas de servidores MCP. |
| **protocol** | Definiciones de protocolo y formatos de transmisión. |
| **marketplace** | Marketplace Deus y panel para desarrolladores (React Router sobre Cloudflare Workers). |
| **client** | Aplicación de consumo Matrix (Next.js / React). |
| **deploy** | Imagen de contenedor del daemon, despliegue de Fly Machine, imágenes de servicios compartidos y scripts de instalación de box. |

## Decisiones clave de diseño

- **Coordinación mediante verbos cerrados (D7)**: los agentes de MCL se coordinan mediante 10 verbos cerrados — `find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate` — de modo que la intención entre agentes sea exacta y nunca se infiera en runtime a partir de texto libre.

- **8 tipos de objeto cerrados**: `service`, `model`, `agent`, `knowledge`, `intent`, `asset`, `plan`, `capability`. Cada operando pertenece a uno de estos tipos. Ningún bloque no estructurado cruza la frontera hacia una ejecución con consecuencias relevantes.

- **Invariante de replay (sección 13.4)**: el estado derivado siempre puede reconstruirse desde el journal con identidad byte a byte. Se aplica en cada pull request mediante `make ci`. Nada de lo que hizo un agente queda sin registrar y nada de lo que no hizo queda oculto.

- **Memoria inmutable**: Cortex es de solo anexado y direccionado por contenido, por lo que la continuidad de un agente no puede reescribirse silenciosamente; es duradera para el agente y confiable para el usuario.

- **Recibos firmados**: cada ejecución con consecuencias relevantes termina en un recibo EIP-712 — entradas, salidas, coste y hash — que cualquiera puede verificar posteriormente.

## Inicio rápido

### Requisitos previos

- Go 1.22+
- GNU Make 4.x
- Node.js 20+
- Python 3.11+
- Docker con Buildx

### Compilación

```bash
# Clone the repository
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core

# Build all nine Go modules
make build

# Install runnable CLIs into ./bin
make install

# Run tests (go test -count=1 -race ./... per module)
make test

# Full CI check (gofmt + vet + tests; mirrors GitHub Actions)
make ci
```

### Configuración

```bash
# Copy the example environment file
cp .env.example .env

# Required for consequential (non-dry-run) execution:
#   FIREWORKS_API_KEY
#   TOGETHER_API_KEY
#
# Required for authenticated daemon mode:
#   MATRIX_DAEMON_TOKEN
```

### Ejecutar un bucle de agente

```bash
./bin/mcl-execute walk \
  -prose "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

### Iniciar el Daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

## Referencia de la API

El daemon expone una API HTTP ligera para interactuar con los agentes.

| Método | Ruta | Propósito |
|--------|------|---------|
| `GET` | `/healthz` | Sonda de liveness + estadísticas del broker SSE |
| `POST` | `/chat` | Conversar con el agente (bucle conversacional mediante Neo) |
| `GET` | `/events` | Stream de Server-Sent Events para seguir la transcripción en tiempo real |
| `POST` | `/messages` | Enviar un mensaje con consecuencias relevantes (escala al grupo MCL) |
| `GET` | `/intents/{id}` | Leer la cadena de intent envelope mediante intent ID |
| `GET` | `/me` | Configuración e identidad por usuario |
| `POST` | `/shutdown` | Drenaje y apagado controlados |

## Documentación

| Recurso | Descripción |
|----------|-------------|
| [Guía de arquitectura](ARCHITECTURE.md) | Mapa del sistema, límites de los módulos, invariantes clave y justificación del diseño |
| [Guía de contribución](CONTRIBUTING.md) | Configuración de desarrollo, disciplina de pruebas, estilo de commits y proceso de PR |
| [Política de seguridad](SECURITY.md) | Divulgación de vulnerabilidades y reporte responsable |
| [Changelog](CHANGELOG.md) | Notas de versión en formato Keep-a-Changelog |
| [Documentación de MCL](docs/MCL-docs/index.md) | Referencia del lenguaje MCL, gramática de verbos cerrados e internals de los agentes |
| [Guía de despliegue del Daemon](deploy/daemon/README.md) | Despliegue en producción, configuración de Fly Machine y operaciones |
| [Documentación completa](https://docs.matrixmcl.com) | Sitio completo de documentación en docs.matrixmcl.com |

## Contribución

Matrix Core es open source y puedes **hacer fork y modificarlo** libremente. Sin embargo, la rama `main` es desarrollada estrictamente por el equipo principal: los pull requests no solicitados generalmente no se integran y los cambios externos solo se aceptan después de haber trabajado directamente con el colaborador.

Antes de abrir cualquier cosa, lee la política de contribución al principio de la [Guía de contribución](CONTRIBUTING.md). Los issues, informes de errores y divulgaciones de seguridad son siempre bienvenidos.

Colaboradores:
- dev-paxeer
- Andrew
- paxlabs-inc
- cursoragent
- Sidiora-Technologies

## Licencia

Matrix Core tiene su código fuente disponible bajo la [Matrix-Protocol License](LICENSE.md).

Puedes leer, usar, desplegar e integrar Matrix Core libremente. Si modificas y redistribuyes el software, debes publicar tus cambios bajo la misma licencia. Se requiere una licencia comercial de PaxLabs Inc. una vez que superes los siguientes umbrales de activación comercial:

- Tarifas cobradas superiores a **100.000 USD** en cualquier período de 12 meses; o
- Liquidez bajo control superior a **10.000.000 USD**.

Consulta [LICENSE.md](LICENSE.md) para ver los términos completos.

## READMEs internacionales

- [Español](README.es.md)
- [Japonés](README.ja.md)
- [Portugués](README.pt-BR.md)
- [Ruso](README.ru.md)
- [Chino simplificado](README.zh-CN.md)

## Relacionados

- [Paxeer Network](https://paxeer.app) — La blockchain L1 sobre la que está construido Matrix Core. Bloques de 400 ms, finalidad en 400 ms y diseño específico para cargas de trabajo de agentes.
- [PaxLabs](https://labs.paxeer.app) — Construyendo el futuro de la colaboración entre humanos y agentes.

---

<p align="center">
  Creado por <a href="https://labs.paxeer.app"><strong>PaxLabs Inc.</strong></a>
</p>

<p align="center">
  <sub>SPDX-License-Identifier: Matrix-Protocol</sub>
</p>