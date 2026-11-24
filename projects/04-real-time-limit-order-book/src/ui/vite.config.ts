import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies API and WebSocket traffic to a locally running `lob serve`.
const api = process.env.LOB_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/v1": { target: api, ws: true },
      "/metrics": api,
    },
  },
});
