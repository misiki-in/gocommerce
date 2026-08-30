import { sveltekit } from "@sveltejs/kit/vite";
import { defineConfig } from "vite";

// During development the panel runs on Vite's own server and talks to a
// GoCommerce process on :8080, so the API is proxied rather than duplicated.
// In production both are the same binary and no proxy exists.
const API_TARGET = process.env.GOCOMMERCE_API || "http://127.0.0.1:8080";

const proxied = ["/api", "/health", "/doc", "/docs", "/x"];

export default defineConfig({
    plugins: [sveltekit()],
    server: {
        port: 5173,
        strictPort: false,
        proxy: Object.fromEntries(
            proxied.map((path) => [path, { target: API_TARGET, changeOrigin: true }]),
        ),
    },
    build: {
        // The whole panel is embedded in a Go binary, so a few larger chunks
        // cost nothing at runtime and keep the request count down.
        chunkSizeWarningLimit: 1200,
        reportCompressedSize: false,
    },
});
