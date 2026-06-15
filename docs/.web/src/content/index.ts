// Cortex imports
import cortexIndex from './cortex-docs/INDEX.md?raw';
import cortexAttestAndCompact from './cortex-docs/attest-and-compact.md?raw';
import cortexContextBundle from './cortex-docs/context-bundle.md?raw';
import cortexEdgesAndGraph from './cortex-docs/edges-and-graph.md?raw';
import cortexEmbedderAndVector from './cortex-docs/embedder-and-vector.md?raw';
import cortexFindQuery from './cortex-docs/find-query.md?raw';
import cortexMemoryTaxonomy from './cortex-docs/memory-taxonomy.md?raw';
import cortexReplay from './cortex-docs/replay.md?raw';
import cortexSalience from './cortex-docs/salience.md?raw';
import cortexScope from './cortex-docs/scope.md?raw';
import cortexSnapshotAndProofs from './cortex-docs/snapshot-and-proofs.md?raw';
import cortexStoreAndJournal from './cortex-docs/store-and-journal.md?raw';
import cortexWriteApi from './cortex-docs/write-api.md?raw';

// Neo imports
import neoIndex from './neo-docs/INDEX.md?raw';
import neoConfigSystem from './neo-docs/config-system.md?raw';
import neoControlLoop from './neo-docs/control-loop.md?raw';
import neoConversationStore from './neo-docs/conversation-store.md?raw';
import neoCoreExecute from './neo-docs/core-execute.md?raw';
import neoLlmClient from './neo-docs/llm-client.md?raw';
import neoMemorySystem from './neo-docs/memory-system.md?raw';
import neoRecallLane from './neo-docs/recall-lane.md?raw';
import neoToolSurface from './neo-docs/tool-surface.md?raw';
import neoWritebackConsolidation from './neo-docs/writeback-consolidation.md?raw';

// Chronos imports
import chronosIndex from './chronos-docs/INDEX.md?raw';
import chronosApiReference from './chronos-docs/api-reference.md?raw';
import chronosArchitecture from './chronos-docs/architecture.md?raw';
import chronosAuthSystem from './chronos-docs/auth-system.md?raw';
import chronosConfigSystem from './chronos-docs/config-system.md?raw';
import chronosDataModel from './chronos-docs/data-model.md?raw';
import chronosDispatchWorker from './chronos-docs/dispatch-worker.md?raw';
import chronosScheduleEngine from './chronos-docs/schedule-engine.md?raw';
import chronosToolSurface from './chronos-docs/tool-surface.md?raw';
import chronosWakeDelivery from './chronos-docs/wake-delivery.md?raw';

// Tachyon imports
import tachyonIndex from './tachyon-docs/index.md?raw';
import tachyonAbiEncoder from './tachyon-docs/abi-encoder.md?raw';
import tachyonApiServer from './tachyon-docs/api-server.md?raw';
import tachyonChains from './tachyon-docs/chains.md?raw';
import tachyonCompiler from './tachyon-docs/compiler.md?raw';
import tachyonConfig from './tachyon-docs/config.md?raw';
import tachyonDaemon from './tachyon-docs/daemon.md?raw';
import tachyonDeployer from './tachyon-docs/deployer.md?raw';
import tachyonEngine from './tachyon-docs/engine.md?raw';
import tachyonEvmClient from './tachyon-docs/evm-client.md?raw';
import tachyonMcpServer from './tachyon-docs/mcp-server.md?raw';
import tachyonRegistry from './tachyon-docs/registry.md?raw';
import tachyonRpcServer from './tachyon-docs/rpc-server.md?raw';
import tachyonSimulate from './tachyon-docs/simulate.md?raw';
import tachyonTester from './tachyon-docs/tester.md?raw';
import tachyonTypes from './tachyon-docs/types.md?raw';
import tachyonWallet from './tachyon-docs/wallet.md?raw';

// MCL imports
import mclIndex from './MCL-docs/index.md?raw';
import mclCompilerPipeline from './MCL-docs/compiler-pipeline.md?raw';
import mclEnvelope from './MCL-docs/envelope.md?raw';
import mclIntentIr from './MCL-docs/intent-ir.md?raw';
import mclLlmClient from './MCL-docs/llm-client.md?raw';
import mclMatrixscript from './MCL-docs/matrixscript.md?raw';
import mclMclcCli from './MCL-docs/mclc-cli.md?raw';
import mclSkillAuthoring from './MCL-docs/skill-authoring.md?raw';

// Single-page domain imports
import routerIndex from './router-docs/INDEX.md?raw';
import bridgeIndex from './bridge-docs/INDEX.md?raw';
import uwacIndex from './uwac-docs/INDEX.md?raw';
import executorIndex from './executor-docs/INDEX.md?raw';
import agentsIndex from './agents-docs/INDEX.md?raw';
import deusIndex from './deus-docs/INDEX.md?raw';
import gatewayIndex from './gateway-docs/INDEX.md?raw';
import rulesIndex from './rules-docs/INDEX.md?raw';
import skillsIndex from './skills-docs/INDEX.md?raw';
import deployIndex from './deploy-docs/INDEX.md?raw';
import toolsIndex from './tools-docs/INDEX.md?raw';

// ── Types ────────────────────────────────────────────────────────────────────
export interface DocPage {
  slug: string;
  title: string;
  content: string;
}

export interface DomainGroup {
  id: string;
  label: string;
  pages: DocPage[];
}

// ── Page arrays per domain ─────────────────────────────────────────────────
const cortexPages: DocPage[] = [
  { slug: 'INDEX', title: 'Overview', content: cortexIndex },
  { slug: 'memory-taxonomy', title: 'Memory Taxonomy', content: cortexMemoryTaxonomy },
  { slug: 'store-and-journal', title: 'Store and Journal', content: cortexStoreAndJournal },
  { slug: 'edges-and-graph', title: 'Edges and Graph', content: cortexEdgesAndGraph },
  { slug: 'embedder-and-vector', title: 'Embedder and Vector', content: cortexEmbedderAndVector },
  { slug: 'write-api', title: 'Write API', content: cortexWriteApi },
  { slug: 'find-query', title: 'Find Query', content: cortexFindQuery },
  { slug: 'context-bundle', title: 'Context Bundle', content: cortexContextBundle },
  { slug: 'scope', title: 'Scope', content: cortexScope },
  { slug: 'salience', title: 'Salience', content: cortexSalience },
  { slug: 'snapshot-and-proofs', title: 'Snapshot and Proofs', content: cortexSnapshotAndProofs },
  { slug: 'attest-and-compact', title: 'Attest and Compact', content: cortexAttestAndCompact },
  { slug: 'replay', title: 'Replay', content: cortexReplay },
];

const neoPages: DocPage[] = [
  { slug: 'INDEX', title: 'Overview', content: neoIndex },
  { slug: 'core-execute', title: 'Core Execute', content: neoCoreExecute },
  { slug: 'control-loop', title: 'Control Loop', content: neoControlLoop },
  { slug: 'conversation-store', title: 'Conversation Store', content: neoConversationStore },
  { slug: 'memory-system', title: 'Memory System', content: neoMemorySystem },
  { slug: 'recall-lane', title: 'Recall Lane', content: neoRecallLane },
  { slug: 'tool-surface', title: 'Tool Surface', content: neoToolSurface },
  { slug: 'llm-client', title: 'LLM Client', content: neoLlmClient },
  { slug: 'writeback-consolidation', title: 'Writeback Consolidation', content: neoWritebackConsolidation },
  { slug: 'config-system', title: 'Config System', content: neoConfigSystem },
];

const chronosPages: DocPage[] = [
  { slug: 'INDEX', title: 'Overview', content: chronosIndex },
  { slug: 'architecture', title: 'Architecture', content: chronosArchitecture },
  { slug: 'data-model', title: 'Data Model', content: chronosDataModel },
  { slug: 'schedule-engine', title: 'Schedule Engine', content: chronosScheduleEngine },
  { slug: 'dispatch-worker', title: 'Dispatch Worker', content: chronosDispatchWorker },
  { slug: 'wake-delivery', title: 'Wake Delivery', content: chronosWakeDelivery },
  { slug: 'tool-surface', title: 'Tool Surface', content: chronosToolSurface },
  { slug: 'auth-system', title: 'Auth System', content: chronosAuthSystem },
  { slug: 'config-system', title: 'Config System', content: chronosConfigSystem },
  { slug: 'api-reference', title: 'API Reference', content: chronosApiReference },
];

const tachyonPages: DocPage[] = [
  { slug: 'index', title: 'Overview', content: tachyonIndex },
  { slug: 'engine', title: 'Engine', content: tachyonEngine },
  { slug: 'evm-client', title: 'EVM Client', content: tachyonEvmClient },
  { slug: 'compiler', title: 'Compiler', content: tachyonCompiler },
  { slug: 'deployer', title: 'Deployer', content: tachyonDeployer },
  { slug: 'tester', title: 'Tester', content: tachyonTester },
  { slug: 'abi-encoder', title: 'ABI Encoder', content: tachyonAbiEncoder },
  { slug: 'wallet', title: 'Wallet', content: tachyonWallet },
  { slug: 'chains', title: 'Chains', content: tachyonChains },
  { slug: 'registry', title: 'Registry', content: tachyonRegistry },
  { slug: 'types', title: 'Types', content: tachyonTypes },
  { slug: 'api-server', title: 'API Server', content: tachyonApiServer },
  { slug: 'rpc-server', title: 'RPC Server', content: tachyonRpcServer },
  { slug: 'mcp-server', title: 'MCP Server', content: tachyonMcpServer },
  { slug: 'simulate', title: 'Simulate', content: tachyonSimulate },
  { slug: 'config', title: 'Config', content: tachyonConfig },
  { slug: 'daemon', title: 'Daemon', content: tachyonDaemon },
];

const mclPages: DocPage[] = [
  { slug: 'index', title: 'Overview', content: mclIndex },
  { slug: 'matrixscript', title: 'MatrixScript', content: mclMatrixscript },
  { slug: 'intent-ir', title: 'Intent IR', content: mclIntentIr },
  { slug: 'compiler-pipeline', title: 'Compiler Pipeline', content: mclCompilerPipeline },
  { slug: 'envelope', title: 'Envelope', content: mclEnvelope },
  { slug: 'llm-client', title: 'LLM Client', content: mclLlmClient },
  { slug: 'mclc-cli', title: 'MCLC CLI', content: mclMclcCli },
  { slug: 'skill-authoring', title: 'Skill Authoring', content: mclSkillAuthoring },
];

// ── Domain groups ───────────────────────────────────────────────────────────
export const docsNav: DomainGroup[] = [
  { id: 'cortex', label: 'Cortex', pages: cortexPages },
  { id: 'neo', label: 'Neo', pages: neoPages },
  { id: 'chronos', label: 'Chronos', pages: chronosPages },
  { id: 'tachyon', label: 'Tachyon', pages: tachyonPages },
  { id: 'mcl', label: 'MCL', pages: mclPages },
  { id: 'router', label: 'Router', pages: [{ slug: 'INDEX', title: 'Overview', content: routerIndex }] },
  { id: 'bridge', label: 'Bridge', pages: [{ slug: 'INDEX', title: 'Overview', content: bridgeIndex }] },
  { id: 'uwac', label: 'UWAC', pages: [{ slug: 'INDEX', title: 'Overview', content: uwacIndex }] },
  { id: 'executor', label: 'Executor', pages: [{ slug: 'INDEX', title: 'Overview', content: executorIndex }] },
  { id: 'agents', label: 'Agents', pages: [{ slug: 'INDEX', title: 'Overview', content: agentsIndex }] },
  { id: 'deus', label: 'Deus', pages: [{ slug: 'INDEX', title: 'Overview', content: deusIndex }] },
  { id: 'gateway', label: 'Gateway', pages: [{ slug: 'INDEX', title: 'Overview', content: gatewayIndex }] },
  { id: 'rules', label: 'Rules', pages: [{ slug: 'INDEX', title: 'Overview', content: rulesIndex }] },
  { id: 'skills', label: 'Skills', pages: [{ slug: 'INDEX', title: 'Overview', content: skillsIndex }] },
  { id: 'deploy', label: 'Deploy', pages: [{ slug: 'INDEX', title: 'Overview', content: deployIndex }] },
  { id: 'tools', label: 'Tools', pages: [{ slug: 'INDEX', title: 'Overview', content: toolsIndex }] },
];

// ── Flat array for search ──────────────────────────────────────────────────
export const allPages: { domain: string; domainLabel: string; slug: string; title: string; content: string }[] = [];
for (const group of docsNav) {
  for (const page of group.pages) {
    allPages.push({
      domain: group.id,
      domainLabel: group.label,
      slug: page.slug,
      title: page.title,
      content: page.content,
    });
  }
}

// ── Helper functions ───────────────────────────────────────────────────────
export function getPage(domain: string, slug?: string): DocPage | undefined {
  const group = docsNav.find((g) => g.id === domain);
  if (!group) return undefined;
  const targetSlug = slug || (group.pages[0]?.slug ?? 'INDEX');
  return group.pages.find((p) => p.slug === targetSlug);
}

export function getDomainPages(domain: string): DocPage[] | undefined {
  return docsNav.find((g) => g.id === domain)?.pages;
}

export function getAdjacentPage(domain: string, slug: string): { prev?: DocPage; next?: DocPage } {
  const pages = getDomainPages(domain);
  if (!pages) return {};
  const idx = pages.findIndex((p) => p.slug === slug);
  if (idx === -1) return {};
  return {
    prev: idx > 0 ? pages[idx - 1] : undefined,
    next: idx < pages.length - 1 ? pages[idx + 1] : undefined,
  };
}
