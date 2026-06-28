// A zero-dependency parser for the .kvx config format, mirroring the semantics
// of spec/specgen's kvx.go: one `key = value` per line, double-quoted strings,
// bracketed lists, `#` comments outside quotes, ordered sections and keys.
//
// Unlike the Go original this also records the source line of each section
// header so the preview can reveal a section in the editor when clicked.

export class Doc {
  /** Section names in file order. */
  order: string[] = [];
  /** section -> keys in file order. */
  keyOrder: Record<string, string[]> = {};
  /** section -> key -> raw (un-unquoted) value. */
  sections: Record<string, Record<string, string>> = {};
  /** section -> 0-based line of its `[header]`. */
  sectionLine: Record<string, number> = {};

  private ensure(section: string, line: number): void {
    if (!(section in this.sections)) {
      this.sections[section] = {};
      this.keyOrder[section] = [];
      this.order.push(section);
      this.sectionLine[section] = line;
    }
  }

  static parse(text: string): Doc {
    const doc = new Doc();
    const lines = text.split(/\r?\n/);
    let section = "";
    for (let i = 0; i < lines.length; i++) {
      const line = stripComment(lines[i].trim());
      if (line === "") continue;
      if (line.startsWith("[")) {
        if (!line.endsWith("]")) continue; // tolerate malformed header in a live buffer
        section = line.slice(1, -1).trim();
        doc.ensure(section, i);
        continue;
      }
      const eq = line.indexOf("=");
      if (eq === -1) continue; // tolerate a half-typed line
      const key = line.slice(0, eq).trim();
      if (key === "") continue;
      doc.ensure(section, i);
      if (!(key in doc.sections[section])) doc.keyOrder[section].push(key);
      doc.sections[section][key] = line.slice(eq + 1).trim();
    }
    return doc;
  }

  has(section: string): boolean {
    return section in this.sections;
  }

  /** Raw, un-unquoted token (so callers can tell a list from a scalar). */
  raw(section: string, key: string): string {
    return this.sections[section]?.[key] ?? "";
  }

  /** Interpolated, unquoted scalar value, or "". */
  str(section: string, key: string): string {
    const raw = this.sections[section]?.[key];
    if (raw === undefined) return "";
    return interpolate(unquote(raw));
  }

  isList(section: string, key: string): boolean {
    const raw = this.raw(section, key).trim();
    return raw.startsWith("[") && raw.endsWith("]");
  }

  /** A bracketed list as interpolated strings (a bare scalar yields [scalar]). */
  list(section: string, key: string): string[] {
    let raw = this.sections[section]?.[key];
    if (raw === undefined) return [];
    raw = raw.trim();
    if (!raw.startsWith("[") || !raw.endsWith("]")) {
      const v = interpolate(unquote(raw));
      return v ? [v] : [];
    }
    const inner = raw.slice(1, -1).trim();
    if (inner === "") return [];
    return splitList(inner)
      .map((p) => interpolate(unquote(p.trim())))
      .filter((v) => v !== "");
  }

  keys(section: string): string[] {
    return this.keyOrder[section] ?? [];
  }

  /** (key, interpolated value) pairs in file order, optionally prefix-filtered. */
  orderedKV(section: string, prefix = ""): { key: string; text: string }[] {
    const out: { key: string; text: string }[] = [];
    for (const k of this.keyOrder[section] ?? []) {
      if (prefix && !k.startsWith(prefix)) continue;
      out.push({ key: k, text: interpolate(unquote(this.sections[section][k])) });
    }
    return out;
  }

  /** Sub-section names under "prefix." (e.g. "req.1" -> "1") in file order. */
  sectionsWithPrefix(prefix: string): string[] {
    const p = prefix + ".";
    return this.order
      .filter((s) => s.startsWith(p))
      .map((s) => s.slice(p.length));
  }

  uintOr(section: string, key: string, fallback: number): number {
    const v = this.str(section, key);
    if (v === "") return fallback;
    const n = Number.parseInt(v, 10);
    return Number.isNaN(n) ? fallback : n;
  }
}

function stripComment(line: string): string {
  let inQuote = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (c === '"') inQuote = !inQuote;
    else if (c === "#" && !inQuote) return line.slice(0, i).trim();
  }
  return line;
}

function splitList(s: string): string[] {
  const parts: string[] = [];
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

function unquote(s: string): string {
  s = s.trim();
  if (s.length >= 2 && s.startsWith('"') && s.endsWith('"')) return s.slice(1, -1);
  return s;
}

const ENV_REF = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
function interpolate(s: string): string {
  if (!s.includes("${")) return s;
  return s.replace(ENV_REF, (_m, name) => process.env?.[name] ?? "");
}

/** Sort ids like "1","1.10","1.2","2" by numeric segments (mirrors SortDottedIDs). */
export function sortDottedIDs(ids: string[]): string[] {
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
