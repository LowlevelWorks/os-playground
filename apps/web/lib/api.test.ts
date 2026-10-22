import { describe, expect, it } from "vitest";
import { ApiError, createApi } from "./api";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
    ...init,
  });

describe("createApi", () => {
  it("creates a VM and returns only what the API sends", async () => {
    const calls: [string, RequestInit | undefined][] = [];
    const api = createApi("/api", async (url, init) => {
      calls.push([url, init]);
      return json({ id: "vm-1", status: "starting", created_at: "2026-10-22T06:00:00Z" }, { status: 201 });
    });

    const vm = await api.createVM();

    expect(vm).toEqual({ id: "vm-1", status: "starting", created_at: "2026-10-22T06:00:00Z" });
    expect(calls[0][0]).toBe("/api/vms");
    expect(calls[0][1]?.method).toBe("POST");
  });

  it("reads one VM and escapes the id in the path", async () => {
    let seen = "";
    const api = createApi("/api", async (url) => {
      seen = url;
      return json({ id: "a/b", status: "running", created_at: "" });
    });

    await api.getVM("a/b");

    expect(seen).toBe("/api/vms/a%2Fb");
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
