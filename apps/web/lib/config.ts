
/**
 * REST base URL. In production this is a same-origin path (e.g. "/api") that
 * CloudFront routes to the backend origin, or an absolute URL via env.
 * NEXT_PUBLIC_* vars are inlined at build time.
 */
export const API_BASE = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "/api").replace(/\/+$/, "");

/**
 * Base URL for the terminal WebSocket. Browsers do not need CORS for
 * WebSockets, but the Go server checks the page's Origin. Call this in the
 * browser only.
 */
export function wsBase(): string {
  const configured = process.env.NEXT_PUBLIC_API_WS_URL;
  if (configured) return configured.replace(/\/+$/, "");
  const scheme = window.location.protocol === "https:" ? "wss" : "ws";
  return `${scheme}://${window.location.hostname}:8080/api`;
}

/** What the Enter key sends. Terminals send \r; some OSes want \n. */
export const ENTER_AS_LF = process.env.NEXT_PUBLIC_ENTER_KEY === "lf";
