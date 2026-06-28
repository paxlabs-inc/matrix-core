"use strict";

// tools/inspect.ts
var import_node_fs = require("node:fs");

// src/kvx/parser.ts
var Doc = class _Doc {
  constructor() {
    /** Section names in file order. */
    this.order = [];
    /** section -> keys in file order. */
    this.keyOrder = {};
    /** section -> key -> raw (un-unquoted) value. */
    this.sections = {};
    /** section -> 0-based line of its `[header]`. */
    this.sectionLine = {};
  }
  ensure(section, line) {
    if (!(section in this.sections)) {
      this.sections[section] = {};
      this.keyOrder[section] = [];
      this.order.push(section);
      this.sectionLine[section] = line;
    }
  }
  static parse(text) {
    const doc = new _Doc();
    const lines = text.split(/\r?\n/);
    let section = "";
    for (let i = 0; i < lines.length; i++) {
      const line = stripComment(lines[i].trim());
      if (line === "") continue;
      if (line.startsWith("[")) {
        if (!line.endsWith("]")) continue;
        section = line.slice(1, -1).trim();
        doc.ensure(section, i);
        continue;
      }
      const eq = line.indexOf("=");
      if (eq === -1) continue;
      const key = line.slice(0, eq).trim();
      if (key === "") continue;
      doc.ensure(section, i);
      if (!(key in doc.sections[section])) doc.keyOrder[section].push(key);
      doc.sections[section][key] = line.slice(eq + 1).trim();
    }
    return doc;
  }
  has(section) {
    return section in this.sections;
  }
  /** Raw, un-unquoted token (so callers can tell a list from a scalar). */
  raw(section, key) {
    return this.sections[section]?.[key] ?? "";
  }
  /** Interpolated, unquoted scalar value, or "". */
  str(section, key) {
    const raw = this.sections[section]?.[key];
    if (raw === void 0) return "";
    return interpolate(unquote(raw));
  }
  isList(section, key) {
    const raw = this.raw(section, key).trim();
    return raw.startsWith("[") && raw.endsWith("]");
  }
  /** A bracketed list as interpolated strings (a bare scalar yields [scalar]). */
  list(section, key) {
    let raw = this.sections[section]?.[key];
    if (raw === void 0) return [];
    raw = raw.trim();
    if (!raw.startsWith("[") || !raw.endsWith("]")) {
      const v = interpolate(unquote(raw));
      return v ? [v] : [];
    }
    const inner = raw.slice(1, -1).trim();
    if (inner === "") return [];
    return splitList(inner).map((p) => interpolate(unquote(p.trim()))).filter((v) => v !== "");
  }
  keys(section) {
    return this.keyOrder[section] ?? [];
  }
  /** (key, interpolated value) pairs in file order, optionally prefix-filtered. */
  orderedKV(section, prefix = "") {
    const out = [];
    for (const k of this.keyOrder[section] ?? []) {
      if (prefix && !k.startsWith(prefix)) continue;
      out.push({ key: k, text: interpolate(unquote(this.sections[section][k])) });
    }
    return out;
  }
  /** Sub-section names under "prefix." (e.g. "req.1" -> "1") in file order. */
  sectionsWithPrefix(prefix) {
    const p = prefix + ".";
    return this.order.filter((s) => s.startsWith(p)).map((s) => s.slice(p.length));
  }
  uintOr(section, key, fallback) {
    const v = this.str(section, key);
    if (v === "") return fallback;
    const n = Number.parseInt(v, 10);
    return Number.isNaN(n) ? fallback : n;
  }
};
function stripComment(line) {
  let inQuote = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (c === '"') inQuote = !inQuote;
    else if (c === "#" && !inQuote) return line.slice(0, i).trim();
  }
  return line;
}
function splitList(s) {
  const parts = [];
  let inQuote = false;
  let start = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (c === '"') inQuote = !inQuote;
    else if (c === "," && !inQuote) {
      parts.push(s.slice(start, i));
      start = i + 1;
    }
  }
  parts.push(s.slice(start));
  return parts;
}
function unquote(s) {
  s = s.trim();
  if (s.length >= 2 && s.startsWith('"') && s.endsWith('"')) return s.slice(1, -1);
  return s;
}
var ENV_REF = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
function interpolate(s) {
  if (!s.includes("${")) return s;
  return s.replace(ENV_REF, (_m, name) => process.env?.[name] ?? "");
}
function sortDottedIDs(ids) {
  return [...ids].sort((a, b) => {
    const as = a.split(".");
    const bs = b.split(".");
    const n = Math.min(as.length, bs.length);
    for (let i = 0; i < n; i++) {
      const ai = Number.parseInt(as[i], 10);
      const bi = Number.parseInt(bs[i], 10);
      if (!Number.isNaN(ai) && !Number.isNaN(bi)) {
        if (ai !== bi) return ai - bi;
        continue;
      }
      if (as[i] !== bs[i]) return as[i] < bs[i] ? -1 : 1;
    }
    return as.length - bs.length;
  });
}

// src/kvx/model.ts
function buildModel(text, fileName) {
  const doc = Doc.parse(text);
  if (doc.sectionsWithPrefix("task").length > 0 || doc.sectionsWithPrefix("req").length > 0) {
    return buildSpecModel(doc, fileName);
  }
  if (doc.has("loop") || doc.has("hard_rules") || doc.has("adapters")) {
    return buildWorkflowModel(doc, fileName);
  }
  return buildGenericModel(doc, fileName);
}
function buildSpecModel(doc, fileName) {
  const reqIds = sortDottedIDs(doc.sectionsWithPrefix("req"));
  const requirements = reqIds.map((id) => {
    const sec = "req." + id;
    const criteria = doc.orderedKV(sec, "ac_").map(({ key, text }) => {
      const ord = key.slice("ac_".length);
      return { key, ord, ref: `${id}.${ord}`, text, coveredBy: [] };
    });
    return {
      id,
      title: doc.str(sec, "title"),
      story: doc.str(sec, "story"),
      criteria,
      line: doc.sectionLine[sec] ?? 0
    };
  });
  const taskIds = sortDottedIDs(doc.sectionsWithPrefix("task"));
  const tasks = taskIds.map((id) => {
    const sec = "task." + id;
    const validates = doc.str(sec, "validates");
    const depth = (id.match(/\./g) ?? []).length;
    return {
      id,
      title: doc.str(sec, "title"),
      status: doc.str(sec, "status") || "pending",
      wave: doc.raw(sec, "wave") === "" ? void 0 : doc.uintOr(sec, "wave", 0),
      reqs: doc.list(sec, "reqs"),
      requires: doc.list(sec, "requires"),
      section: doc.str(sec, "section") || void 0,
      dos: doc.orderedKV(sec, "do_").map((kv) => kv.text),
      note: doc.str(sec, "note") || void 0,
      property: doc.str(sec, "property") || void 0,
      validates: validates || void 0,
      validatesRefs: mineRefs(validates),
      depth,
      parent: depth > 0 ? id.slice(0, id.lastIndexOf(".")) : void 0,
      line: doc.sectionLine[sec] ?? 0
    };
  });
  const coverage = computeCoverage(requirements, tasks);
  const waves = Array.from(
    new Set(tasks.filter((t) => t.wave !== void 0).map((t) => t.wave))
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
    coverage
  };
}
function mineRefs(text) {
  const out = /* @__PURE__ */ new Set();
  for (const m of text.matchAll(/\b(\d+)\.(\d+)\b/g)) out.add(`${m[1]}.${m[2]}`);
  return [...out];
}
function computeCoverage(reqs, tasks) {
  const byRef = /* @__PURE__ */ new Map();
  for (const r of reqs) for (const c of r.criteria) byRef.set(c.ref, c);
  const dangling = [];
  for (const t of tasks) {
    const refs = /* @__PURE__ */ new Set([...t.reqs, ...t.validatesRefs]);
    for (const ref of refs) {
      const c = byRef.get(ref);
      if (c) {
        if (!c.coveredBy.includes(t.id)) c.coveredBy.push(t.id);
      } else if (/^\d+\.\d+$/.test(ref)) {
        dangling.push({ task: t.id, ref });
      }
    }
  }
  for (const c of byRef.values()) sortDottedIDs(c.coveredBy).forEach((_, i, a) => c.coveredBy[i] = a[i]);
  const all = [...byRef.values()];
  const gaps = all.filter((c) => c.coveredBy.length === 0).map((c) => c.ref);
  return {
    totalCriteria: all.length,
    coveredCriteria: all.length - gaps.length,
    gaps,
    dangling
  };
}
function buildWorkflowModel(doc, fileName) {
  const kv = (section) => doc.orderedKV(section).map(({ key, text }) => ({ key, text }));
  const adapters = doc.orderedKV("adapters").map(({ key, text }) => ({
    label: key,
    path: text,
    line: doc.sectionLine["adapters"] ?? 0
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
    adapters
  };
}
function buildGenericModel(doc, fileName) {
  return {
    kind: "generic",
    fileName,
    sections: doc.order.map((name) => ({
      name,
      line: doc.sectionLine[name] ?? 0,
      entries: doc.orderedKV(name).map(({ key, text }) => ({ key, text }))
    }))
  };
}

// tools/inspect.ts
var path = process.argv[2];
if (!path) {
  console.error("usage: npm run inspect -- <file.kvx>");
  process.exit(1);
}
var model = buildModel((0, import_node_fs.readFileSync)(path, "utf8"), path);
if (model.kind === "spec") {
  console.log(`spec: ${model.feature} (${model.status})`);
  console.log(`  requirements=${model.requirements.length} tasks=${model.tasks.length} waves=[${model.waves.join(",")}]`);
  const c = model.coverage;
  console.log(`  coverage: ${c.coveredCriteria}/${c.totalCriteria} criteria covered`);
  if (c.gaps.length) console.log(`  gaps (${c.gaps.length}): ${c.gaps.join(", ")}`);
  if (c.dangling.length) console.log(`  dangling: ${c.dangling.map((d) => `${d.task}->${d.ref}`).join(", ")}`);
} else if (model.kind === "workflow") {
  console.log(`workflow: ${model.name}`);
  console.log(`  loop=${model.loop.length} principles=${model.principles.length} hardRules=${model.hardRules.length} adapters=${model.adapters.length}`);
} else {
  console.log(`generic: ${model.sections.length} sections`);
}
