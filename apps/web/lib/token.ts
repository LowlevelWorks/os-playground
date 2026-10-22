/**
 * The owner token: a secret this browser makes for itself and sends with every
 * request. The server lets only the browser that created a machine see, attach
 * to or delete it. There is no account behind it; losing the token only means
 * losing access to a machine, which the server removes once it sits idle.
 */

const STORAGE_KEY = "os-playground.token";

/** 32 random bytes, base64url without padding. The server checks this exactly. */
const TOKEN_PATTERN = /^[A-Za-z0-9_-]{43}$/;

/** The part of Storage this module uses, so tests can supply a fake. */
export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export interface Token {
  value: string;
  /** False when storage is blocked: a reload will make a new token. */
  persisted: boolean;
}

export function newToken(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Read the stored token, or make and store one. Never throws. */
export function loadToken(storage: StorageLike | null, make: () => string = newToken): Token {
  try {
    const saved = storage?.getItem(STORAGE_KEY);
    if (saved && TOKEN_PATTERN.test(saved)) return { value: saved, persisted: true };
  } catch {
    // Blocked: fall through and keep a token in memory instead.
  }

  const value = make();
  try {
    if (storage) {
      storage.setItem(STORAGE_KEY, value);
      // Some browsers accept the write and drop it; only a read proves it.
      return { value, persisted: storage.getItem(STORAGE_KEY) === value };
    }
  } catch {
    // As above.
  }
  return { value, persisted: false };
}

let current: Token | null = null;

function browserStorage(): StorageLike | null {
  try {
    return typeof window === "undefined" ? null : window.localStorage;
  } catch {
    return null; // reading the property itself throws when storage is blocked
  }
}

function load(): Token {
  current ??= loadToken(browserStorage());
  return current;
}

/** This browser's token. Call it in the browser only. */
export function getToken(): string {
  return load().value;
}

/** Whether the token will survive a reload. Call it in the browser only. */
export function tokenPersisted(): boolean {
  return load().persisted;
}
