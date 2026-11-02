import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { createAuthApi, loginMessage, withoutLoginParam } from "./auth";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" }, ...init });

describe("createAuthApi", () => {
  it("reads the signed-in user, with the cookie", async () => {
    const calls: [string, RequestInit | undefined][] = [];
    const auth = createAuthApi("/api", async (url, init) => {
      calls.push([url, init]);
      return json({ id: "u-1", email: "ada@example.com", name: "Ada" });
    });

    expect(await auth.me()).toEqual({ id: "u-1", email: "ada@example.com", name: "Ada" });
    expect(calls[0][0]).toBe("/api/v1/auth/me");
    expect(calls[0][1]?.credentials).toBe("include");
  });

  it("treats 401 as signed out, and other failures as errors", async () => {
    const out = createAuthApi("/api", async () => json({ error: "not signed in" }, { status: 401 }));
    expect(await out.me()).toBeNull();

    const down = createAuthApi("/api", async () => json({ error: "internal error" }, { status: 500 }));
    await expect(down.me()).rejects.toBeInstanceOf(ApiError);

    const unreachable = createAuthApi("/api", async () => {
      throw new TypeError("Failed to fetch");
    });
    await expect(unreachable.me()).rejects.toMatchObject({ status: 0 });
  });

  it("refuses a user without an id", async () => {
    const auth = createAuthApi("/api", async () => json({ email: "a@example.com" }));
    await expect(auth.me()).rejects.toBeInstanceOf(ApiError);
  });

  it("signs out with a POST that carries the cookie", async () => {
    const calls: [string, RequestInit | undefined][] = [];
    const auth = createAuthApi("/api", async (url, init) => {
      calls.push([url, init]);
      return new Response(null, { status: 204 });
    });

    await auth.signOut();

    expect(calls).toHaveLength(1);
    expect(calls[0][0]).toBe("/api/v1/auth/logout");
    expect(calls[0][1]).toMatchObject({ method: "POST", credentials: "include" });
  });

  it("builds a login URL that returns to the page", () => {
    const auth = createAuthApi("http://localhost:8080/api");
    expect(auth.loginUrl("http://localhost:3000/?a=b&c=d")).toBe(
      "http://localhost:8080/api/v1/auth/login?return_to=http%3A%2F%2Flocalhost%3A3000%2F%3Fa%3Db%26c%3Dd",
    );
  });
});

describe("the login outcome", () => {
  it("has words for each outcome the API reports, and none otherwise", () => {
    expect(loginMessage("failed")).toMatch(/failed/i);
    expect(loginMessage("cancelled")).toMatch(/cancelled/i);
    expect(loginMessage("disabled")).toMatch(/not allowed/i);
    expect(loginMessage(null)).toBeNull();
    expect(loginMessage("<script>")).toBeNull();
  });

  it("is removed from the URL, keeping everything else", () => {
    expect(withoutLoginParam("http://localhost:3000/play?login=failed&x=1#t")).toBe("http://localhost:3000/play?x=1#t");
    expect(withoutLoginParam("http://localhost:3000/")).toBe("http://localhost:3000/");
  });
});
