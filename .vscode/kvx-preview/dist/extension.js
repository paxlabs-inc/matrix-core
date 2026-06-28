"use strict";
var __create = Object.create;
var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __getProtoOf = Object.getPrototypeOf;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toESM = (mod, isNodeMode, target) => (target = mod != null ? __create(__getProtoOf(mod)) : {}, __copyProps(
  // If the importer is in node compatibility mode or this is not an ESM
  // file that has been converted to a CommonJS file using a Babel-
  // compatible transform (i.e. "__esModule" has not been set), then set
  // "default" to the CommonJS "module.exports" for node compatibility.
  isNodeMode || !mod || !mod.__esModule ? __defProp(target, "default", { value: mod, enumerable: true }) : target,
  mod
));
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

// src/extension.ts
var extension_exports = {};
__export(extension_exports, {
  activate: () => activate,
  deactivate: () => deactivate
});
module.exports = __toCommonJS(extension_exports);
var vscode = __toESM(require("vscode"));

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

// src/extension.ts
function activate(context) {
  const manager = new PreviewManager(context);
  context.subscriptions.push(
    vscode.commands.registerCommand("kvx.showPreview", () => manager.showForActiveEditor())
  );
}
function deactivate() {
}
function isKvx(doc) {
  return !!doc && (doc.languageId === "kvx" || doc.fileName.toLowerCase().endsWith(".kvx"));
}
var PreviewManager = class {
  constructor(context) {
    this.context = context;
    this.disposables = [];
    vscode.workspace.onDidChangeTextDocument(
      (e) => {
        if (this.targetUri && e.document.uri.toString() === this.targetUri.toString()) {
          this.scheduleUpdate();
        }
      },
      null,
      context.subscriptions
    );
    vscode.window.onDidChangeActiveTextEditor(
      (ed) => {
        if (this.panel && isKvx(ed?.document)) {
          this.targetUri = ed.document.uri;
          this.update();
        }
      },
      null,
      context.subscriptions
    );
  }
  showForActiveEditor() {
    const editor = vscode.window.activeTextEditor;
    if (!isKvx(editor?.document)) {
      vscode.window.showInformationMessage("Open a .kvx file, then run KVX: Open Preview.");
      return;
    }
    this.targetUri = editor.document.uri;
    if (this.panel) {
      this.panel.reveal(vscode.ViewColumn.Beside, true);
      this.update();
      return;
    }
    this.panel = vscode.window.createWebviewPanel(
      "kvxPreview",
      "KVX Preview",
      { viewColumn: vscode.ViewColumn.Beside, preserveFocus: true },
      {
        enableScripts: true,
        retainContextWhenHidden: true,
        localResourceRoots: [vscode.Uri.joinPath(this.context.extensionUri, "dist")]
      }
    );
    this.panel.webview.html = this.getHtml(this.panel.webview);
    this.panel.webview.onDidReceiveMessage(
      (msg) => {
        if (msg.type === "ready") this.update();
        else if (msg.type === "reveal") this.reveal(msg.line);
      },
      null,
      this.disposables
    );
    this.panel.onDidDispose(
      () => {
        this.panel = void 0;
        this.targetUri = void 0;
        while (this.disposables.length) this.disposables.pop()?.dispose();
      },
      null,
      this.context.subscriptions
    );
  }
  scheduleUpdate() {
    if (this.debounce) clearTimeout(this.debounce);
    this.debounce = setTimeout(() => this.update(), 150);
  }
  update() {
    if (!this.panel || !this.targetUri) return;
    const doc = vscode.workspace.textDocuments.find(
      (d) => d.uri.toString() === this.targetUri.toString()
    );
    if (!doc) return;
    const model = buildModel(doc.getText(), shortName(doc.uri));
    this.panel.title = `KVX: ${shortName(doc.uri)}`;
    void this.panel.webview.postMessage({ type: "update", model });
  }
  async reveal(line) {
    if (!this.targetUri) return;
    const doc = await vscode.workspace.openTextDocument(this.targetUri);
    const editor = await vscode.window.showTextDocument(doc, {
      viewColumn: vscode.ViewColumn.One,
      preserveFocus: false
    });
    const pos = new vscode.Position(Math.max(0, line), 0);
    const range = doc.lineAt(pos.line).range;
    editor.selection = new vscode.Selection(range.start, range.end);
    editor.revealRange(range, vscode.TextEditorRevealType.InCenter);
  }
  getHtml(webview) {
    const nonce = makeNonce();
    const scriptUri = webview.asWebviewUri(
      vscode.Uri.joinPath(this.context.extensionUri, "dist", "webview.js")
    );
    const csp = [
      "default-src 'none'",
      `img-src ${webview.cspSource} https: data:`,
      `style-src ${webview.cspSource} 'unsafe-inline'`,
      `font-src ${webview.cspSource} https: data:`,
      `script-src 'nonce-${nonce}'`
    ].join("; ");
    return (
      /* html */
      `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<meta http-equiv="Content-Security-Policy" content="${csp}" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<style>${STYLES}</style>
</head>
<body>
<div id="root" class="loading">Parsing spec\u2026</div>
<script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`
    );
  }
};
function shortName(uri) {
  const parts = uri.path.split("/");
  return parts.slice(-2).join("/");
}
function makeNonce() {
  const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  let s = "";
  for (let i = 0; i < 32; i++) s += chars[Math.floor(Math.random() * chars.length)];
  return s;
}
var STYLES = (
  /* css */
  `
:root {
  --accent: #004CED;
  --ok: #3fb950;
  --run: #d6a915;
  --idle: var(--vscode-descriptionForeground);
  --gap: #f85149;
  --font: 'Inter', var(--vscode-font-family), system-ui, sans-serif;
  --mono: 'JetBrains Mono', var(--vscode-editor-font-family), ui-monospace, monospace;
}
* { box-sizing: border-box; }
body {
  margin: 0; padding: 0;
  font-family: var(--font);
  color: var(--vscode-foreground);
  background: var(--vscode-editor-background);
  font-size: 13px; line-height: 1.5;
}
.loading { padding: 24px; color: var(--idle); font-family: var(--mono); }
.wrap { padding: 16px 20px 64px; max-width: 1100px; margin: 0 auto; }

.head { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; margin-bottom: 4px; }
.head h1 { font-size: 18px; font-weight: 650; margin: 0; letter-spacing: -0.01em; }
.head .feature { font-family: var(--mono); font-size: 11px; color: var(--idle); }
.intro { color: var(--vscode-descriptionForeground); margin: 8px 0 18px; max-width: 70ch; }

.tabs { display: flex; gap: 2px; margin: 14px 0 20px; }
.tab {
  font: inherit; font-size: 12px; cursor: pointer; border: none;
  padding: 6px 14px; border-radius: 6px;
  background: var(--vscode-editor-background); color: var(--idle);
}
.tab:hover { background: var(--vscode-list-hoverBackground); }
.tab.active { background: var(--accent); color: #fff; }

.bar { height: 6px; border-radius: 3px; background: var(--vscode-input-background); overflow: hidden; }
.bar > span { display: block; height: 100%; background: var(--ok); }

.summary { display: flex; gap: 20px; flex-wrap: wrap; margin-bottom: 18px; }
.stat { display: flex; flex-direction: column; gap: 2px; }
.stat b { font-size: 22px; font-weight: 650; font-family: var(--mono); }
.stat span { font-size: 11px; color: var(--idle); text-transform: uppercase; letter-spacing: 0.05em; }
.stat.alarm b { color: var(--gap); }

/* ----- Task map (wave columns) ----- */
.waves { display: flex; gap: 12px; overflow-x: auto; padding-bottom: 12px; }
.wavecol { min-width: 188px; flex: 0 0 188px; display: flex; flex-direction: column; gap: 8px; }
.wavehd { font-family: var(--mono); font-size: 11px; color: var(--idle); padding: 2px 4px;
  position: sticky; top: 0; }
.wavehd b { color: var(--vscode-foreground); }
.card {
  background: var(--vscode-input-background); border-radius: 8px;
  padding: 9px 10px 9px 12px; cursor: pointer; position: relative;
  border-left: 3px solid transparent;
}
.card:hover { background: var(--vscode-list-hoverBackground); }
.card.sel { outline: 1px solid var(--accent); }
.card.dim { opacity: 0.35; }
.card.s-done { border-left-color: var(--ok); }
.card.s-in_progress { border-left-color: var(--run); }
.card.s-pending { border-left-color: var(--vscode-input-border, #555); }
.card .id { font-family: var(--mono); font-size: 11px; color: var(--accent); }
.card .ttl { font-size: 12px; margin-top: 2px; }
.chips { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; }
.chip {
  font-family: var(--mono); font-size: 9.5px; padding: 1px 5px; border-radius: 4px;
  background: var(--vscode-badge-background); color: var(--vscode-badge-foreground);
}
.chip.gap { background: var(--gap); color: #fff; }

/* ----- Detail panel ----- */
.detail {
  margin-top: 16px; padding: 14px 16px; border-radius: 10px;
  background: var(--vscode-input-background);
}
.detail h3 { margin: 0 0 2px; font-size: 14px; }
.detail .meta { font-family: var(--mono); font-size: 11px; color: var(--idle); margin-bottom: 8px; }
.detail ul { margin: 6px 0; padding-left: 18px; }
.detail li { margin: 2px 0; }
.detail .k { font-size: 11px; color: var(--idle); text-transform: uppercase; letter-spacing: 0.05em; margin-top: 10px; }

/* ----- Traceability ----- */
.req { margin-bottom: 6px; border-radius: 8px; background: var(--vscode-input-background); }
.req > .reqhd { display: flex; align-items: center; gap: 10px; padding: 10px 12px; cursor: pointer; }
.req > .reqhd:hover { background: var(--vscode-list-hoverBackground); border-radius: 8px; }
.req .rid { font-family: var(--mono); color: var(--accent); font-size: 12px; min-width: 28px; }
.req .rt { flex: 1; font-size: 12.5px; }
.req .cov { font-family: var(--mono); font-size: 11px; color: var(--idle); }
.req .cov.full { color: var(--ok); }
.req .cov.none { color: var(--gap); }
.crits { padding: 0 12px 10px 50px; display: none; }
.req.open .crits { display: block; }
.crit { display: flex; gap: 8px; align-items: baseline; padding: 3px 0; }
.crit .dot { width: 7px; height: 7px; border-radius: 50%; flex: 0 0 auto; margin-top: 5px; background: var(--ok); }
.crit.gap .dot { background: var(--gap); }
.crit .cref { font-family: var(--mono); font-size: 11px; color: var(--idle); min-width: 34px; }
.crit .ctext { flex: 1; font-size: 11.5px; color: var(--vscode-descriptionForeground); }
.crit .by { display: flex; gap: 3px; flex-wrap: wrap; }
.tasklink {
  font-family: var(--mono); font-size: 10px; padding: 0 5px; border-radius: 4px; cursor: pointer;
  background: var(--vscode-badge-background); color: var(--vscode-badge-foreground);
}
.tasklink:hover { background: var(--accent); color: #fff; }

/* ----- Workflow ----- */
.flow { display: flex; flex-direction: column; gap: 0; position: relative; margin-left: 8px; }
.step { display: flex; gap: 12px; padding: 8px 0; align-items: flex-start; }
.step .num {
  font-family: var(--mono); font-size: 12px; color: #fff; background: var(--accent);
  width: 24px; height: 24px; border-radius: 50%; display: flex; align-items: center;
  justify-content: center; flex: 0 0 auto; z-index: 1;
}
.step .txt { padding-top: 2px; font-size: 12.5px; }
.loopback { font-family: var(--mono); font-size: 11px; color: var(--accent); margin: 4px 0 0 36px; }

.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-top: 8px; }
.kvcard { background: var(--vscode-input-background); border-radius: 8px; padding: 10px 12px; }
.kvcard .key { font-family: var(--mono); font-size: 11px; color: var(--accent); margin-bottom: 3px; }
.kvcard .val { font-size: 11.5px; color: var(--vscode-descriptionForeground); }
.section-title { font-size: 13px; font-weight: 650; margin: 22px 0 8px; }
.muted { color: var(--idle); font-family: var(--mono); font-size: 11px; }
`
);
// Annotate the CommonJS export names for ESM import in node:
0 && (module.exports = {
  activate,
  deactivate
});
//# sourceMappingURL=extension.js.map
