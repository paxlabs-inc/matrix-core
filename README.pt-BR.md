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

## O que é o Matrix?

O Matrix é a camada de cognição e de experiência do usuário que fica sobre a [Paxeer Network](https://paxeer.app).
Ele transforma requisições em linguagem natural de pessoas não desenvolvedoras em uma
**Intent IR** (representação intermediária de intenção) tipada, inspecionável e corrigível
que um agente consegue de fato executar — sem os quatro modos de falha clássicos
que hoje quebram os fluxos entre não desenvolvedores e agentes:

1. **Fragilidade do prompt** — pequenas reformulações geram saídas radicalmente diferentes.
2. **Perda de intenção** — a linguagem natural não sobrevive à execução em vários passos.
3. **Ausência de ontologia compartilhada** — usuário e agente divergem sobre qual entidade está em questão.
4. **Sem correção estruturada** — o desvio força o usuário a reescrever do zero.

O Matrix oferece **dois trilhos de agente** sobre um único substrato compartilhado de memória e execução:

- **Neo** — o agente conversacional de chamada de ferramentas *padrão*: familiar, robusto e
  totalmente permissivo em trabalho reversível (shell, código, fetch, web). Ele delega qualquer
  tarefa monetária ou irreversível ao trilho rigoroso.
- **Pipeline MCL** — o trilho *rigoroso*: linguagem natural → Intent IR tipada →
  plano → caminhada reproduzível, para trabalho de alto risco / on-chain / irreversível.

A pilha é organizada em camadas:

| Camada       | Papel                                                                                          |
| ------------ | ---------------------------------------------------------------------------------------------- |
| **MCL**      | Protocolo que transforma NL → Intent IR tipada. Verbos fechados (10), objetos fechados (8 tipos). |
| **cortex**   | Grafo de memória tipado por ator sobre o Pebble. Journal somente de anexação, snapshots ancorados por Merkle. |
| **bridge**   | Cola que conecta a interface `Cortex` do compilador MCL a uma instância viva do cortex.         |
| **executor** | Caminhador de planos, máquina de ciclo de vida, despacho de ferramentas MCP, daemon por usuário, narrador Liaison, suíte e2e. |
| **neo**      | O agente conversacional padrão — laço de chamada de ferramentas com memória cortex paginada.   |
| **gateway**  | Proxy de LLM medido + livro-razão de créditos PAX (lista branca da camada gratuita + tabela de preços). |
| **router**   | Provisionamento de Fly Machine por usuário + porta de entrada wake-then-reverse-proxy.          |
| **deus**     | Marketplace de serviços de agentes: registro, descoberta, invocação medida, recibos EIP-712, hospedagem. |
| **uwac**     | Universal Web Agent Connector — cofre OAuth → ferramentas MCP por usuário.                       |
| **tachyon**  | Motor Solidity/EVM nativo para agentes — compilar / testar / simular / fazer deploy.            |
| **agents**   | Manifestos vinculados a DID. Protocolo, não personalidade.                                       |
| **tools**    | Servidores MCP que os agentes invocam (inclusive os que tocam a cadeia).                          |


---

## Estrutura do repositório

```text
matrix/
├── cortex/        Grafo de memória tipado por ator (Pebble) + invariante de replay + snapshots Merkle
├── MCL/           Compilador MatrixScript — lexer/parser/validador/canonical/interpretador + Intent IR + envelopes + cliente LLM
├── bridge/        Adaptador MCL ↔ cortex (módulo Go separado; ligado por diretiva replace)
├── executor/      Máquina de ciclo de vida, caminhador em runtime, cliente MCP + registro de ferramentas, daemon por usuário (+ narrador Liaison), suíte e2e
├── neo/           Neo — o agente conversacional de chamada de ferramentas padrão (delega o monetário/irreversível ao MCL)
├── gateway/       Proxy de LLM medido + livro-razão de créditos PAX (lista branca da camada gratuita + tabela de preços)
├── router/        Provisionamento de Fly Machine por usuário + porta wake-then-reverse-proxy
├── deus/          Marketplace de serviços de agentes: registro, descoberta, invocação medida, recibos EIP-712, execução hospedada
├── uwac/          Universal Web Agent Connector — cofre OAuth → ferramentas MCP por usuário (em construção)
├── tachyon/       Motor Solidity/EVM nativo para agentes — compilar/testar/simular/deploy (submódulo git)
├── chronos/       Agendador/sistema de wake-up centralizado de agentes (design congelado)
├── agents/        Manifestos de agente vinculados a DID (default.json, neo.json) + templates de servidor MCP
├── tools/         Servidores MCP — paxeer, browser, tachyon, deus, uwac, web-search, media, cortex
├── skills/        Manifestos de capacidade SKILL.mtx + corpos em prosa SKILL.md
├── client/        App de consumo do Matrix (Next.js / React)
├── marketplace/   Marketplace Deus + painel para desenvolvedores (React Router sobre Cloudflare Workers)
├── deploy/        Imagem do daemon, deploy em Fly Machine, imagens de serviços compartilhados, scripts de instalação box
├── rules/         Identidade + regras de codificação por linguagem
├── knowledge/     Referências canônicas (estado do projeto matrix.kvx, modelos)
└── runs/          Saída transitória da suíte (no gitignore)
```

### Os módulos Go

O `Makefile` raiz aciona **nove** módulos Go irmãos — MCL, bridge, executor, gateway,
router, cortex, tachyon, deus, neo — junto com **uwac** (e **chronos**, em andamento).
Cada um pode ser `go build`/`go test` de forma independente, com seu próprio `go.mod`; as
importações entre módulos usam diretivas `replace` durante o desenvolvimento e versões
explícitas na publicação.

```text
cortex   → matrix/cortex                    grafo de memória tipado, invariante de replay, snapshots/Merkle
MCL      → matrix/mcl                       compilador + Intent IR + envelopes + cliente LLM
bridge   → matrix/bridge                    adaptador MCL ↔ cortex
executor → matrix/executor                  caminhador de planos, ciclo de vida, despacho MCP, daemon, Liaison
neo      → matrix/neo                       agente conversacional padrão
gateway  → matrix/gateway                   proxy de LLM medido + livro-razão de créditos PAX
router   → matrix/router                    roteamento de Fly Machine por usuário
deus     → github.com/paxlabs-inc/deus      marketplace de serviços de agentes
uwac     → github.com/paxlabs-inc/uwac      conectores de apps externos
tachyon  → github.com/paxlabs-inc/tachyon-tools   motor Solidity/EVM (submódulo)
```

---

## Início rápido

### Pré-requisitos

- Go **1.22+** (toolchain fixada em todos os módulos).
- `make` (GNU make 4.x).
- Para os fluxos acionados por servidores MCP: `node` ≥ 20, `npx`, `python3` ≥ 3.11, `uv`.
- Para a imagem do daemon: `docker` com buildx.

### Clonar e compilar

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core
make build           # compila os nove módulos Go
make install         # coloca os CLIs executáveis em ./bin
make test            # `go test -count=1 -race ./...` por módulo
make vet             # `go vet ./...` por módulo
make ci              # gofmt-check + vet + tests (espelha o GitHub Actions)
```

Se precisar do `golangci-lint` localmente:

```bash
make lint-install    # fixado em v1.61.0
make lint
```

### Configurar os segredos

```bash
cp .env.example .env
# preencha FIREWORKS_API_KEY / TOGETHER_API_KEY para qualquer compilação que não seja dry-run
# preencha MATRIX_DAEMON_TOKEN se for rodar o daemon com autenticação
```

`.env` está no gitignore; `.env.example` documenta toda variável que o Matrix lê.

### Compile sua primeira intenção

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb  build
```

Com `FIREWORKS_API_KEY` definido, o compilador emite um Intent Frame real (verbo, objetos
tipados, incógnitas bloqueantes). Sem chaves, ele recorre ao modo dry-run e imprime a
estrutura do prompt totalmente interpolada.

### Execute uma caminhada de ponta a ponta

```bash
./bin/mcl-execute walk \
  -prose       "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

Isso carrega o manifesto do agente, sobe os servidores MCP que ele declara, compila a prosa
em um Intent + PlanTree, percorre o plano, registra cada passo como um Event do cortex e
termina com `cortex.Attest` gravando `KindAttest` + `KindLearnWeights` de forma atômica.

### Rodar o daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

Rotas:

| Método | Caminho          | Propósito                                |
| ------ | ---------------- | ---------------------------------------- |
| GET    | `/healthz`       | Liveness + estatísticas do broker SSE    |
| POST   | `/chat`          | Conversar com o agente (porta do Neo)    |
| GET    | `/events`        | Cauda de Server-Sent Events (transcrição) |
| POST   | `/messages`      | Enviar uma mensagem em prosa (trilho rigoroso) |
| GET    | `/intents/{id}`  | Ler a cadeia de envelopes de intenção por ID |
| GET    | `/me`            | Configurações + identidade por usuário   |
| POST   | `/shutdown`      | Drenagem graciosa                        |

---

## Documentação

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — mapa do sistema, fronteiras de módulos, invariantes-chave
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — configuração de desenvolvimento, disciplina de testes, estilo de commits
- [`SECURITY.md`](./SECURITY.md) — divulgação de vulnerabilidades
- [`CHANGELOG.md`](./CHANGELOG.md) — notas de versão no estilo Keep-a-Changelog
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — build da imagem + deploy no Fly
---

## Licença

Disponível em código-fonte sob a **Matrix-Protocol License** — veja [`LICENSE.md`](./LICENSE.md).

Versão curta: leia, use, faça deploy e integre livremente. Se você **modificar e redistribuir**,
publique suas alterações sob a mesma licença. Uma licença comercial é exigida quando você
ultrapassa os limiares de gatilho comercial (Taxas cobradas > USD 100k / 12 meses **ou**
Liquidez sob controle > USD 10M). O resumo não vinculante em `LICENSE.md` é por conveniência;
o corpo da licença é o texto autoritativo.

---

<p align="center">
  Construído por <a href="https://labs.paxeer.app">Paxlabs Inc.</a> · <code>SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol</code>
</p>