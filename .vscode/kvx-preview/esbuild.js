// Bundles two targets:
//   dist/extension.js  — Node/CommonJS, `vscode` external (runs in the host)
//   dist/webview.js    — browser IIFE (runs in the preview webview)
const esbuild = require("esbuild");

const production = process.argv.includes("--production");
const watch = process.argv.includes("--watch");

/** @type {import('esbuild').BuildOptions} */
const common = {
  bundle: true,
  minify: production,
  sourcemap: !production,
  logLevel: "info",
};

const extensionCfg = {
  ...common,
  entryPoints: ["src/extension.ts"],
  outfile: "dist/extension.js",
  platform: "node",
  format: "cjs",
  external: ["vscode"],
  target: "node18",
};

const webviewCfg = {
  ...common,
  entryPoints: ["src/webview/main.ts"],
  outfile: "dist/webview.js",
  platform: "browser",
  format: "iife",
  target: "es2020",
};

async function main() {
  if (watch) {
    const ctxA = await esbuild.context(extensionCfg);
    const ctxB = await esbuild.context(webviewCfg);
    await Promise.all([ctxA.watch(), ctxB.watch()]);
    console.log("watching…");
  } else {
    await Promise.all([esbuild.build(extensionCfg), esbuild.build(webviewCfg)]);
    console.log("build complete");
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
