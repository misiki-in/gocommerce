// A single-page app: no server rendering and nothing to prerender, because
// every screen depends on a running store and an admin token.
export const ssr = false;
export const prerender = false;
export const trailingSlash = "never";
