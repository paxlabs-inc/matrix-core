<p align="center">
  <img src="https://pub-cc719bc237f94810bec78e93e056bec4.r2.dev/centra.ai_wordmark_dark.png" alt="Centra AI" width="520" />
</p>


<p align="center">
  <strong>面向严肃、长期任务的有状态 AI 操作系统。</strong>
</p>

<p align="center">
  Centra AI 为智能体提供持久记忆、真实工具、隔离计算环境、协同执行能力，
  并完整保留它们实际完成工作的证据。
</p>

<p align="center">
  <a href="https://github.com/Sidiora-Labs/centra-llm-agents"><img src="https://img.shields.io/badge/Project-Centra%20AI-0A0A0A?style=flat-square" alt="Centra AI" /></a>
  <a href="https://github.com/Sidiora-Labs"><img src="https://img.shields.io/badge/Built%20by-Sidiora%20Labs-0A0A0A?style=flat-square" alt="Built by Sidiora Labs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Centra%20AI%20Protocol-0A0A0A?style=flat-square" alt="Centra AI Protocol License" /></a>
  <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/Version-1.0.0-0A0A0A?style=flat-square" alt="Version 1.0.0" /></a>
</p>

## Centra AI 是什么？

Centra AI 是 Sidiora Labs 构建的私有、持久化智能体平台。它专为无法依靠一次提示可靠完成的工作而设计：开发软件、调查复杂问题、操作浏览器和文件、协调多个专业智能体、生成可交付成果，以及跨会话持续推进任务而不丢失上下文。

系统以两个核心智能体为中心：

- **Neo** 是主要智能体。它负责研究、推理、调用工具、创建成果、管理进行中的工作，并能在单次模型响应结束后继续推进任务。
- **Ion** 是技术智能体和编程环境。它在受限工作区中直接操作真实项目、终端、文件、测试、预览和开发工具链。

它们由 **Workforce** 和 **Neo Computer** 提供支持。Workforce 将大型目标拆解为受治理的并行工作；Neo Computer 则统一呈现来源、引用、预览、成果、变更记录和工作区证据。

Centra 并不围绕聊天记录组织产品。对话是控制界面，真正的产品是背后的工作系统。

## 核心差异

### 持久认知

Centra 持久保存智能体身份、记忆、当前目标、证据和未完成工作。进程重启或上下文窗口切换后，智能体无需从零开始。Cortex 与 Neocortex 记录发生了什么、还有什么未完成，以及系统为什么得出当前结论。

### 真实执行环境

智能体可以直接使用文件、终端、浏览器、代码库、数据库、本地服务、媒体工具和外部系统。编程发生在真实项目工作区中，具备包管理、测试、服务运行、实时预览和可下载交付物，而不是停留在代码片段里。

### 证据优先

工具活动、来源材料、精确引用、成果版本、检查点和验证结果都是产品的一部分。Centra 区分“听起来可信的回答”与“已经完成且可验证的工作”，并保留审查这种差异所需的证据。

### 长周期工作与恢复

任务可以被拆解、排队、恢复、修正和继续执行。智能体能够安排后续工作，在中断后恢复状态，并协调边界明确的专业智能体，而不必把整个任务压缩进一个巨大提示中。

### 明确的权限边界

常规且可逆的工作可以快速进行；敏感写入、外部副作用、财务操作及其他高影响行为必须经过明确的权限和审批边界。每次决策都保留可审计的路径。

### 每用户隔离

生产架构为每位用户提供独立的智能体环境和持久状态边界。中央服务负责认证、路由、计量和唤醒这些环境，而用户工作区不会被合并到共享执行池中。智能体在专属环境内拥有完成工作的能力，但无法访问平台源码、宿主控制权、运行时身份或凭据。

## 产品系统

```text
                                用户
                                  |
                     Centra 客户端与 Neo Computer
                                  |
                    +-------------+-------------+
                    |                           |
                   Neo                         Ion
             研究、操作、成果、自动化       软件项目、终端、测试、预览
                    |                           |
                    +-------------+-------------+
                                  |
                         认知、证据与工作账本
                     Cortex / Neocortex / Vault
                                  |
              +-------------------+-------------------+
              |                   |                   |
          Workforce           原生工具              外部系统
          受治理的并行工作   浏览器、文件、终端、媒体   API 与服务
```

## 核心能力

| 领域 | Centra 提供的能力 |
| --- | --- |
| 软件工程 | 理解代码库、执行终端命令、安装依赖、运行测试与服务、查看差异、生成预览、建立检查点并交付项目。 |
| 研究 | 多来源调查、精确引用、原文摘录、综合分析和可持久保存的研究成果。 |
| 计算机操作 | 在权限受限、状态可恢复且证据可见的条件下操作浏览器和桌面环境。 |
| 知识与记忆 | 持久身份、偏好、事实、目标、对话连续性、时间检索和与证据关联的结论。 |
| 协同工作 | 任务拆解、专业智能体调度、受控并行、状态回执、监督与可恢复执行。 |
| 成果与媒体 | 通过 Neo Computer 呈现文档、代码、数据、图像、预览、版本和工作区原生输出。 |
| 自动化 | 定时工作、按需唤醒、主动简报，以及由用户明确控制的持久队列。 |
| 安全与权限 | 每用户隔离、范围受限的工具、加密状态、审批关卡、审计记录和故障关闭控制。 |

## 架构

Centra 是由多个可独立构建的服务和客户端组成的单体仓库。其关键边界很清晰：智能体可以在专属用户环境内拥有广泛能力，但守护进程源码、平台身份、宿主控制和凭据始终位于其权限范围之外。

| 组件 | 职责 |
| --- | --- |
| `agents/neo/` | 主要智能体运行时、流式对话、工具、记忆集成、自动化和交付。 |
| `ion/` | 技术智能体、项目理解、编程工作区、计算机控制和安全策略。 |
| `workforce/` | 多智能体任务拆解、监督、任务状态和协同执行。 |
| `client/` | Next.js 产品客户端，包括聊天、编程、Neo Computer、工作界面和实时状态。 |
| `core/neocortex/` | 基于证据的确定性记忆引擎，支持重放、投影和精确恢复。 |
| `core/cortex/` | 持久化 Go 记忆层及兼容路径。 |
| `packages/vault/` | 信封加密和每用户数据保护。 |
| `executor/` | 持久操作生命周期、工具调度、检查点和高影响操作执行。 |
| `router/` | 认证、每用户路由、配置、唤醒和反向代理。 |
| `gateway/` | 模型访问、计量、策略和供应商路由。 |
| `packages/chronos/` | 持久调度、唤醒事件、周期任务和主动交付。 |
| `packages/sandboxd/` | 受限工作区和预览基础设施。 |
| `protocol/codegraph/` | 结构化代码智能和智能体自我模型数据。 |

根目录 Makefile 当前管理十五个 Go 模块。现有服务名、环境变量、协议头和镜像路径等兼容性敏感的机器标识记录在 [BRANDING.md](BRANDING.md) 中。

## 可靠性模型

Centra 将可靠性视为系统工程问题：

- **状态可持久化。** 对话、工作、记忆和检查点可以跨进程保存。
- **结论有来源。** 来源、工具结果和成果始终与使用它们的回答关联。
- **未完成就是未完成。** 资源耗尽、取消、歧义和验证失败都会被如实记录。
- **恢复能力内建。** 监督器可以从持久状态重建进行中的工作，而不是猜测最后一条消息的含义。
- **权限明确。** 工具和副作用受用户、环境、操作类型与审批状态共同约束。
- **派生状态可重建。** 证据日志和确定性投影支持恢复与审计，而不依赖不透明的内存快照。

## 快速开始

### 环境要求

- Go 1.26.5
- Node.js 22+
- pnpm 10.33+
- Python 3.11+
- GNU Make 4.x
- 支持 Buildx 的 Docker

### 克隆并构建后端

```bash
git clone https://github.com/Sidiora-Labs/centra-llm-agents.git
cd centra-llm-agents

make build
make install
```

`make build` 构建根目录 Makefile 中列出的十五个 Go 模块，`make install` 将可执行文件写入 `./bin`。

### 运行客户端

```bash
cd client
corepack enable
pnpm install
pnpm dev
```

客户端默认启动在 `http://localhost:3000`。运行时服务还需要各模块及部署配置中说明的环境变量。

### 运行本地验证

```bash
make ci

cd client
pnpm build
```

## 仓库结构

```text
centra-llm-agents/
|-- client/          产品客户端与 Neo Computer
|-- agents/neo/             主要智能体
|-- ion/             技术智能体与编程环境
|-- workforce/       协同工作系统
|-- core/cortex/          持久记忆层
|-- core/neocortex/       确定性证据引擎
|-- executor/        持久操作生命周期
|-- router/          认证与用户路由
|-- gateway/         模型网关与计量
|-- packages/chronos/         调度与唤醒系统
|-- packages/vault/           加密与密钥边界
|-- packages/sandboxd/        受限工作区与预览
|-- protocol/skills/          可复用智能体能力
|-- protocol/tools/           原生工具桥接
|-- deploy/          服务与容器打包
|-- docs/            架构和运维文档
|-- protocol/spec/            功能规范的唯一事实来源
```

## 文档

| 资源 | 说明 |
| --- | --- |
| [架构](ARCHITECTURE.md) | 系统边界、运行时拓扑和设计约束。 |
| [品牌规范](BRANDING.md) | 品牌重命名期间保留的规范名称和兼容标识。 |
| [贡献指南](CONTRIBUTING.md) | 开发环境、质量关卡和贡献政策。 |
| [安全策略](SECURITY.md) | 漏洞报告与支持版本。 |
| [Centra AI 的构建方式](HOW_CENTRA_AI_WAS_BUILT.md) | 团队基于规范的开发方法。 |
| [变更日志](CHANGELOG.md) | 版本历史。 |
| [完整文档](docs/) | 模块、运行时、部署和运维文档。 |

## 许可证

Centra AI 根据 [Centra AI Protocol License](LICENSE.md) 以源码可用方式发布。

```text
Copyright © 2026 Sidiora Labs. All rights reserved.
SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
```

## 其他语言

- [英语](README.md)
- [西班牙语](README.es.md)
- [日语](README.ja.md)
- [葡萄牙语](README.pt-BR.md)
- [俄语](README.ru.md)

<p align="center">
  由 <a href="https://github.com/Sidiora-Labs"><strong>Sidiora Labs</strong></a> 构建
</p>
