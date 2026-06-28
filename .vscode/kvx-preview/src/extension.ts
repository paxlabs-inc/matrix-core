import * as vscode from "vscode";
import { buildModel } from "./kvx/model";
import type { WebviewToHost } from "./kvx/types";

export function activate(context: vscode.ExtensionContext): void {
  const manager = new PreviewManager(context);
  context.subscriptions.push(
    vscode.commands.registerCommand("kvx.showPreview", () => manager.showForActiveEditor()),
  );
}

export function deactivate(): void {
  /* nothing to clean up beyond disposables */
}

function isKvx(doc?: vscode.TextDocument): boolean {
  return !!doc && (doc.languageId === "kvx" || doc.fileName.toLowerCase().endsWith(".kvx"));
}

class PreviewManager {
  private panel: vscode.WebviewPanel | undefined;
  private targetUri: vscode.Uri | undefined;
  private debounce: ReturnType<typeof setTimeout> | undefined;
  private readonly disposables: vscode.Disposable[] = [];

  constructor(private readonly context: vscode.ExtensionContext) {
    // Re-render when the previewed document is edited.
    vscode.workspace.onDidChangeTextDocument(
      (e) => {
        if (this.targetUri && e.document.uri.toString() === this.targetUri.toString()) {
          this.scheduleUpdate();
        }
      },
      null,
      context.subscriptions,
    );
    // Follow the active editor to whichever .kvx file is in focus.
    vscode.window.onDidChangeActiveTextEditor(
      (ed) => {
        if (this.panel && isKvx(ed?.document)) {
          this.targetUri = ed!.document.uri;
          this.update();
        }
      },
      null,
      context.subscriptions,
    );
  }

  showForActiveEditor(): void {
    const editor = vscode.window.activeTextEditor;
    if (!isKvx(editor?.document)) {
      vscode.window.showInformationMessage("Open a .kvx file, then run KVX: Open Preview.");
      return;
    }
    this.targetUri = editor!.document.uri;

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
        localResourceRoots: [vscode.Uri.joinPath(this.context.extensionUri, "dist")],
      },
    );
    this.panel.webview.html = this.getHtml(this.panel.webview);

    this.panel.webview.onDidReceiveMessage(
      (msg: WebviewToHost) => {
        if (msg.type === "ready") this.update();
        else if (msg.type === "reveal") this.reveal(msg.line);
      },
      null,
      this.disposables,
    );

    this.panel.onDidDispose(
      () => {
        this.panel = undefined;
        this.targetUri = undefined;
        while (this.disposables.length) this.disposables.pop()?.dispose();
      },
      null,
      this.context.subscriptions,
    );
  }

  private scheduleUpdate(): void {
    if (this.debounce) clearTimeout(this.debounce);
    this.debounce = setTimeout(() => this.update(), 150);
  }

  private update(): void {
    if (!this.panel || !this.targetUri) return;
    const doc = vscode.workspace.textDocuments.find(
      (d) => d.uri.toString() === this.targetUri!.toString(),
    );
    if (!doc) return;
    const model = buildModel(doc.getText(), shortName(doc.uri));
    this.panel.title = `KVX: ${shortName(doc.uri)}`;
    void this.panel.webview.postMessage({ type: "update", model });
  }

  private async reveal(line: number): Promise<void> {
    if (!this.targetUri) return;
    const doc = await vscode.workspace.openTextDocument(this.targetUri);
    const editor = await vscode.window.showTextDocument(doc, {
      viewColumn: vscode.ViewColumn.One,
      preserveFocus: false,
    });
    const pos = new vscode.Position(Math.max(0, line), 0);
    const range = doc.lineAt(pos.line).range;
    editor.selection = new vscode.Selection(range.start, range.end);
    editor.revealRange(range, vscode.TextEditorRevealType.InCenter);
  }

  private getHtml(webview: vscode.Webview): string {
    const nonce = makeNonce();
    const scriptUri = webview.asWebviewUri(
      vscode.Uri.joinPath(this.context.extensionUri, "dist", "webview.js"),
    );
    const csp = [
      "default-src 'none'",
      `img-src ${webview.cspSource} https: data:`,
      `style-src ${webview.cspSource} 'unsafe-inline'`,
      `font-src ${webview.cspSource} https: data:`,
      `script-src 'nonce-${nonce}'`,
    ].join("; ");

    return /* html */ `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<meta http-equiv="Content-Security-Policy" content="${csp}" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<style>${STYLES}</style>
</head>
<body>
<div id="root" class="loading">Parsing spec…</div>
<script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
  }
}

function shortName(uri: vscode.Uri): string {
  const parts = uri.path.split("/");
  return parts.slice(-2).join("/");
}

function makeNonce(): string {
  const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  let s = "";
  for (let i = 0; i < 32; i++) s += chars[Math.floor(Math.random() * chars.length)];
  return s;
}

// Webview chrome. Adapts to the active VS Code theme via --vscode-* variables,
// with Paxeer Blue (#004CED) as the single accent and Inter / JetBrains Mono
// as the type pairing — honoring the project's own UI house rules (no borders
// for separation, no glow). Layer separation is by background contrast only.
const STYLES = /* css */ `
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
`;
