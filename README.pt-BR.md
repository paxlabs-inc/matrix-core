<!--
parent:
  order: false
-->
<p align="center">
<img src="MATRIX.gif" alt="Matrix" >
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxNiIgaGVpZ2h0PSIxNiIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIiBzdHJva2U9IiNmZmZmZmYiIHN0cm9rZS13aWR0aD0iMiIgc3Ryb2tlLWxpbmVjYXA9InJvdW5kIiBzdHJva2UtbGluZWpvaW49InJvdW5kIj48Y2lyY2xlIGN4PSIxMiIgY3k9IjEyIiByPSIxMCIvPjxwYXRoIGQ9Ik0xMiAxNnYtNCIvPjxwYXRoIGQ9Ik0xMiA4aC4wMSIvPjwvc3ZnPg==&logoColor=white" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square&logoColor=white" alt="Built by PaxLabs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
  <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/Version-1.0.0-0A0A0A?style=flat-square" alt="Version: 1.0.0" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Status-Active-0A0A0A?style=flat-square" alt="Status: Active" /></a>
  <a href="https://paxeer.app"><img src="https://img.shields.io/badge/Layer-Paxeer%20Network-0A0A0A?style=flat-square" alt="Paxeer Network" /></a>
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/stargazers"><img src="https://img.shields.io/github/stars/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Stars" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/network/members"><img src="https://img.shields.io/github/forks/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Forks" /></a>
  <a href="https://docs.matrixmcl.com"><img src="https://img.shields.io/badge/Docs-docs.matrixmcl.com-0A0A0A?style=flat-square" alt="Documentation" /></a>
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/search?l=go"><img src="https://img.shields.io/badge/Go-64.4%25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/search?l=typescript"><img src="https://img.shields.io/badge/TypeScript-13.5%25-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/search?l=html"><img src="https://img.shields.io/badge/HTML-11.6%25-E34F26?style=flat-square&logo=html5&logoColor=white" alt="HTML" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/search?l=c%2B%2B"><img src="https://img.shields.io/badge/C%2B%2B-4.8%25-00599C?style=flat-square&logo=cplusplus&logoColor=white" alt="C++" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/search?l=javascript"><img src="https://img.shields.io/badge/JavaScript-2.9%25-F7DF1E?style=flat-square&logo=javascript&logoColor=black" alt="JavaScript" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/search?l=python"><img src="https://img.shields.io/badge/Python-1.5%25-3776AB?style=flat-square&logo=python&logoColor=white" alt="Python" /></a>
</p>

---

<h2 align="center">Um framework de agentes e uma camada de cognição para LLMs.</h2>

<p align="center">
  Matrix leva um LLM além do chat e o coloca em execução por todo o domínio digital <br/>
  permitindo que humanos e máquinas coordenem trabalhos que precisam ser exatos.
</p>

---

## O que é Matrix?

Matrix é uma camada de cognição criada para a visão de Machine Economy da Paxeer Network. Ela estende um modelo de linguagem para além da conversa e o leva à execução real: coordenação financeira on-chain entre agentes, execução de tarefas de alto risco e tratamento seguro de trabalhos críticos e confidenciais.

A razão pela qual a maioria das stacks de agentes falha nesse tipo de trabalho é que elas levam linguagem natural até as camadas mais profundas. A linguagem humana é um canal com vazamentos: a forma biológica como raciocinamos, percebemos e fazemos aproximações se infiltra em cada frase que produzimos. Isso funciona para chat. Não funciona quando um agente está movimentando fundos, realizando uma escrita irreversível ou mantendo algo confidencial. Matrix oferece a humanos e máquinas uma forma de coordenar exatamente esse tipo de trabalho *sem* permitir que a ambiguidade do raciocínio humano e da linguagem humana alcance as partes que não podem ser ambíguas.

Ela faz isso por meio de três camadas.

## As três camadas

### 1 — The Matrix Compiler (MCL)

**O nível máximo de comando da stack.** MCL é um grupo de três agentes rigorosos que se comunicam, planejam e agem entre si por meio de um protocolo de verbos fechados, livre das limitações e da ambiguidade da linguagem humana e da entrada humana. Eles se coordenam com a exatidão de uma máquina e aplicam isso a tarefas reais, sensíveis e de alto risco: dinheiro, operações irreversíveis e tratamento de informações confidenciais.

Quando uma tarefa cruza a linha e passa a ter consequências relevantes, ela vem para cá. Os três agentes calculam o espaço de resultados, confirmam que possuem todas as entradas necessárias para o trabalho, pedem esclarecimentos quando não possuem e executam uma única vez, de acordo com a especificação. Nada é executado com base em suposições.

### 2 — The Cortex

**Um mecanismo completo de memória, contexto e estado imutável.** Cortex oferece a cada agente memória persistente e durável: uma linha do tempo de eventos por ator, atenção ativa e estado tipado, tudo somente acrescentável e reproduzível de forma determinística em nível de bytes. A continuidade deixa de ser uma ilusão que o modelo precisa fingir a cada sessão; ela é real para o usuário e inquebrável para o agente. Um agente que roda sobre Matrix não desperta vazio.

### 3 — The Loop Manager

**Um mecanismo de loop por agente.** Para cada agente, o Loop Manager coordena o fluxo e a troca constantes entre o usuário, o LLM e Cortex e escala para o pipeline do MCL no instante em que o trabalho se torna consequencial. É o runtime que mantém um agente coerente entre turnos, ferramentas e tempo, e sabe exatamente quando elevar uma decisão em vez de improvisar além do limite seguro.

## Como tudo se encaixa

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

O agente conversacional padrão (**Neo**) roda dentro do Loop Manager com ferramentas de shell, code, fetch e web disponíveis para trabalhos reversíveis. No instante em que o nível de risco aumenta, o Loop Manager escala para o MCL, e o controle retorna ao Neo quando esse rigor adicional deixa de ser necessário.

## Os módulos

O Makefile na raiz coordena vários módulos Go irmãos — cada um pode executar `go build` / `go test` de forma independente e possui seu próprio `go.mod`. As três camadas acima são mapeadas para eles: **MCL** é o grupo do compilador, **cortex** é o mecanismo de memória e **executor** implementa o Loop Manager.


  <img src="https://www.readmecodegen.com/api/file-tree-embed?repo=paxlabs-inc%2Fmatrix-core&branch=main&maxDepth=1&foldersOnly=true&transparentBg=true&showHeader=true" alt="Dynamic File Tree" />


| Módulo | Função |
|--------|------|
| **MCL** | O grupo do Matrix Compiler. Três agentes rigorosos de verbos fechados que planejam e agem sobre tarefas sensíveis e de alto risco com exatidão de máquina. |
| **cortex** | Mecanismo de memória tipada por ator sobre Pebble. Journal somente acrescentável, snapshots ancorados em Merkle e replay determinístico em nível de bytes. Persistente, imutável e durável. |
| **bridge** | Adaptador de MCL para cortex. Módulo Go separado para manter limites de interface limpos. |
| **executor** | O Loop Manager. Mecanismo de loop por agente, máquina de estados de ciclo de vida, dispatch MCP, daemon por usuário, narrador Liaison e harness de testes end-to-end. |
| **neo** | Agente conversacional padrão que roda dentro do loop, com escalonamento automático para MCL em operações consequenciais. |
| **gateway** | Proxy LLM tarifado com ledger de créditos PAX, allowlist da camada gratuita e aplicação da tabela de preços. |
| **router** | Provisionamento de Fly Machine por usuário com uma entrada que primeiro desperta a máquina e depois atua como reverse proxy. |
| **deus** | Marketplace de serviços de agentes: registro, descoberta, invocação tarifada, recibos EIP-712 e execução hospedada. |
| **tachyon** | Mecanismo Solidity/EVM nativo para agentes — compilar, testar, simular e implantar. (git submodule) |
| **uwac** | Universal Web Agent Connector — cofre OAuth que fornece ferramentas MCP por usuário. |
| **layerx** | Malha de liquidação e espinha dorsal de custódia para saldos de agentes. |
| **chronos** | Agendador centralizado de agentes e sistema de despertar. |
| **atlas** | Camada adicional de orquestração de infraestrutura. |
| **context** | Subsistema de gerenciamento de contexto. |
| **journal** | Subsistema de journal somente acrescentável para replay determinístico de estado. |
| **knowledge** | Referências canônicas: estado do projeto matrix.kvx, modelos e definições de schema. |
| **skills** | Manifestos de capacidades SKILL.mtx e descrições textuais de capacidades em SKILL.md. |
| **tools** | Servidores MCP: paxeer, browser, tachyon, deus, uwac, web-search, media, cortex. |
| **agents** | Manifestos de agentes vinculados a DID (default.json, neo.json) mais templates de servidores MCP. |
| **protocol** | Definições de protocolo e formatos de comunicação. |
| **marketplace** | Marketplace Deus e dashboard para desenvolvedores (React Router sobre Cloudflare Workers). |
| **client** | Aplicação de consumo Matrix (Next.js / React). |
| **deploy** | Imagem de contêiner do daemon, deploy de Fly Machine, imagens de serviços compartilhados e scripts de instalação de box. |

## Principais decisões de design

- **Coordenação por verbos fechados (D7)**: os agentes do MCL se coordenam por meio de 10 verbos fechados — `find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate` — para que a intenção entre agentes seja exata e nunca inferida em runtime a partir de texto livre.

- **8 tipos de objeto fechados**: `service`, `model`, `agent`, `knowledge`, `intent`, `asset`, `plan`, `capability`. Todo operando pertence a um desses tipos. Nenhum bloco não estruturado cruza a fronteira para a execução consequencial.

- **Invariante de replay (seção 13.4)**: o estado derivado sempre pode ser reconstruído a partir do journal com identidade byte a byte. Isso é aplicado a cada pull request por meio de `make ci`. Nada que um agente fez fica sem registro, e nada que ele não fez fica oculto.

- **Memória imutável**: Cortex é somente acrescentável e endereçado por conteúdo, portanto a continuidade de um agente não pode ser reescrita silenciosamente — durável para o agente e confiável para o usuário.

- **Recibos assinados**: toda execução consequencial termina em um recibo EIP-712 — entradas, saídas, custo e hash — que qualquer pessoa pode verificar posteriormente.

## Início rápido

### Pré-requisitos

- Go 1.22+
- GNU Make 4.x
- Node.js 20+
- Python 3.11+
- Docker com Buildx

### Build

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

### Configuração

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

### Executar um loop de agente

```bash
./bin/mcl-execute walk \
  -prose "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

### Iniciar o Daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

## Referência da API

O daemon expõe uma API HTTP leve para interação com agentes.

| Método | Caminho | Finalidade |
|--------|------|---------|
| `GET` | `/healthz` | Probe de liveness + estatísticas do broker SSE |
| `POST` | `/chat` | Conversar com o agente (loop conversacional via Neo) |
| `GET` | `/events` | Stream de Server-Sent Events para acompanhar o transcript em tempo real |
| `POST` | `/messages` | Enviar uma mensagem consequencial (escala para o grupo MCL) |
| `GET` | `/intents/{id}` | Ler a cadeia de intent envelope pelo intent ID |
| `GET` | `/me` | Configurações e identidade por usuário |
| `POST` | `/shutdown` | Drenagem e encerramento gracioso |

## Documentação

| Recurso | Descrição |
|----------|-------------|
| [Guia de Arquitetura](ARCHITECTURE.md) | Mapa do sistema, limites dos módulos, invariantes principais e justificativa de design |
| [Guia de Contribuição](CONTRIBUTING.md) | Configuração de desenvolvimento, disciplina de testes, estilo de commits e processo de PR |
| [Política de Segurança](SECURITY.md) | Divulgação de vulnerabilidades e comunicação responsável |
| [Changelog](CHANGELOG.md) | Notas de versão no formato Keep-a-Changelog |
| [Documentação do MCL](docs/MCL-docs/index.md) | Referência da linguagem MCL, gramática de verbos fechados e funcionamento interno dos agentes |
| [Guia de Deploy do Daemon](deploy/daemon/README.md) | Deploy em produção, configuração de Fly Machine e operações |
| [Documentação Completa](https://docs.matrixmcl.com) | Site completo de documentação em docs.matrixmcl.com |

## Contribuição

Matrix Core é open source e você pode **fazer fork e modificá-lo** livremente. A branch `main`, no entanto, é desenvolvida estritamente pela equipe principal: pull requests não solicitados geralmente não são integrados, e mudanças externas só são aceitas depois de trabalharmos diretamente com o contribuidor.

Antes de abrir qualquer coisa, leia a política de contribuição no início do [Guia de Contribuição](CONTRIBUTING.md). Issues, relatórios de bugs e divulgações de segurança são sempre bem-vindos.

Contribuidores:
- dev-paxeer
- Andrew
- paxlabs-inc
- cursoragent
- Sidiora-Technologies

## Licença

Matrix Core tem seu código-fonte disponível sob a [Matrix-Protocol License](LICENSE.md).

Você pode ler, usar, implantar e integrar Matrix Core livremente. Se modificar e redistribuir o software, deverá publicar suas alterações sob a mesma licença. Uma licença comercial da PaxLabs Inc. é necessária quando você ultrapassar os seguintes limites de ativação comercial:

- Taxas cobradas superiores a **USD 100.000** em qualquer período de 12 meses; ou
- Liquidez sob controle superior a **USD 10.000.000**.

Consulte [LICENSE.md](LICENSE.md) para os termos completos.

## READMEs internacionais

- [Espanhol](README.es.md)
- [Japonês](README.ja.md)
- [Português](README.pt-BR.md)
- [Russo](README.ru.md)
- [Chinês simplificado](README.zh-CN.md)

## Relacionados

- [Paxeer Network](https://paxeer.app) — A blockchain L1 sobre a qual Matrix Core é construído. Blocos de 400 ms, finalidade em 400 ms e arquitetura criada especificamente para cargas de trabalho de agentes.
- [PaxLabs](https://labs.paxeer.app) — Construindo o futuro da colaboração entre humanos e agentes.

---

<p align="center">
  Criado por <a href="https://labs.paxeer.app"><strong>PaxLabs Inc.</strong></a>
</p>

<p align="center">
  <sub>SPDX-License-Identifier: Matrix-Protocol</sub>
</p>