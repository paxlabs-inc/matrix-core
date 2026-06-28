import { Doc, sortDottedIDs } from "./parser";
import type {
  CoverageSummary,
  GenericModel,
  KV,
  Model,
  Requirement,
  SpecModel,
  Task,
  WorkflowModel,
} from "./types";

/** Parse a .kvx document into the appropriate model for rendering. */
export function buildModel(text: string, fileName: string): Model {
  const doc = Doc.parse(text);
  if (doc.sectionsWithPrefix("task").length > 0 || doc.sectionsWithPrefix("req").length > 0) {
    return buildSpecModel(doc, fileName);
  }
  if (doc.has("loop") || doc.has("hard_rules") || doc.has("adapters")) {
    return buildWorkflowModel(doc, fileName);
  }
  return buildGenericModel(doc, fileName);
}

function buildSpecModel(doc: Doc, fileName: string): SpecModel {
  const reqIds = sortDottedIDs(doc.sectionsWithPrefix("req"));
  const requirements: Requirement[] = reqIds.map((id) => {
    const sec = "req." + id;
    const criteria = doc
      .orderedKV(sec, "ac_")
      .map(({ key, text }) => {
        const ord = key.slice("ac_".length);
        return { key, ord, ref: `${id}.${ord}`, text, coveredBy: [] as string[] };
      });
    return {
      id,
      title: doc.str(sec, "title"),
      story: doc.str(sec, "story"),
      criteria,
      line: doc.sectionLine[sec] ?? 0,
    };
  });

  const taskIds = sortDottedIDs(doc.sectionsWithPrefix("task"));
  const tasks: Task[] = taskIds.map((id) => {
    const sec = "task." + id;
    const validates = doc.str(sec, "validates");
    const depth = (id.match(/\./g) ?? []).length;
    return {
      id,
      title: doc.str(sec, "title"),
      status: doc.str(sec, "status") || "pending",
      wave: doc.raw(sec, "wave") === "" ? undefined : doc.uintOr(sec, "wave", 0),
      reqs: doc.list(sec, "reqs"),
      requires: doc.list(sec, "requires"),
      section: doc.str(sec, "section") || undefined,
      dos: doc.orderedKV(sec, "do_").map((kv) => kv.text),
      note: doc.str(sec, "note") || undefined,
      property: doc.str(sec, "property") || undefined,
      validates: validates || undefined,
      validatesRefs: mineRefs(validates),
      depth,
      parent: depth > 0 ? id.slice(0, id.lastIndexOf(".")) : undefined,
      line: doc.sectionLine[sec] ?? 0,
    };
  });

  const coverage = computeCoverage(requirements, tasks);

  const waves = Array.from(
    new Set(tasks.filter((t) => t.wave !== undefined).map((t) => t.wave as number)),
  ).sort((a, b) => a - b);

  return {
    kind: "spec",
    fileName,
    feature: doc.str("meta", "feature"),
    title: doc.str("meta", "title") || doc.str("meta", "feature"),
    status: doc.str("meta", "status"),
    intro: doc.str("meta", "intro"),
    requirements,
    tasks,
    waves,
    coverage,
  };
}

/** Mine "<n>.<m>" criterion references out of free-text (e.g. a validates clause). */
function mineRefs(text: string): string[] {
  const out = new Set<string>();
  for (const m of text.matchAll(/\b(\d+)\.(\d+)\b/g)) out.add(`${m[1]}.${m[2]}`);
  return [...out];
}

function computeCoverage(reqs: Requirement[], tasks: Task[]): CoverageSummary {
  const byRef = new Map<string, Requirement["criteria"][number]>();
  for (const r of reqs) for (const c of r.criteria) byRef.set(c.ref, c);

  const dangling: CoverageSummary["dangling"] = [];
  for (const t of tasks) {
    const refs = new Set([...t.reqs, ...t.validatesRefs]);
    for (const ref of refs) {
      const c = byRef.get(ref);
      if (c) {
        if (!c.coveredBy.includes(t.id)) c.coveredBy.push(t.id);
      } else if (/^\d+\.\d+$/.test(ref)) {
        dangling.push({ task: t.id, ref });
      }
    }
  }
  for (const c of byRef.values()) sortDottedIDs(c.coveredBy).forEach((_, i, a) => (c.coveredBy[i] = a[i]));

  const all = [...byRef.values()];
  const gaps = all.filter((c) => c.coveredBy.length === 0).map((c) => c.ref);
  return {
    totalCriteria: all.length,
    coveredCriteria: all.length - gaps.length,
    gaps,
    dangling,
  };
}

function buildWorkflowModel(doc: Doc, fileName: string): WorkflowModel {
  const kv = (section: string): KV[] => doc.orderedKV(section).map(({ key, text }) => ({ key, text }));
  const adapters = doc.orderedKV("adapters").map(({ key, text }) => ({
    label: key,
    path: text,
    line: doc.sectionLine["adapters"] ?? 0,
  }));
  return {
    kind: "workflow",
    fileName,
    name: doc.str("meta", "name"),
    sourceOfTruth: doc.str("meta", "source_of_truth"),
    activeFeature: doc.str("meta", "active_feature"),
    principles: kv("principle"),
    loop: kv("loop"),
    cortex: kv("cortex"),
    hardRules: kv("hard_rules"),
    adapters,
  };
}

function buildGenericModel(doc: Doc, fileName: string): GenericModel {
  return {
    kind: "generic",
    fileName,
    sections: doc.order.map((name) => ({
      name,
      line: doc.sectionLine[name] ?? 0,
      entries: doc.orderedKV(name).map(({ key, text }) => ({ key, text })),
    })),
  };
}
