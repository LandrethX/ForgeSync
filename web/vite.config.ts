import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// The build goes straight into the Go package that embeds it.
// In development, `npm run dev` proxies the API to a controller on :8090
// (`make run`), so the session cookie stays same-origin.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://127.0.0.1:8090", changeOrigin: false },
    },
  },
  test: {
    environment: "jsdom",
    restoreMocks: true,
    setupFiles: ["src/test/setup.ts"],
  },
});
