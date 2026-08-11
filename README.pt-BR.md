<p align="center">
  <img src="https://pub-cc719bc237f94810bec78e93e056bec4.r2.dev/centra.ai_wordmark_dark.png" alt="Centra AI" width="520" />
</p>


<p align="center">
  <strong>Um sistema operacional de IA com estado para trabalho sério e de longa duração.</strong>
</p>

<p align="center">
  Centra AI oferece aos agentes memória, ferramentas, computadores isolados,
  trabalho coordenado e um registro durável do que realmente fizeram.
</p>

<p align="center">
  <a href="https://github.com/Sidiora-Labs/centra-llm-agents">Repositório</a> ·
  <a href="README.md">English</a> ·
  <a href="LICENSE.md">Licença Centra AI Protocol</a>
</p>

## O que é Centra AI?

Centra AI é uma plataforma privada e persistente de agentes criada pela Sidiora Labs. Ela foi projetada para trabalhos que não cabem com confiabilidade em um único prompt: construir software, investigar questões complexas, operar navegadores e arquivos, coordenar especialistas em paralelo, produzir artefatos e continuar entre sessões sem perder o estado do trabalho.

O sistema é centrado em dois agentes:

- **Neo** é o agente principal. Ele pesquisa, raciocina, opera ferramentas, cria artefatos, gerencia trabalhos ativos e permanece com uma tarefa além de uma única resposta do modelo.
- **Ion** é o agente técnico e o ambiente de programação. Ele trabalha diretamente com projetos reais, terminais, arquivos, testes, previews e toolchains dentro de um workspace limitado.

Eles são apoiados pelo **Workforce**, a camada de execução coordenada que divide grandes objetivos em trabalho paralelo governado, e pelo **Neo Computer**, o local unificado para fontes, previews, artefatos, alterações e evidências do workspace.

A conversa é a superfície de controle; o produto é o sistema de trabalho por trás dela.

## O que torna o sistema diferente

- **Cognição persistente.** Identidade, memória, objetivos, evidências e trabalho pendente sobrevivem a reinicializações e mudanças de janela de contexto.
- **Ambientes de execução reais.** Os agentes trabalham com repositórios, terminais, navegadores, serviços, bancos de dados, ferramentas de mídia e sistemas externos.
- **Evidência em vez de aparência.** Fontes, citações, resultados de ferramentas, artefatos, checkpoints e verificações permanecem ligados ao trabalho.
- **Trabalho de longa duração.** Tarefas podem ser decompostas, enfileiradas, corrigidas, retomadas e continuadas por especialistas limitados.
- **Controle humano.** Operações sensíveis ou com efeitos externos atravessam limites explícitos de autoridade e aprovação.
- **Isolamento por usuário.** Cada usuário recebe um ambiente de agente e um limite de estado durável dedicados.

## Capacidades principais

| Área | Capacidades |
| --- | --- |
| Engenharia de software | Programação consciente do repositório, shell, pacotes, testes, serviços, diffs, previews e entrega de projetos. |
| Pesquisa | Investigação em múltiplas fontes, citações exatas, trechos, síntese e artefatos duráveis. |
| Uso do computador | Operação de navegador e desktop com evidência visível e autoridade limitada. |
| Memória | Identidade, preferências, fatos, objetivos, continuidade e crenças ligadas a evidências. |
| Trabalho coordenado | Decomposição, especialistas, paralelismo limitado, supervisão e retomada. |
| Automação | Trabalho agendado, execução sob demanda, filas duráveis e entregas proativas. |

## Início rápido

```bash
git clone https://github.com/Sidiora-Labs/centra-llm-agents.git
cd centra-llm-agents
make build
make install
```

Cliente:

```bash
cd client
corepack enable
pnpm install
pnpm dev
```

## Documentação

- [Arquitetura](ARCHITECTURE.md)
- [Contrato de marca e compatibilidade](BRANDING.md)
- [Contribuição](CONTRIBUTING.md)
- [Segurança](SECURITY.md)
- [Como Centra AI é construída](HOW_CENTRA_AI_WAS_BUILT.md)
- [Documentação completa](docs/)

## Licença

Centra AI está disponível sob a [Licença Centra AI Protocol](LICENSE.md).

```text
Copyright © 2026 Sidiora Labs. All rights reserved.
SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
```
