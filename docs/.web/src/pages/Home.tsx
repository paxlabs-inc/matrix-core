import { Rocket, Layers, GitBranch, Bot, Brain, Clock, FileCode, MessageSquare, Globe, Shield, Zap, BookOpen } from 'lucide-react';
import { docsNav } from '@/content';

function StartCard({
  icon,
  title,
  description,
}: {
  icon: React.ReactNode;
  title: string;
  description: string;
}) {
  return (
    <a
      href="#/docs/cortex/INDEX"
      className="block bg-bg-surface border border-border-subtle rounded-docs p-5 hover:border-border-bright hover:bg-bg-surface-hover transition"
    >
      <div className="flex items-center gap-2 mb-2">
        <span className="text-fg-primary">{icon}</span>
        <span className="text-fg-primary font-semibold text-docs-base">{title}</span>
      </div>
      <p className="text-fg-secondary text-docs-sm">{description}</p>
    </a>
  );
}

function FeatureCard({ title, description }: { title: string; description: string }) {
  return (
    <div className="bg-bg-surface border border-border-subtle rounded-docs p-5">
      <div className="text-fg-primary font-semibold text-docs-base mb-1">{title}</div>
      <p className="text-fg-secondary text-docs-sm">{description}</p>
    </div>
  );
}

function ResourceCard({
  icon,
  title,
  description,
}: {
  icon: React.ReactNode;
  title: string;
  description: string;
}) {
  return (
    <a
      href="#/"
      className="block bg-bg-surface border border-border-subtle rounded-docs p-5 hover:border-border-bright hover:bg-bg-surface-hover transition"
    >
      <div className="flex items-center gap-2 mb-2">
        <span className="text-fg-muted">{icon}</span>
        <span className="text-fg-primary font-medium text-docs-sm">{title}</span>
      </div>
      <p className="text-fg-secondary text-docs-sm">{description}</p>
    </a>
  );
}

export default function Home() {
  return (
    <div>
      {/* Breadcrumb */}
      <div className="text-docs-xs text-fg-muted mb-4">Get Started</div>

      {/* Hero */}
      <h1 className="text-docs-3xl font-bold text-fg-primary mb-4 tracking-tight">
        Matrix Documentation
      </h1>
      <p className="text-docs-base text-fg-secondary max-w-2xl mb-10 leading-relaxed">
        Matrix is an AI Agent Operating System. Use it to build autonomous agents, manage memory,
        schedule tasks, and deploy smart contracts — all with deterministic, verifiable execution.
      </p>

      {/* Hero illustration placeholder */}
      <div className="w-full h-[240px] bg-bg-surface border border-border-subtle rounded-docs mb-12 flex items-center justify-center overflow-hidden">
        <img
          src="/hero-illustration.png"
          alt="Matrix AI Operating System"
          className="w-full h-full object-cover"
          onError={(e) => {
            (e.target as HTMLImageElement).style.display = 'none';
          }}
        />
      </div>

      {/* Start here */}
      <h2 className="text-docs-xl font-semibold text-fg-primary mb-4">Start here</h2>
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-12">
        <StartCard
          icon={<Rocket size={18} />}
          title="Get started"
          description="Go from install to your first useful agent in Matrix"
        />
        <StartCard
          icon={<Layers size={18} />}
          title="Architecture"
          description="Understand how the system works and how components interact"
        />
        <StartCard
          icon={<GitBranch size={18} />}
          title="Changelog"
          description="Stay up to date with the latest features and improvements"
        />
      </div>

      {/* What you can do */}
      <h2 className="text-docs-xl font-semibold text-fg-primary mb-4">
        What you can do with Matrix
      </h2>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 mb-12">
        <FeatureCard
          title="Build agents"
          description="Create autonomous AI agents with memory, tools, and deterministic execution"
        />
        <FeatureCard
          title="Manage memory"
          description="Cortex provides persistent, queryable memory with vector search"
        />
        <FeatureCard
          title="Schedule tasks"
          description="Chronos handles time-based wake-ups and recurring agent tasks"
        />
        <FeatureCard
          title="Deploy contracts"
          description="Tachyon provides a complete Solidity/EVM toolbox over HTTP and MCP"
        />
        <FeatureCard
          title="Compile intents"
          description="MCL transforms natural language into typed, signed Intent IR"
        />
        <FeatureCard
          title="Connect everything"
          description="Router, Bridge, and Gateway wire agents to the external world"
        />
      </div>

      {/* Components table */}
      <h2 className="text-docs-xl font-semibold text-fg-primary mb-4">Components</h2>
      <div className="overflow-x-auto mb-12">
        <table className="w-full text-left text-docs-sm border-collapse">
          <thead className="border-b border-border">
            <tr>
              <th className="py-2.5 px-3 font-medium text-fg-secondary">Component</th>
              <th className="py-2.5 px-3 font-medium text-fg-secondary">Description</th>
              <th className="py-2.5 px-3 font-medium text-fg-secondary">Pages</th>
            </tr>
          </thead>
          <tbody>
            {docsNav.map((group) => (
              <tr
                key={group.id}
                className="border-b border-border-subtle hover:bg-bg-surface-hover transition-colors"
              >
                <td className="py-2.5 px-3">
                  <a
                    href={`#/docs/${group.id}/${group.pages[0]?.slug ?? 'INDEX'}`}
                    className="text-fg-primary underline underline-offset-4 decoration-border-bright hover:decoration-fg-secondary transition-colors font-medium"
                  >
                    {group.label}
                  </a>
                </td>
                <td className="py-2.5 px-3 text-fg-secondary">
                  {group.id === 'cortex' && 'Persistent memory system with vector search'}
                  {group.id === 'neo' && 'Agent execution engine with LLM integration'}
                  {group.id === 'chronos' && 'Task scheduling and wake-up delivery'}
                  {group.id === 'tachyon' && 'Solidity/EVM smart contract deployment'}
                  {group.id === 'mcl' && 'Matrix Composable Language for intents'}
                  {group.id === 'router' && 'Message routing between agents and services'}
                  {group.id === 'bridge' && 'Cross-chain bridge operations'}
                  {group.id === 'uwac' && 'Universal WebAssembly Contract runtime'}
                  {group.id === 'executor' && 'Deterministic transaction execution'}
                  {group.id === 'agents' && 'Agent management and orchestration'}
                  {group.id === 'deus' && 'Decentralized execution and consensus'}
                  {group.id === 'gateway' && 'External API gateway for agents'}
                  {group.id === 'rules' && 'Rule engine for agent behavior'}
                  {group.id === 'skills' && 'Skill registry and discovery'}
                  {group.id === 'deploy' && 'Deployment management'}
                  {group.id === 'tools' && 'Tool registry for agents'}
                </td>
                <td className="py-2.5 px-3 text-fg-muted">{group.pages.length}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* More resources */}
      <h2 className="text-docs-xl font-semibold text-fg-primary mb-4">More resources</h2>
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <ResourceCard
          icon={<BookOpen size={16} />}
          title="Downloads"
          description="Get the latest Matrix CLI and tools"
        />
        <ResourceCard
          icon={<MessageSquare size={16} />}
          title="Help"
          description="FAQs, troubleshooting, and support"
        />
        <ResourceCard
          icon={<Shield size={16} />}
          title="Support"
          description="Contact the team for enterprise support"
        />
      </div>
    </div>
  );
}
