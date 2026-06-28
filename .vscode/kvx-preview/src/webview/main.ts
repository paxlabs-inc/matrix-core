import { render } from "./render";
import type { HostToWebview } from "../kvx/types";

declare function acquireVsCodeApi(): {
  postMessage(msg: unknown): void;
  getState(): unknown;
  setState(s: unknown): void;
};

const vscode = acquireVsCodeApi();
const root = document.getElementById("root") as HTMLElement;

window.addEventListener("message", (event: MessageEvent<HostToWebview>) => {
  const msg = event.data;
  if (msg.type === "update") {
    try {
      render(root, msg.model, vscode);
    } catch (err) {
      root.className = "loading";
      root.textContent = "Render error: " + (err instanceof Error ? err.message : String(err));
    }
  }
});

// Tell the host we're mounted so it sends the first model.
vscode.postMessage({ type: "ready" });
