import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  base: "/",
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  server: { proxy: { "/api": "http://127.0.0.1:8081", "/faults": "http://127.0.0.1:8081", "/link": "http://127.0.0.1:8081", "/reset": "http://127.0.0.1:8081" } }
});
