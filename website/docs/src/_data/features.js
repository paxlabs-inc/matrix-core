export default [
  // Core Features
  {
    title: "Closed Vocabulary",
    description:
      "10 verbs, 8 object kinds. Intent maps to a typed AST — no open-ended classification, no ambiguity.",
    icon: "book-text",
    link: "/architecture/",
    category: "core",
  },
  {
    title: "Intent IR Compiler",
    description:
      "Natural language compiles to typed, inspectable Intent IR. Meaning preserved through every stage.",
    icon: "code-2",
    link: "/architecture/",
    category: "core",
  },
  {
    title: "Deterministic Walks",
    description:
      "Replayable execution with byte-deterministic state. Every step is journaled and auditable.",
    icon: "route",
    link: "/architecture/",
    category: "core",
  },
  {
    title: "Four Failure Mode Solution",
    description:
      "Eliminates prompt fragility, intent loss, missing ontology, and no structured correction.",
    icon: "shield-check",
    link: "/features/",
    category: "core",
  },
  {
    title: "Cortex Memory",
    description:
      "Per-actor typed memory graph with Merkle-anchored snapshots and append-only journal.",
    icon: "brain",
    link: "/architecture/",
    category: "core",
  },
  {
    title: "Two Rails Architecture",
    description:
      "Neo for conversation, MCL for rigor. Switch seamlessly based on task stakes.",
    icon: "git-branch",
    link: "/architecture/",
    category: "core",
  },

  // Developer Features
  {
    title: "MCP Tool Dispatch",
    description:
      "Standard MCP protocol for tool invocation. Connect any service as an agent capability.",
    icon: "plug",
    link: "/developers/",
    category: "developer",
  },
  {
    title: "SKILL Manifests",
    description:
      "Typed capability declarations that agents discover and compose automatically.",
    icon: "file-code-2",
    link: "/developers/",
    category: "developer",
  },
  {
    title: "CLI & SDK",
    description:
      "Compile intents, run walks, and start daemons from the command line or programmatically.",
    icon: "terminal",
    link: "/developers/",
    category: "developer",
  },
  {
    title: "Open Source Core",
    description:
      "Fork, modify, and deploy. MIT-compatible for most uses with transparent governance.",
    icon: "github",
    link: "/developers/",
    category: "developer",
  },

  // Enterprise Features
  {
    title: "DID-Bound Agents",
    description:
      "Cryptographic agent identity with verifiable credentials and attestation chains.",
    icon: "fingerprint",
    link: "/enterprise/",
    category: "enterprise",
  },
  {
    title: "On-Chain Settlement",
    description:
      "EIP-712 receipts, PAX credit ledger, and transparent metered billing.",
    icon: "landmark",
    link: "/enterprise/",
    category: "enterprise",
  },
  {
    title: "Paxeer Network",
    description:
      "Purpose-built L1 with 400ms block time and finality. Optimized for agentic workloads.",
    icon: "network",
    link: "/enterprise/",
    category: "enterprise",
  },
  {
    title: "Multi-Tenant Isolation",
    description:
      "Per-user daemon provisioning with wake-then-proxy architecture.",
    icon: "server",
    link: "/enterprise/",
    category: "enterprise",
  },
];
