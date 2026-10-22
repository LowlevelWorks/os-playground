import { getToken } from "./token";

export type VMStatus =
  | "provisioning"
  | "starting"
  | "running"
  | "stopped"
  | "failed"
  | "unknown";

/** All the API tells a client about a VM. */
export interface VM {
  id: string;
  status: VMStatus;
  created_at: string;
}

/**
 * Why a create was refused for being over the per-client limit. `yours` means
 * the machine in the way belongs to this browser; `id` is set once it exists.
 */
export interface Quota {
  yours: boolean;
  id?: string;
}

/** A failed call. Status 0 means the server could not be reached at all. */
export class ApiError extends Error {
  readonly status: number;
  readonly retryAfterSec?: number;
  readonly quota?: Quota;

  constructor(status: number, message: string, retryAfterSec?: number, quota?: Quota) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryAfterSec = retryAfterSec;
    this.quota = quota;
  }
}

export interface Api {
  createVM(signal?: AbortSignal): Promise<VM>;
  getVM(id: string, signal?: AbortSignal): Promise<VM>;
  deleteVM(id: string, signal?: AbortSignal): Promise<void>;
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export function createApi(
  base = "/api",
  fetchImpl: FetchLike = (input, init) => fetch(input, init),
  token: () => string = getToken,
): Api {
  async function call(path: string, init: RequestInit): Promise<Response> {
    let res: Response;
    try {
      res = await fetchImpl(base + path, {
        ...init,
        headers: { Authorization: `Bearer ${token()}` },
      });
    } catch (err) {
      if (isAbort(err)) throw err;
      throw new ApiError(0, "Can't reach the server.");
    }
    if (!res.ok) throw await errorFrom(res);
    return res;
  }

  return {
    async createVM(signal) {
      return readVM(await call("/vms", { method: "POST", signal }));
    },
    async getVM(id, signal) {
      return readVM(
        await call(`/vms/${encodeURIComponent(id)}`, { method: "GET", signal }),
      );
    },
    async deleteVM(id, signal) {
      try {
        await call(`/vms/${encodeURIComponent(id)}`, { method: "DELETE", signal });
      } catch (err) {
        // Deleting something that is already gone is the outcome we wanted.
        if (err instanceof ApiError && err.status === 404) return;
        throw err;
      }
    },
  };
}

function isAbort(err: unknown): boolean {
  return (err as { name?: string } | null)?.name === "AbortError";
}

async function errorFrom(res: Response): Promise<ApiError> {
  let message = res.statusText || `Request failed (${res.status}).`;
  let quota: Quota | undefined;
  try {
    const body: unknown = JSON.parse(await res.text());
    if (body && typeof body === "object" && "error" in body && typeof body.error === "string") {
      message = body.error;
      if (res.status === 409 && body.error === "quota" && "yours" in body && typeof body.yours === "boolean") {
        const id = "id" in body && typeof body.id === "string" && body.id ? body.id : undefined;
        quota = { yours: body.yours, id };
      }
    }
  } catch {
    // Not JSON (a proxy's error page, say): keep the status text.
  }
  const retry = Number.parseInt(res.headers.get("Retry-After") ?? "", 10);
  return new ApiError(res.status, message, Number.isFinite(retry) && retry > 0 ? retry : undefined, quota);
}

async function readVM(res: Response): Promise<VM> {
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    throw new ApiError(0, "The server sent a response that could not be read.");
  }
  if (
    !body ||
    typeof body !== "object" ||
    !("id" in body) ||
    typeof body.id !== "string" ||
    !("status" in body) ||
    typeof body.status !== "string"
  ) {
    throw new ApiError(0, "The server sent a response that could not be read.");
  }
  const created = "created_at" in body && typeof body.created_at === "string" ? body.created_at : "";
  return { id: body.id, status: body.status as VMStatus, created_at: created };
}
