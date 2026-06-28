import type {
  GenericModel,
  Model,
  Requirement,
  SpecModel,
  Task,
  WorkflowModel,
} from "../kvx/types";

interface VsApi {
  postMessage(msg: unknown): void;
  getState(): unknown;
  setState(s: unknown): void;
}

interface UiState {
  tab: string;
  selected?: string; // selected task id
  openReqs: Record<string, boolean>;
}

// ---- tiny DOM helper -------------------------------------------------------

type Child = Node | string | null | undefined | false;
function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Record<string, unknown> = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined) continue;
    if (k === "class") node.className = v as string;
    else if (k === "style") node.style.cssText = v as string;
    else if (k === "dataset") for (const [dk, dv] of Object.entries(v as Record<string, string>)) node.dataset[dk] = String(dv);
    else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2).toLowerCase(), v as EventListener);
    else (node as Record<string, unknown>)[k] = v;
  }
  for (const c of children) if (c !== null && c !== undefined && c !== false) node.append(c as Node | string);
  return node;
}

function reveal(vscode: VsApi, line: number): void {
  vscode.postMessage({ type: "reveal", line });
}

function trunc(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1).trimEnd() + "…" : s;
}

// ---- entry -----------------------------------------------------------------

export function render(root: HTMLElement, model: Model, vscode: VsApi): void {
  const saved = (vscode.getState() as UiState) ?? { tab: "", openReqs: {} };
  const state: UiState = { tab: saved.tab || defaultTab(model), selected: saved.selected, openReqs: saved.openReqs ?? {} };

  const rerender = () => {
    vscode.setState(state);
    root.className = "";
    root.replaceChildren(view(model, state, vscode, rerender));
  };
  rerender();
}

function defaultTab(model: Model): string {
  if (model.kind === "spec") return "tasks";
  if (model.kind === "workflow") return "workflow";
  return "sections";
}

function view(model: Model, state: UiState, vscode: VsApi, rerender: () => void): Node {
  const wrap = el("div", { class: "wrap" });
  if (model.kind === "spec") {
    wrap.append(specHead(model), tabs(["tasks", "traceability"], tabLabels, state, rerender));
    wrap.append(state.tab === "traceability" ? traceView(model, vscode, state, rerender) : taskView(model, vscode, state, rerender));
  } else if (model.kind === "workflow") {
    wrap.append(workflowHead(model), workflowView(model, vscode));
  } else {
    wrap.append(genericHead(model), genericView(model, vscode));
  }
  return wrap;
}

const tabLabels: Record<string, string> = {
  tasks: "Task map",
  traceability: "Traceability",
};

function tabs(ids: string[], labels: Record<string, string>, state: UiState, rerender: () => void): Node {
  const row = el("div", { class: "tabs" });
  for (const id of ids) {
    row.append(
      el("button", {
        class: "tab" + (state.tab === id ? " active" : ""),
        textContent: labels[id] ?? id,
        onClick: () => {
          state.tab = id;
          rerender();
        },
      }),
    );
  }
  return row;
}

// ---- spec: header + task map ----------------------------------------------

function specHead(m: SpecModel): Node {
  return el(
    "div",
    {},
    el(
      "div",
      { class: "head" },
      el("h1", { textContent: m.title || m.feature || "Spec" }),
      el("span", { class: "feature", textContent: m.feature }),
      m.status && el("span", { class: "chip", textContent: m.status }),
    ),
    m.intro && el("p", { class: "intro", textContent: trunc(m.intro, 320) }),
  );
}

function taskView(m: SpecModel, vscode: VsApi, state: UiState, rerender: () => void): Node {
  const leaves = m.tasks.filter((t) => t.wave !== undefined);
  const counts = tally(m.tasks);
  const danglingByTask = new Set(m.coverage.dangling.map((d) => d.task + "|" + d.ref));

  const container = el("div", {});

  // Summary row.
  container.append(
    el(
      "div",
      { class: "summary" },
      stat(String(m.tasks.length), "tasks"),
      stat(String(counts.done), "done"),
      stat(String(counts.in_progress), "in progress"),
      stat(String(counts.pending), "pending"),
      stat(`${m.coverage.coveredCriteria}/${m.coverage.totalCriteria}`, "criteria covered"),
      m.coverage.gaps.length > 0 && stat(String(m.coverage.gaps.length), "coverage gaps", true),
    ),
  );

  container.append(
    el("p", {
      class: "muted",
      textContent:
        "Columns are dependency waves — tasks in the same wave can run in parallel. Cross-task edges aren't encoded yet (tasks declare reqs + wave, not explicit requires).",
    }),
  );

  // Wave columns.
  const waves = el("div", { class: "waves" });
  for (const w of m.waves) {
    const col = el("div", { class: "wavecol" });
    col.append(el("div", { class: "wavehd" }, el("b", { textContent: "Wave " + w })));
    const inWave = leaves.filter((t) => t.wave === w);
    for (const t of inWave) {
      const group = t.id.split(".")[0];
      const card = el("div", {
        class: ["card", "s-" + t.status, state.selected === t.id ? "sel" : ""].join(" ").trim(),
        dataset: { group },
        onClick: () => {
          state.selected = state.selected === t.id ? undefined : t.id;
          reveal(vscode, t.line);
          rerender();
        },
        onMouseenter: () => highlightGroup(waves, group, true),
        onMouseleave: () => highlightGroup(waves, group, false),
      });
      card.append(el("div", { class: "id", textContent: t.id }));
      card.append(el("div", { class: "ttl", textContent: trunc(t.title, 64) }));
      const refs = uniq([...t.reqs, ...t.validatesRefs]);
      if (refs.length) {
        const chips = el("div", { class: "chips" });
        for (const r of refs) {
          chips.append(el("span", { class: "chip" + (danglingByTask.has(t.id + "|" + r) ? " gap" : ""), textContent: r }));
        }
        card.append(chips);
      }
      col.append(card);
    }
    waves.append(col);
  }
  container.append(waves);

  // Detail / overview panel.
  const selected = state.selected ? m.tasks.find((t) => t.id === state.selected) : undefined;
  container.append(selected ? taskDetail(selected, m, vscode) : phaseOverview(m, vscode));
  return container;
}

function highlightGroup(scope: HTMLElement, group: string, on: boolean): void {
  scope.querySelectorAll<HTMLElement>(".card").forEach((c) => {
    c.classList.toggle("dim", on && c.dataset.group !== group);
  });
}

function taskDetail(t: Task, m: SpecModel, vscode: VsApi): Node {
  const d = el("div", { class: "detail" });
  d.append(el("h3", {}, el("span", { class: "muted", textContent: t.id + "  " }), document.createTextNode(t.title)));
  const meta = [statusLabel(t.status), t.wave !== undefined ? "wave " + t.wave : "", t.parent ? "in " + t.parent : ""].filter(Boolean).join("  ·  ");
  d.append(el("div", { class: "meta", textContent: meta }));

  if (t.dos.length) {
    d.append(el("div", { class: "k", textContent: "Implementation" }));
    const ul = el("ul", {});
    for (const x of t.dos) ul.append(el("li", { textContent: x }));
    d.append(ul);
  }
  if (t.note) d.append(el("p", { textContent: t.note }));
  if (t.property) {
    d.append(el("div", { class: "k", textContent: "Property test" }));
    d.append(el("p", { textContent: t.property }));
  }
  const refs = uniq([...t.reqs, ...t.validatesRefs]);
  if (refs.length) {
    d.append(el("div", { class: "k", textContent: "Validates acceptance criteria" }));
    const chips = el("div", { class: "chips" });
    for (const r of refs) {
      const crit = findCriterion(m, r);
      chips.append(
        el("span", {
          class: "tasklink",
          textContent: r,
          title: crit ? crit.text : "no such criterion",
          onClick: () => crit && reveal(vscode, requirementLine(m, r)),
        }),
      );
    }
    d.append(chips);
  }
  return d;
}

function phaseOverview(m: SpecModel, vscode: VsApi): Node {
  const d = el("div", { class: "detail" });
  d.append(el("h3", { textContent: "Phases & checkpoints" }));
  d.append(el("div", { class: "meta", textContent: "Select a task above for detail. Top-level groups:" }));
  const groups = m.tasks.filter((t) => t.depth === 0);
  for (const g of groups) {
    const childLeaves = m.tasks.filter((t) => t.parent === g.id && t.wave !== undefined);
    const done = childLeaves.filter((t) => t.status === "done").length;
    const roll = childLeaves.length ? ` (${done}/${childLeaves.length})` : "";
    const row = el(
      "div",
      { class: "step", onClick: () => reveal(vscode, g.line), style: "cursor:pointer" },
      el("span", { class: "muted", textContent: g.id, style: "min-width:34px" }),
      el("span", { class: "txt", textContent: trunc(g.title, 80) + roll }),
    );
    if (g.section) row.prepend(el("div", { class: "section-title", textContent: g.section, style: "width:100%;margin:14px 0 2px" }));
    d.append(row);
  }
  return d;
}

// ---- spec: traceability ----------------------------------------------------

function traceView(m: SpecModel, vscode: VsApi, state: UiState, rerender: () => void): Node {
  const container = el("div", {});
  container.append(
    el(
      "div",
      { class: "summary" },
      stat(String(m.coverage.totalCriteria), "criteria"),
      stat(String(m.coverage.coveredCriteria), "covered"),
      stat(String(m.coverage.gaps.length), "gaps", m.coverage.gaps.length > 0),
      stat(String(m.coverage.dangling.length), "dangling refs", m.coverage.dangling.length > 0),
    ),
  );
  container.append(
    el("p", {
      class: "muted",
      textContent: "A criterion is covered when a task's reqs (or a property test's validates) references it. Gaps are acceptance criteria no task implements.",
    }),
  );

  for (const r of m.requirements) {
    const covered = r.criteria.filter((c) => c.coveredBy.length > 0).length;
    const total = r.criteria.length;
    const hasGap = covered < total;
    const open = state.openReqs[r.id] ?? hasGap; // auto-open requirements with gaps

    const cov = el("span", {
      class: "cov" + (total && covered === total ? " full" : covered === 0 ? " none" : ""),
      textContent: `${covered}/${total}`,
    });
    const reqEl = el("div", { class: "req" + (open ? " open" : "") });
    reqEl.append(
      el(
        "div",
        {
          class: "reqhd",
          onClick: () => {
            state.openReqs[r.id] = !open;
            rerender();
          },
        },
        el("span", { class: "rid", textContent: r.id }),
        el("span", { class: "rt", textContent: r.title }),
        cov,
      ),
    );
    const crits = el("div", { class: "crits" });
    for (const c of r.criteria) {
      const gap = c.coveredBy.length === 0;
      const by = el("div", { class: "by" });
      for (const tid of c.coveredBy) {
        const task = m.tasks.find((t) => t.id === tid);
        by.append(el("span", { class: "tasklink", textContent: tid, onClick: () => task && reveal(vscode, task.line) }));
      }
      crits.append(
        el(
          "div",
          { class: "crit" + (gap ? " gap" : "") },
          el("span", { class: "dot" }),
          el("span", { class: "cref", textContent: c.ref }),
          el("span", { class: "ctext", textContent: trunc(c.text, 150) }),
          by,
        ),
      );
    }
    reqEl.append(crits);
    container.append(reqEl);
  }

  if (m.coverage.dangling.length) {
    container.append(el("div", { class: "section-title", textContent: "Dangling references" }));
    for (const d of m.coverage.dangling) {
      const task = m.tasks.find((t) => t.id === d.task);
      container.append(
        el(
          "div",
          { class: "crit gap" },
          el("span", { class: "dot" }),
          el("span", { class: "cref", textContent: d.ref }),
          el("span", { class: "ctext", textContent: `task ${d.task} references a criterion that does not exist` }),
          el("span", { class: "tasklink", textContent: d.task, onClick: () => task && reveal(vscode, task.line) }),
        ),
      );
    }
  }
  return container;
}

// ---- workflow --------------------------------------------------------------

function workflowHead(m: WorkflowModel): Node {
  return el(
    "div",
    {},
    el("div", { class: "head" }, el("h1", { textContent: m.name || "Workflow" }), m.activeFeature && el("span", { class: "feature", textContent: "active: " + m.activeFeature })),
    m.sourceOfTruth && el("p", { class: "intro", textContent: trunc(m.sourceOfTruth, 240) }),
  );
}

function workflowView(m: WorkflowModel, vscode: VsApi): Node {
  const c = el("div", {});
  if (m.loop.length) {
    c.append(el("div", { class: "section-title", textContent: "The loop (every session)" }));
    const flow = el("div", { class: "flow" });
    m.loop.forEach((s, i) => {
      flow.append(el("div", { class: "step" }, el("div", { class: "num", textContent: String(i + 1) }), el("div", { class: "txt", textContent: s.text })));
    });
    c.append(flow);
    const back = loopBackTarget(m.loop);
    if (back) c.append(el("div", { class: "loopback", textContent: `↻ repeat from step ${back} until all tasks are done` }));
  }
  c.append(kvSection("Principles", m.principles));
  c.append(kvSection("Cortex (persistent memory)", m.cortex));
  c.append(kvSection("Hard rules", m.hardRules));
  if (m.adapters.length) {
    c.append(el("div", { class: "section-title", textContent: "Generated IDE targets" }));
    const grid = el("div", { class: "grid2" });
    for (const a of m.adapters) {
      grid.append(el("div", { class: "kvcard", onClick: () => reveal(vscode, a.line), style: "cursor:pointer" }, el("div", { class: "key", textContent: a.label }), el("div", { class: "val", textContent: a.path })));
    }
    c.append(grid);
  }
  return c;
}

function loopBackTarget(loop: { text: string }[]): number | undefined {
  for (let i = loop.length - 1; i >= 0; i--) {
    const m = loop[i].text.match(/loop to step[_ ]?(\d+)/i) ?? loop[i].text.match(/repeat from step[_ ]?(\d+)/i);
    if (m) return Number(m[1]);
  }
  return undefined;
}

function kvSection(title: string, items: { key: string; text: string }[]): Node {
  const wrap = el("div", {});
  if (!items.length) return wrap;
  wrap.append(el("div", { class: "section-title", textContent: title }));
  const grid = el("div", { class: "grid2" });
  for (const it of items) {
    grid.append(el("div", { class: "kvcard" }, el("div", { class: "key", textContent: humanize(it.key) }), el("div", { class: "val", textContent: it.text })));
  }
  wrap.append(grid);
  return wrap;
}

// ---- generic fallback ------------------------------------------------------

function genericHead(m: GenericModel): Node {
  return el("div", { class: "head" }, el("h1", { textContent: m.fileName }), el("span", { class: "feature", textContent: `${m.sections.length} sections` }));
}

function genericView(m: GenericModel, vscode: VsApi): Node {
  const c = el("div", {});
  for (const s of m.sections) {
    c.append(el("div", { class: "section-title", textContent: s.name, onClick: () => reveal(vscode, s.line), style: "cursor:pointer" }));
    const grid = el("div", { class: "grid2" });
    for (const e of s.entries) grid.append(el("div", { class: "kvcard" }, el("div", { class: "key", textContent: e.key }), el("div", { class: "val", textContent: trunc(e.text, 200) })));
    if (s.entries.length) c.append(grid);
  }
  return c;
}

// ---- shared bits -----------------------------------------------------------

function stat(value: string, label: string, alarm = false): Node {
  return el("div", { class: "stat" + (alarm ? " alarm" : "") }, el("b", { textContent: value }), el("span", { textContent: label }));
}

function tally(tasks: Task[]): { done: number; in_progress: number; pending: number } {
  const out = { done: 0, in_progress: 0, pending: 0 } as Record<string, number>;
  for (const t of tasks) out[t.status] = (out[t.status] ?? 0) + 1;
  return out as { done: number; in_progress: number; pending: number };
}

function statusLabel(s: string): string {
  return s === "in_progress" ? "in progress" : s;
}

function findCriterion(m: SpecModel, ref: string): Requirement["criteria"][number] | undefined {
  for (const r of m.requirements) for (const c of r.criteria) if (c.ref === ref) return c;
  return undefined;
}

function requirementLine(m: SpecModel, ref: string): number {
  const id = ref.split(".")[0];
  return m.requirements.find((r) => r.id === id)?.line ?? 0;
}

function uniq<T>(xs: T[]): T[] {
  return [...new Set(xs)];
}

function humanize(k: string): string {
  return k
    .split("_")
    .filter(Boolean)
    .map((p) => p[0].toUpperCase() + p.slice(1))
    .join(" ");
}
