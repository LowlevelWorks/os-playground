import { ApiError, apiCall, type FetchLike } from "./api";

/** The signed-in user, as the API shows them. */
export interface User {
  id: string;
  email?: string;
  name?: string;
}

/**
 * Sign-in happens on the API: it runs the Google (OIDC) flow and sets an
 * HttpOnly cookie. The page only asks who is signed in, sends the browser to
 * the login URL, and asks the API to sign out. It never sees a token.
 */
export interface AuthApi {
  /** The signed-in user, or null when nobody is. */
  me(signal?: AbortSignal): Promise<User | null>;
  signOut(): Promise<void>;
  /** Where to send the browser to sign in; it comes back to returnTo. */
  loginUrl(returnTo: string): string;
}

export function createAuthApi(base = "/api", fetchImpl?: FetchLike): AuthApi {
  const call = apiCall(base, fetchImpl);
  return {
    async me(signal) {
      let res: Response;
      try {
        res = await call("/v1/auth/me", { method: "GET", signal });
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) return null;
        throw err;
      }
      return readUser(await res.json().catch(() => null));
    },
    async signOut() {
      await call("/v1/auth/logout", { method: "POST" });
    },
    loginUrl(returnTo) {
      return `${base}/v1/auth/login?return_to=${encodeURIComponent(returnTo)}`;
    },
  };
}

function readUser(body: unknown): User {
  if (!body || typeof body !== "object" || !("id" in body) || typeof body.id !== "string" || !body.id) {
    throw new ApiError(0, "The server sent a response that could not be read.");
  }
  const str = (k: string) => {
    const v = (body as Record<string, unknown>)[k];
    return typeof v === "string" && v ? v : undefined;
  };
  return { id: body.id, email: str("email"), name: str("name") };
}

/** What the API reports, in ?login=, when sign-in did not complete. */
export type LoginOutcome = "failed" | "cancelled" | "disabled";

/** Words for a sign-in that did not complete. */
export function loginMessage(outcome: string | null): string | null {
  switch (outcome) {
    case "cancelled":
      return "Sign-in was cancelled.";
    case "disabled":
      return "This account is not allowed to sign in.";
    case "failed":
      return "Sign-in failed. Try again.";
    default:
      return null;
  }
}

/** The page's URL without ?login=, so a reload does not repeat the message. */
export function withoutLoginParam(href: string): string {
  const url = new URL(href);
  url.searchParams.delete("login");
  return url.toString();
}
