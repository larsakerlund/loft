import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The apex site builds to dist/, which the image copies to nginx's apex root. In dev, /api is
// proxied to a locally running loftd (or the split's frontend) so the console can call the daemon.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      "/api": { target: "http://localhost:8082", changeOrigin: true },
    },
  },
});
