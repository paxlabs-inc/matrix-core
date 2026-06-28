// Shared model types. The extension host parses a .kvx document into one of
// these models and posts it (as plain JSON) to the webview, which renders it.
// Types are erased at build time, so both bundles can import this file.

export type TaskStatus = "pending" | "in_progress" | "done" | string;

/** One acceptance criterion under a requirement, e.g. req.1 ac_3. */
export interface Criterion {
  /** The ac key, e.g. "ac_1". */
  key: string;
  /** The criterion ordinal as referenced by tasks, e.g. "1" for ac_1. */
  ord: string;
  /** Full "<reqId>.<ord>" reference, e.g. "1.3". */
  ref: string;
  /** The EARS clause text. */
  text: string;
  /** Task ids that reference this criterion (filled during derivation). */
  coveredBy: string[];
}

export interface Requirement {
  id: string; // "1"
  title: string;
  story: string;
  criteria: Criterion[];
  /** Source line (0-based) of the [req.<id>] header, for reveal-in-editor. */
  line: number;
}

export interface Task {
  id: string; // "1.1"
  title: string;
  status: TaskStatus;
  wave?: number;
  /** Structured criterion refs from `reqs = [...]`, e.g. ["1.1","16.2"]. */
  reqs: string[];
  /** Future: explicit dependency edges from `requires = [...]`. Empty today. */
  requires: string[];
  /** Top-level phase heading from `section`, present on group tasks only. */
  section?: string;
  /** Ordered do_* implementation bullets. */
  dos: string[];
  note?: string;
  property?: string;
  /** Free-text `validates` (property tests). Criterion refs are mined from it. */
  validates?: string;
  /** Criterion refs mined from `validates` prose, merged into coverage. */
  validatesRefs: string[];
  depth: number; // count of dots: 0 for "1", 1 for "1.1"
  parent?: string; // "1" for "1.1"
  line: number; // source line of [task.<id>] header
}

export interface CoverageSummary {
  totalCriteria: number;
  coveredCriteria: number;
  /** Criterion refs with no covering task. */
  gaps: string[];
  /** Task refs pointing at a requirement/criterion that does not exist. */
  dangling: { task: string; ref: string }[];
}

export interface KV {
  key: string;
  text: string;
}

export interface SpecModel {
  kind: "spec";
  fileName: string;
  feature: string;
  title: string;
  status: string;
  intro: string;
  requirements: Requirement[];
  tasks: Task[];
  /** Distinct wave numbers present among leaf tasks, ascending. */
  waves: number[];
  coverage: CoverageSummary;
}

export interface WorkflowModel {
  kind: "workflow";
  fileName: string;
  name: string;
  sourceOfTruth: string;
  activeFeature: string;
  principles: KV[];
  loop: KV[];
  cortex: KV[];
  hardRules: KV[];
  adapters: { label: string; path: string; line: number }[];
}

/** Fallback when a .kvx file matches neither known schema. */
export interface GenericModel {
  kind: "generic";
  fileName: string;
  sections: { name: string; line: number; entries: KV[] }[];
}

export type Model = SpecModel | WorkflowModel | GenericModel;

// Messages exchanged between the extension host and the webview.
export type HostToWebview = { type: "update"; model: Model };
export type WebviewToHost =
  | { type: "ready" }
  | { type: "reveal"; line: number };
