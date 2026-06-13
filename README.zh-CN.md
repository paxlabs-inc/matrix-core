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

## Matrix 是什么？

Matrix 是构建于 [Paxeer Network](https://paxeer.app) 之上的认知与交互层。
它把非开发者的自然语言请求转化为一种带类型、可检视、可纠正的
**Intent IR（意图中间表示）**，让智能体真正能够执行——同时规避当今破坏
“非开发者 ↔ 智能体”工作流的四大经典失败模式：

1. **提示词脆弱性** — 措辞的细微改动会导致输出大相径庭。
2. **意图丢失** — 自然语言无法在多步执行中保持完整。
3. **缺乏共享本体** — 用户与智能体对所指实体的理解不一致。
4. **无结构化纠错** — 一旦偏离，用户只能从头重写。

Matrix 在同一套共享的记忆与执行底座之上提供**两条智能体轨道**：

- **Neo** — *默认*的对话式工具调用智能体：熟悉、稳健，对可逆操作
  （shell、代码、fetch、web）完全开放。它会把任何涉及金钱或不可逆的
  工作委派给严谨轨道。
- **MCL 流水线** — *严谨*轨道：自然语言 → 带类型 Intent IR →
  计划 → 可重放的执行遍历，面向高风险 / 链上 / 不可逆的工作。

整个技术栈分层如下：

| 层           | 职责                                                                                          |
| ------------ | --------------------------------------------------------------------------------------------- |
| **MCL**      | 将 NL → 带类型 Intent IR 的协议。动词封闭（10 个），对象封闭（8 类）。                          |
| **cortex**   | 基于 Pebble 的按参与者类型化记忆图。仅追加日志，Merkle 锚定快照。                              |
| **bridge**   | 将 MCL 编译器的 `Cortex` 接口连接到运行中的 cortex 实例的胶水层。                              |
| **executor** | 计划遍历器、生命周期状态机、MCP 工具分发、按用户守护进程、Liaison 叙述器、端到端测试套件。      |
| **neo**      | 默认对话式智能体——带分页 cortex 记忆的工具调用循环。                                            |
| **gateway**  | 计量式 LLM 代理 + PAX 信用账本（免费层白名单 + 费率表）。                                       |
| **router**   | 按用户的 Fly Machine 预置 + 唤醒后反向代理的入口。                                              |
| **deus**     | 智能体服务市场：注册、发现、计量调用、EIP-712 收据、托管。                                      |
| **uwac**     | 通用 Web 智能体连接器——OAuth 保险库 → 按用户 MCP 工具。                                         |
| **tachyon**  | 智能体原生的 Solidity/EVM 引擎——编译 / 测试 / 模拟 / 部署。                                     |
| **agents**   | DID 绑定的清单。是协议，而非人格。                                                              |
| **tools**    | 智能体调用的 MCP 服务器（含触链的那些）。                                                       |


---

## 仓库结构

```text
matrix/
├── cortex/        按参与者类型化记忆图（Pebble）+ 重放不变式 + Merkle 快照
├── MCL/           MatrixScript 编译器——词法/语法/校验/规范化/解释器 + Intent IR + 信封 + LLM 客户端
├── bridge/        MCL ↔ cortex 适配器（独立 Go 模块；通过 replace 指令链接）
├── executor/      生命周期状态机、运行时遍历器、MCP 客户端 + 工具注册表、按用户守护进程（+ Liaison 叙述器）、端到端测试套件
├── neo/           Neo——默认对话式工具调用智能体（把涉及金钱/不可逆的工作委派给 MCL）
├── gateway/       计量式 LLM 代理 + PAX 信用账本（免费层白名单 + 费率表）
├── router/        按用户的 Fly Machine 预置 + 唤醒后反向代理的入口
├── deus/          智能体服务市场：注册、发现、计量调用、EIP-712 收据、托管执行
├── uwac/          通用 Web 智能体连接器——OAuth 保险库 → 按用户 MCP 工具（开发中）
├── tachyon/       智能体原生的 Solidity/EVM 引擎——编译/测试/模拟/部署（git 子模块）
├── chronos/       中心化智能体调度器 / 唤醒系统（设计已冻结）
├── agents/        DID 绑定的智能体清单（default.json、neo.json）+ MCP 服务器模板
├── tools/         MCP 服务器——paxeer、browser、tachyon、deus、uwac、web-search、media、cortex
├── skills/        SKILL.mtx 能力清单 + SKILL.md 散文正文
├── client/        Matrix 消费端应用（Next.js / React）
├── marketplace/   Deus 市场 + 开发者仪表板（基于 Cloudflare Workers 的 React Router）
├── deploy/        守护进程镜像、Fly Machine 部署、共享服务镜像、box 安装脚本
├── rules/         身份 + 各语言编码规范
├── knowledge/     权威参考（matrix.kvx 项目状态、模型）
└── runs/          临时测试套件输出（已 gitignore）
```

### Go 模块

根 `Makefile` 驱动**九个**同级 Go 模块——MCL、bridge、executor、gateway、
router、cortex、tachyon、deus、neo——以及 **uwac**（和正在开发中的 **chronos**）。
每个模块都能独立 `go build`/`go test`，拥有各自的 `go.mod`；跨模块导入
在开发期使用 `replace` 指令，发布时使用显式版本号。

```text
cortex   → matrix/cortex                    类型化记忆图、重放不变式、快照/Merkle
MCL      → matrix/mcl                       编译器 + Intent IR + 信封 + LLM 客户端
bridge   → matrix/bridge                    MCL ↔ cortex 适配器
executor → matrix/executor                  计划遍历器、生命周期、MCP 分发、守护进程、Liaison
neo      → matrix/neo                       默认对话式智能体
gateway  → matrix/gateway                   计量式 LLM 代理 + PAX 信用账本
router   → matrix/router                    按用户的 Fly Machine 路由
deus     → github.com/paxlabs-inc/deus      智能体服务市场
uwac     → github.com/paxlabs-inc/uwac      外部应用连接器
tachyon  → github.com/paxlabs-inc/tachyon-tools   Solidity/EVM 引擎（子模块）
```

---

## 快速开始

### 前置要求

- Go **1.22+**（所有模块统一锁定工具链）。
- `make`（GNU make 4.x）。
- 对于由 MCP 服务器驱动的流程：`node` ≥ 20、`npx`、`python3` ≥ 3.11、`uv`。
- 对于守护进程镜像：带 buildx 的 `docker`。

### 克隆与构建

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core
make build           # 构建全部九个 Go 模块
make install         # 把可运行的 CLI 放入 ./bin
make test            # 每个模块执行 `go test -count=1 -race ./...`
make vet             # 每个模块执行 `go vet ./...`
make ci              # gofmt 检查 + vet + 测试（与 GitHub Actions 一致）
```

如需本地安装 `golangci-lint`：

```bash
make lint-install    # 锁定 v1.61.0
make lint
```

### 配置密钥

```bash
cp .env.example .env
# 为任何非 dry-run 编译填入 FIREWORKS_API_KEY / TOGETHER_API_KEY
# 若需带鉴权运行守护进程，填入 MATRIX_DAEMON_TOKEN
```

`.env` 已被 gitignore；`.env.example` 记录了 Matrix 读取的每一个变量。

### 编译你的第一个意图

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb  build
```

设置 `FIREWORKS_API_KEY` 后，编译器会输出真实的 Intent Frame（动词、带类型对象、
阻塞性未知项）。未设置密钥时，它会回退到 dry-run 模式并打印
完整插值后的提示词结构。

### 驱动端到端执行遍历

```bash
./bin/mcl-execute walk \
  -prose       "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

这会加载智能体清单，启动它所声明的 MCP 服务器，把 prose 编译为
Intent + PlanTree，遍历该计划，将每一步作为 cortex 事件记入日志，并以
`cortex.Attest` 原子化写入 `KindAttest` + `KindLearnWeights` 结束。

### 运行守护进程

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

路由：

| 方法   | 路径             | 用途                                     |
| ------ | ---------------- | ---------------------------------------- |
| GET    | `/healthz`       | 存活检测 + SSE 代理统计                  |
| POST   | `/chat`          | 与智能体对话（Neo 入口）                 |
| GET    | `/events`        | 服务器推送事件尾随（转录）               |
| POST   | `/messages`      | 提交一条 prose 消息（严谨轨道）          |
| GET    | `/intents/{id}`  | 按 ID 读取意图信封链                     |
| GET    | `/me`            | 按用户的设置 + 身份                      |
| POST   | `/shutdown`      | 优雅排空                                 |

---

## 文档

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — 系统地图、模块边界、关键不变式
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — 开发环境搭建、测试纪律、提交风格
- [`SECURITY.md`](./SECURITY.md) — 漏洞披露
- [`CHANGELOG.md`](./CHANGELOG.md) — 基于 Keep-a-Changelog 的发布说明
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — 镜像构建 + Fly 部署
---

## 许可证

以 **Matrix-Protocol License** 形式提供源代码可用——参见 [`LICENSE.md`](./LICENSE.md)。

简短版本：可自由阅读、使用、部署、集成。如果你**修改并再分发**，
须以相同许可证发布你的改动。一旦越过商业触发阈值（收取费用 >
12 个月内 100k 美元 **或** 受控流动性 > 1000 万美元），则需商业许可证。
`LICENSE.md` 中的非约束性摘要仅为方便阅读；许可证正文具有权威效力。

---

<p align="center">
  由 <a href="https://labs.paxeer.app">Paxlabs Inc.</a> 构建 · <code>SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol</code>
</p>