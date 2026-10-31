import { describe, expect, it } from "vitest";
import { ApiError, createApi } from "./api";

const TOKEN = "T".repeat(43);

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
    ...init,
  });

describe("createApi", () => {
  it("sends the owner token with every request", async () => {
    const headers: Record<string, string>[] = [];
    const api = createApi(
      "/api",
      async (_url, init) => {
        headers.push(init?.headers as Record<string, string>);
        return json({ id: "vm-1", status: "running", created_at: "" });
      },
      () => TOKEN,
    );

    await api.createVM();
    await api.getVM("vm-1");
    await api.deleteVM("vm-1");

    expect(headers).toEqual(Array(3).fill({ Authorization: `Bearer ${TOKEN}` }));
  });

  it("reads a quota refusal for a machine of this browser's", async () => {
    const api = createApi("/api", async () =>
      json({ error: "quota", yours: true, id: "vm-7" }, { status: 409 }), () => TOKEN);

    const err = await api.createVM().catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).quota).toEqual({ yours: true, id: "vm-7" });
  });

  it("reads a quota refusal for someone else's machine", async () => {
    const api = createApi("/api", async () => json({ error: "quota", yours: false }, { status: 409 }), () => TOKEN);

    const err = await api.createVM().catch((e: unknown) => e);

    expect((err as ApiError).quota).toEqual({ yours: false, id: undefined });
  });

  it("does not mistake another 409 for a quota refusal", async () => {
    const api = createApi("/api", async () => json({ error: "conflict" }, { status: 409 }), () => TOKEN);

    const err = await api.createVM().catch((e: unknown) => e);

    expect((err as ApiError).quota).toBeUndefined();
  });

  it("creates a VM and returns only what the API sends", async () => {
    const calls: [string, RequestInit | undefined][] = [];
    const api = createApi("/api", async (url, init) => {
      calls.push([url, init]);
      return json({ id: "vm-1", status: "starting", created_at: "2026-10-22T06:00:00Z" }, { status: 201 });
    });

    const vm = await api.createVM();

    expect(vm).toEqual({ id: "vm-1", status: "starting", created_at: "2026-10-22T06:00:00Z" });
    expect(calls[0][0]).toBe("/api/playgrounds");
    expect(calls[0][1]?.method).toBe("POST");
  });

  it("reads one VM and escapes the id in the path", async () => {
    let seen = "";
    const api = createApi("/api", async (url) => {
      seen = url;
      return json({ id: "a/b", status: "running", created_at: "" });
    });

    await api.getVM("a/b");

    expect(seen).toBe("/api/playgrounds/a%2Fb");
  });

  it("turns an error body into an ApiError with the server's message", async () => {
    const api = createApi("/api", async () => json({ error: "vm not found" }, { status: 404 }));

    await expect(api.getVM("x")).rejects.toMatchObject({
      name: "ApiError",
      status: 404,
      message: "vm not found",
    });
  });

  it("reads Retry-After from a 503", async () => {
    const api = createApi("/api", async () =>
      json({ error: "busy" }, { status: 503, headers: { "Retry-After": "5" } }),
    );

    const err = await api.createVM().catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).retryAfterSec).toBe(5);
  });

  it("survives an error response that is not JSON", async () => {
    const api = createApi("/api", async () => new Response("<html>bad gateway</html>", { status: 502 }));

    await expect(api.createVM()).rejects.toMatchObject({ status: 502 });
  });

  it("reports an unreachable server as status 0", async () => {
    const api = createApi("/api", async () => {
      throw new TypeError("fetch failed");
    });

    await expect(api.createVM()).rejects.toMatchObject({ status: 0 });
  });

  it("passes an abort through unchanged", async () => {
    const abort = new DOMException("aborted", "AbortError");
    const api = createApi("/api", async () => {
      throw abort;
    });

    await expect(api.getVM("x")).rejects.toBe(abort);
  });

  it("rejects a success response that is not a VM", async () => {
    const api = createApi("/api", async () => json({ nope: true }));

    await expect(api.createVM()).rejects.toMatchObject({ status: 0 });
  });

  it("treats deleting something already gone as success", async () => {
    const api = createApi("/api", async () => json({ error: "vm not found" }, { status: 404 }));

    await expect(api.deleteVM("x")).resolves.toBeUndefined();
  });

  it("still fails when a delete fails for real", async () => {
    const api = createApi("/api", async () => json({ error: "internal error" }, { status: 500 }));

    await expect(api.deleteVM("x")).rejects.toMatchObject({ status: 500 });
  });
});
