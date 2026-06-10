import { reactRouter } from "@react-router/dev/vite";
import { cloudflare } from "@cloudflare/vite-plugin";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

const resolvePath = (p: string) => fileURLToPath(new URL(p, import.meta.url));

export default defineConfig({
  plugins: [
    cloudflare({ viteEnvironment: { name: "ssr" } }),
    tailwindcss(),
    reactRouter(),
  ],
  resolve: {
    alias: {
      // Vendored smoothui library was authored against an un-ported monorepo.
      // Map its `@repo/*` and `@smoothui/*` specifiers onto the local shim.
      "@repo/shadcn-ui/lib/utils": resolvePath("./app/lib/utils.ts"),
      "@repo/shadcn-ui/components/ui": resolvePath("./components/ui/shadcn"),
      "@repo/smoothui/components/smooth-button": resolvePath(
        "./components/ui/smoothui/smooth-button/index.tsx"
      ),
      "@smoothui/data": resolvePath("./app/lib/smoothui-data.ts"),
      "@": resolvePath("./app"),
    },
  },
});
