import adapter from "@sveltejs/adapter-static";

/** @type {import('@sveltejs/kit').Config} */
export default {
    kit: {
        // A single-page app: every route falls back to index.html, which the Go
        // binary serves for anything under /_/ that is not a real file. The
        // admin panel is a client of the same public API as any other client,
        // so there is nothing to render on the server.
        adapter: adapter({
            pages: "build",
            assets: "build",
            fallback: "index.html",
            precompress: false,
            strict: false,
        }),

        // Mounted at the root. The API lives entirely under /api (plus /health,
        // /doc and a module's /x/), so the panel can have the plain URL — you
        // type the store's address and you are in the dashboard.
        paths: {
            base: "",
            relative: false,
        },

        // SvelteKit emits one inline bootstrap script that it will not let us
        // remove. Hash mode puts that script's SHA-256 into a <meta> CSP, so
        // the *only* inline script the browser will run is that exact one —
        // any injected script has a different hash and is refused.
        //
        // The binary also sends a CSP header. Both policies are enforced, and
        // a script must satisfy both, so the header can afford to allow inline
        // broadly while these hashes do the real narrowing.
        csp: {
            mode: "hash",
            directives: {
                "default-src": ["self"],
                "script-src": ["self"],
                "style-src": ["self", "unsafe-inline"],
                "img-src": ["self", "data:", "blob:"],
                "font-src": ["self"],
                "connect-src": ["self"],
                "base-uri": ["self"],
                "object-src": ["none"],
            },
        },

        typescript: { config: (c) => c },
    },
};
