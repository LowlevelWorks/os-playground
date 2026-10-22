import { ApiError, type Api, type VM } from "./api";

/**
 * Where a session is. The UI renders this and nothing else, so every way a
 * session can go wrong is one of these states with a message a person can act on.
 */
export type SessionState =
  | { kind: "idle" }
  | { kind: "creating" }
  | { kind: "starting"; id: string }
  | { kind: "connecting"; id: string }
  | { kind: "live"; id: string }
  | { kind: "ending"; id: string }
  | { kind: "ended"; id: string; message: string }
  | {
    kind: "error";
    message: string;
    /** Set when the VM may still exist, so trying again can resume it. */
    id?: string;
    canRetry: boolean;
    /** The VM is gone for good: forget it and start a new one. */
    gone?: boolean;
    retryAfterSec?: number;
  };

/** The part of WebSocket this module uses, so tests can supply a fake. */
export interface SocketLike {
  binaryType: string;
  readyState: number;
  onopen: ((ev: Event) => void) | null;
  onmessage: ((ev: MessageEvent) => void) | null;
  onclose: ((ev: CloseEvent) => void) | null;
  onerror: ((ev: Event) => void) | null;
  send(data: Uint8Array<ArrayBuffer>): void;
  close(code?: number, reason?: string): void;
}

export interface SessionOptions {
  api: Api;
  /** e.g. "ws://localhost:8080" */
  wsBase: string;
  onState(state: SessionState): void;
  /** Bytes from the guest, in order. */
  onOutput(data: Uint8Array): void;

  openSocket?: (url: string) => SocketLike;
  pollIntervalMs?: number;
  startTimeoutMs?: number;
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
  now?: () => number;
}

const SOCKET_OPEN = 1;
const NORMAL_CLOSURE = 1000;
const GOING_AWAY = 1001;

/** One run of start, resume or end. Anything older than the latest is stale. */
interface Run {
  signal: AbortSignal;
  live(): boolean;
}

/**
 * Drives one session through reserve, boot, attach and live. It lives outside
 * React on purpose: opening a console must happen once, in response to a click,
 * never in an effect that Strict Mode runs twice (the second attach would be
 * refused, because the console allows one browser at a time).
 */
export class SessionController {
  private readonly opts: SessionOptions;
  private readonly pollIntervalMs: number;
  private readonly startTimeoutMs: number;
  private readonly sleep: (ms: number, signal: AbortSignal) => Promise<void>;
  private readonly now: () => number;

  private generation = 0;
  private abort: AbortController | null = null;
  private socket: SocketLike | null = null;
  private id: string | null = null;

  constructor(opts: SessionOptions) {
    this.opts = opts;
    this.pollIntervalMs = opts.pollIntervalMs ?? 500;
    this.startTimeoutMs = opts.startTimeoutMs ?? 60_000;
    this.sleep = opts.sleep ?? defaultSleep;
    this.now = opts.now ?? Date.now;
  }

  /** Reserve a new machine, wait for it to boot, and attach. */
  async start(): Promise<void> {
    const run = this.begin();
    this.id = null;
    this.set(run, { kind: "creating" });

    let vm: VM;
    try {
      vm = await this.opts.api.createVM(run.signal);
    } catch (err) {
      this.fail(run, err);
      return;
    }
    if (!run.live()) return;
    await this.boot(run, vm);
  }

  /** Attach to a machine that already exists, for example after a reload. */
  async resume(id: string): Promise<void> {
    const run = this.begin();
    this.id = id;
    this.set(run, { kind: "starting", id });

    let vm: VM;
    try {
      vm = await this.opts.api.getVM(id, run.signal);
    } catch (err) {
      this.fail(run, err, id);
      return;
    }
    if (!run.live()) return;
    await this.boot(run, vm);
  }

  /** Send keystrokes. Ignored unless the console is open. */
  send(data: Uint8Array<ArrayBuffer>): void {
    if (this.socket && this.socket.readyState === SOCKET_OPEN) {
      this.socket.send(data);
    }
  }

  /** Stop the session and delete the machine. */
  async end(): Promise<void> {
    const id = this.id;
    const run = this.begin();
    this.id = null;
    if (!id) {
      this.set(run, { kind: "idle" });
      return;
    }
    this.set(run, { kind: "ending", id });
    try {
      await this.opts.api.deleteVM(id);
    } catch (err) {
      this.id = id;
      this.fail(run, err, id);
      return;
    }
    this.set(run, { kind: "idle" });
  }

  /**
   * Let go without deleting anything, for when the page goes away. The machine
   * stays; the server removes it when it has sat idle.
   */
  dispose(): void {
    this.generation++;
    this.abort?.abort();
    this.abort = null;
    this.closeSocket();
  }

  private begin(): Run {
    const gen = ++this.generation;
    this.abort?.abort();
    const abort = new AbortController();
    this.abort = abort;
    this.closeSocket();
    return { signal: abort.signal, live: () => this.generation === gen };
  }

  private set(run: Run, state: SessionState): void {
    if (run.live()) this.opts.onState(state);
  }

  private fail(run: Run, err: unknown, id?: string): void {
    if (!run.live() || isAbort(err)) return;
    this.set(run, { kind: "error", id, ...describe(err) });
  }

  private async boot(run: Run, vm: VM): Promise<void> {
    const id = vm.id;
    this.id = id;
    this.set(run, { kind: "starting", id });

    const deadline = this.now() + this.startTimeoutMs;
    let current = vm;
    for (; ;) {
      if (current.status === "running") break;

      if (current.status === "failed" || current.status === "stopped") {
        this.set(run, {
          kind: "error",
          id,
          canRetry: false,
          gone: true,
          message:
            current.status === "failed"
              ? "The machine failed to start."
              : "The machine shut down before it was ready.",
        });
        return;
      }
      if (this.now() >= deadline) {
        this.set(run, {
          kind: "error",
          id,
          canRetry: true,
          message: "The machine is taking too long to boot.",
        });
        return;
      }

      await this.sleep(this.pollIntervalMs, run.signal);
      if (!run.live()) return;
      try {
        current = await this.opts.api.getVM(id, run.signal);
      } catch (err) {
        this.fail(run, err, id);
        return;
      }
      if (!run.live()) return;
    }

    this.attach(run, id);
  }

  private attach(run: Run, id: string): void {
    this.set(run, { kind: "connecting", id });

    const open = this.opts.openSocket ?? ((url: string) => new WebSocket(url));
    const socket = open(`${this.opts.wsBase}/ws/vms/${encodeURIComponent(id)}`);
    socket.binaryType = "arraybuffer";
    this.socket = socket;
    let opened = false;

    socket.onopen = () => {
      if (!run.live()) return;
      opened = true;
      this.set(run, { kind: "live", id });
    };

    socket.onmessage = (ev) => {
      if (!run.live()) return;
      if (ev.data instanceof ArrayBuffer) this.opts.onOutput(new Uint8Array(ev.data));
    };

    socket.onclose = (ev) => {
      if (!run.live()) return;
      this.socket = null;

      if (!opened) {
        // A browser hides why a handshake failed. The usual cause is the console
        // being open somewhere else, so say that first.
        this.set(run, {
          kind: "error",
          id,
          canRetry: true,
          message:
            "Couldn't attach to the console. If this session is open in another tab, close that tab and try again.",
        });
      } else if (ev.code === NORMAL_CLOSURE) {
        this.set(run, { kind: "ended", id, message: "The machine shut down." });
      } else if (ev.code === GOING_AWAY) {
        this.set(run, {
          kind: "ended",
          id,
          message: "The server is restarting, so this session is gone. Start a new one.",
        });
      } else {
        this.set(run, {
          kind: "error",
          id,
          canRetry: true,
          message: "The connection dropped. Try again to pick up where you left off.",
        });
      }
    };
  }

  private closeSocket(): void {
    const socket = this.socket;
    this.socket = null;
    if (!socket) return;
    socket.onopen = socket.onmessage = socket.onclose = socket.onerror = null;
    try {
      socket.close(NORMAL_CLOSURE, "client leaving");
    } catch {
      // Already closed.
    }
  }
}

interface Failure {
  message: string;
  canRetry: boolean;
  gone?: boolean;
  retryAfterSec?: number;
}

/** Turn an error into words a visitor can act on. */
function describe(err: unknown): Failure {
  if (err instanceof ApiError) {
    if (err.status === 0) {
      return { message: "Can't reach the server. Check that the API is running.", canRetry: true };
    }
    if (err.status === 404) {
      return { message: "This session no longer exists.", canRetry: false, gone: true };
    }
    if (err.status === 503) {
      const sec = err.retryAfterSec ?? 5;
      return {
        message: `No machines are free right now. Try again in ${sec} seconds.`,
        canRetry: true,
        retryAfterSec: sec,
      };
    }
    if (err.status === 429) {
      const sec = err.retryAfterSec ?? 10;
      return {
        message: `You're going too fast. Try again in ${sec} seconds.`,
        canRetry: true,
        retryAfterSec: sec,
      };
    }
    if (err.status >= 500) {
      return { message: "The server ran into a problem. Try again.", canRetry: true };
    }
    return { message: err.message, canRetry: true };
  }
  return { message: "Something went wrong. Try again.", canRetry: true };
}

function isAbort(err: unknown): boolean {
  return (err as { name?: string } | null)?.name === "AbortError";
}

/** Resolves after ms, or at once when the signal aborts. */
function defaultSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
  });
}
