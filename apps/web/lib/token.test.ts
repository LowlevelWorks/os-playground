import { describe, expect, it } from "vitest";
import { loadToken, newToken, type StorageLike } from "./token";

class MemoryStorage implements StorageLike {
  items = new Map<string, string>();
  getItem(key: string) {
    return this.items.get(key) ?? null;
  }
  setItem(key: string, value: string) {
    this.items.set(key, value);
  }
}

const blocked: StorageLike = {
  getItem() {
    throw new DOMException("denied", "SecurityError");
  },
  setItem() {
    throw new DOMException("denied", "SecurityError");
  },
};

describe("the owner token", () => {
  it("is 43 characters of base64url, and different every time", () => {
    const a = newToken();
    const b = newToken();

    expect(a).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(b).not.toBe(a);
  });

  it("is created once and then reused", () => {
    const storage = new MemoryStorage();

    const first = loadToken(storage);
    const second = loadToken(storage);

    expect(first.persisted).toBe(true);
    expect(second.value).toBe(first.value);
    expect(storage.items.get("os-playground.token")).toBe(first.value);
  });

  it("replaces a stored value the server would reject", () => {
    const storage = new MemoryStorage();
    storage.setItem("os-playground.token", "not-a-token");

    const token = loadToken(storage);

    expect(token.value).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(storage.items.get("os-playground.token")).toBe(token.value);
  });

  it("falls back to memory when storage is blocked, and says so", () => {
    const token = loadToken(blocked);

    expect(token.value).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(token.persisted).toBe(false);
  });

  it("notices a write that storage silently dropped", () => {
    const dropping: StorageLike = { getItem: () => null, setItem: () => { } };

    expect(loadToken(dropping).persisted).toBe(false);
  });

  it("works with no storage at all", () => {
    expect(loadToken(null)).toMatchObject({ persisted: false });
  });
});
