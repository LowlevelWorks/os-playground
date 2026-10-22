import { describe, expect, it, vi } from "vitest";
import { ApiError, type Api, type VM, type VMStatus } from "./api";
import { SessionController, type SessionState, type SocketLike } from "./session";

const vm = (status: VMStatus, id = "vm-1"): VM => ({ id, status, created_at: "" });

const TOKEN = "T".repeat(43);

class FakeSocket implements SocketLike {
  binaryType = "blob";
  readyState = 0;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  sent: number[][] = [];
  closedWith: { code?: number; reason?: string } | null = null;

  constructor(readonly url: string, readonly protocols: string[] = []) { }

  send(data: Uint8Array<ArrayBuffer>) {
    this.sent.push([...data]);
  }
  close(code?: number, reason?: string) {
    this.closedWith = { code, reason };
    this.readyState = 3;
  }

  // What the network does:
  open() {
    this.readyState = 1;
    this.onopen?.(new Event("open"));
  }
  receive(bytes: number[]) {
    this.onmessage?.({ data: new Uint8Array(bytes).buffer } as MessageEvent);
  }
  receiveText(text: string) {
    this.onmessage?.({ data: text } as MessageEvent);
  }
  serverClose(code: number, reason = "") {
    this.readyState = 3;
    this.onclose?.({ code, reason } as CloseEvent);
  }
}

function setup(over: { api?: Partial<Api>; startTimeoutMs?: number; sleep?: () => Promise<void>; now?: () => number } = {}) {
  const states: SessionState[] = [];
  const output: number[][] = [];
  const sockets: FakeSocket[] = [];
  const api: Api = {
    createVM: vi.fn(async () => vm("starting")),
    getVM: vi.fn(async () => vm("running")),
    deleteVM: vi.fn(async () => { }),
    ...over.api,
  };
  const controller = new SessionController({
    api,
    wsBase: "ws://api.test",
    onState: (s) => states.push(s),
    onOutput: (d) => output.push([...d]),
    token: () => TOKEN,
    openSocket: (url, protocols) => {
      const s = new FakeSocket(url, protocols);
      sockets.push(s);
      return s;
    },
    sleep: over.sleep ?? (async () => { }),
    startTimeoutMs: over.startTimeoutMs,
    now: over.now,
  });
  return { controller, api, states, output, sockets, kinds: () => states.map((s) => s.kind) };
}

const last = (states: SessionState[]) => states[states.length - 1];

describe("starting a session", () => {
  it("goes through creating, starting, connecting and live, and opens the right socket", async () => {
    const t = setup();

    await t.controller.start();
    expect(t.kinds()).toEqual(["creating", "starting", "connecting"]);
    expect(t.sockets[0].url).toBe("ws://api.test/ws/vms/vm-1");
    expect(t.sockets[0].url).not.toContain(TOKEN);
    expect(t.sockets[0].protocols).toEqual(["os-playground.v1", `token.${TOKEN}`]);
    expect(t.sockets[0].binaryType).toBe("arraybuffer");

    t.sockets[0].open();
    expect(last(t.states)).toEqual({ kind: "live", id: "vm-1" });
  });

  it("polls until the machine is running", async () => {
    const getVM = vi
      .fn<Api["getVM"]>()
      .mockResolvedValueOnce(vm("starting"))
      .mockResolvedValueOnce(vm("running"));
    const t = setup({ api: { createVM: async () => vm("provisioning"), getVM } });

    await t.controller.start();

    expect(getVM).toHaveBeenCalledTimes(2);
    expect(t.sockets).toHaveLength(1);
  });

  it("skips polling when the machine is already running", async () => {
    const getVM = vi.fn<Api["getVM"]>();
    const t = setup({ api: { createVM: async () => vm("running"), getVM } });

    await t.controller.start();

    expect(getVM).not.toHaveBeenCalled();
    expect(t.sockets).toHaveLength(1);
  });

  it("tells the visitor when no machines are free, and for how long", async () => {
    const t = setup({ api: { createVM: async () => { throw new ApiError(503, "busy", 7); } } });

    await t.controller.start();

    expect(last(t.states)).toMatchObject({
      kind: "error",
      canRetry: true,
      retryAfterSec: 7,
      message: expect.stringContaining("7 seconds"),
    });
    expect(t.sockets).toHaveLength(0);
  });

  it("explains an unreachable server", async () => {
    const t = setup({ api: { createVM: async () => { throw new ApiError(0, "x"); } } });

    await t.controller.start();

    expect(last(t.states)).toMatchObject({ kind: "error", canRetry: true });
    expect((last(t.states) as { message: string }).message).toContain("Can't reach the server");
  });

  it("fails when the machine fails to start", async () => {
    const t = setup({ api: { createVM: async () => vm("failed") } });

    await t.controller.start();

    expect(last(t.states)).toMatchObject({ kind: "error", gone: true, canRetry: false, id: "vm-1" });
    expect(t.sockets).toHaveLength(0);
  });

  it("gives up when the machine never finishes booting", async () => {
    let clock = 0;
    const t = setup({
      api: { createVM: async () => vm("starting"), getVM: async () => vm("starting") },
      startTimeoutMs: 3000,
      sleep: async () => { clock += 1000; },
      now: () => clock,
    });

    await t.controller.start();

    expect(last(t.states)).toMatchObject({
      kind: "error",
      canRetry: true,
      id: "vm-1",
      message: expect.stringContaining("too long"),
    });
  });

  // A second start would abandon the first machine, which would then hold
  // this browser's only slot until the server reaped it.
  it("ignores a second start while the first is still on its way", async () => {
    let release!: (v: VM) => void;
    const slow = new Promise<VM>((r) => (release = r));
    const createVM = vi.fn<Api["createVM"]>().mockReturnValueOnce(slow);
    const t = setup({ api: { createVM } });

    const first = t.controller.start();
    await t.controller.start();
    release(vm("running", "vm-1"));
    await first;

    expect(createVM).toHaveBeenCalledTimes(1);
    expect(t.sockets).toHaveLength(1);
    expect(t.sockets[0].url).toContain("vm-1");
  });

  it("can start again once the first start has failed", async () => {
    const createVM = vi
      .fn<Api["createVM"]>()
      .mockRejectedValueOnce(new ApiError(503, "busy", 5))
      .mockResolvedValueOnce(vm("running", "vm-2"));
    const t = setup({ api: { createVM } });

    await t.controller.start();
    await t.controller.start();

    expect(createVM).toHaveBeenCalledTimes(2);
    expect(t.sockets[0].url).toContain("vm-2");
  });

  it("offers to resume this browser's machine when it is over the limit", async () => {
    const t = setup({
      api: { createVM: async () => { throw new ApiError(409, "quota", undefined, { yours: true, id: "vm-7" }); } },
    });

    await t.controller.start();

    expect(last(t.states)).toEqual({
      kind: "error",
      message: "You already have a machine running.",
      canRetry: false,
      resumeId: "vm-7",
    });
  });

  it("says to wait when this browser's own machine is still being created", async () => {
    const t = setup({
      api: { createVM: async () => { throw new ApiError(409, "quota", undefined, { yours: true }); } },
    });

    await t.controller.start();

    expect(last(t.states)).toMatchObject({
      kind: "error",
      canRetry: true,
      message: "Your machine is still starting, try again in a moment.",
    });
    expect(last(t.states)).not.toHaveProperty("resumeId");
  });

  it("explains that someone else on the network holds the machine", async () => {
    const t = setup({
      api: { createVM: async () => { throw new ApiError(409, "quota", undefined, { yours: false }); } },
    });

    await t.controller.start();

    expect(last(t.states)).toMatchObject({
      kind: "error",
      canRetry: true,
      message: "Someone on your network is using the machine. Try again in a few minutes.",
    });
    expect(last(t.states)).not.toHaveProperty("resumeId");
  });
});

describe("the console", () => {
  async function live() {
    const t = setup();
    await t.controller.start();
    t.sockets[0].open();
    return t;
  }

  it("delivers guest output as bytes and ignores text frames", async () => {
    const t = await live();

    t.sockets[0].receive([104, 105]);
    t.sockets[0].receiveText("not for the terminal");

    expect(t.output).toEqual([[104, 105]]);
  });

  it("sends keystrokes once open, and not before", async () => {
    const t = setup();
    await t.controller.start();

    t.controller.send(new Uint8Array([97]));
    expect(t.sockets[0].sent).toEqual([]);

    t.sockets[0].open();
    t.controller.send(new Uint8Array([98]));
    expect(t.sockets[0].sent).toEqual([[98]]);
  });

  it("explains a console that cannot be attached", async () => {
    const t = setup();
    await t.controller.start();

    t.sockets[0].serverClose(1006);

    expect(last(t.states)).toMatchObject({
      kind: "error",
      id: "vm-1",
      canRetry: true,
      message: expect.stringContaining("another tab"),
    });
  });

  it("reports the machine shutting down as an ordinary end", async () => {
    const t = await live();

    t.sockets[0].serverClose(1000, "vm ended");

    expect(last(t.states)).toEqual({ kind: "ended", id: "vm-1", message: "The machine shut down." });
  });

  it("says the session is gone when the server restarts", async () => {
    const t = await live();

    t.sockets[0].serverClose(1001);

    expect(last(t.states)).toMatchObject({ kind: "ended", message: expect.stringContaining("restarting") });
  });

  it("offers to pick up again after a dropped connection", async () => {
    const t = await live();

    t.sockets[0].serverClose(1006);

    expect(last(t.states)).toMatchObject({ kind: "error", id: "vm-1", canRetry: true });
  });
});

describe("resuming", () => {
  it("resumes the machine a quota refusal pointed at", async () => {
    const t = setup({
      api: {
        createVM: async () => { throw new ApiError(409, "quota", undefined, { yours: true, id: "vm-7" }); },
        getVM: async (id) => vm("running", id),
      },
    });
    await t.controller.start();
    const { resumeId } = last(t.states) as { resumeId: string };

    await t.controller.resume(resumeId);
    t.sockets[0].open();

    expect(t.sockets[0].url).toContain("vm-7");
    expect(last(t.states)).toEqual({ kind: "live", id: "vm-7" });
  });

  it("attaches without creating anything", async () => {
    const t = setup({ api: { getVM: async (id) => vm("running", id) } });

    await t.controller.resume("vm-9");

    expect(t.api.createVM).not.toHaveBeenCalled();
    expect(t.sockets[0].url).toContain("vm-9");
  });

  it("reports a session that no longer exists", async () => {
    const t = setup({ api: { getVM: async () => { throw new ApiError(404, "vm not found"); } } });

    await t.controller.resume("vm-9");

    expect(last(t.states)).toMatchObject({ kind: "error", gone: true, canRetry: false });
  });
});

describe("ending a session", () => {
  it("deletes the machine, closes the console and returns to idle", async () => {
    const t = setup();
    await t.controller.start();
    t.sockets[0].open();

    await t.controller.end();

    expect(t.api.deleteVM).toHaveBeenCalledWith("vm-1");
    expect(t.sockets[0].closedWith?.code).toBe(1000);
    expect(t.kinds().slice(-2)).toEqual(["ending", "idle"]);
  });

  it("does not call the API when there is nothing to end", async () => {
    const t = setup();

    await t.controller.end();

    expect(t.api.deleteVM).not.toHaveBeenCalled();
    expect(last(t.states)).toEqual({ kind: "idle" });
  });

  it("says so when the machine could not be deleted, and can try again", async () => {
    const t = setup({ api: { deleteVM: async () => { throw new ApiError(500, "internal error"); } } });
    await t.controller.start();
    t.sockets[0].open();

    await t.controller.end();

    expect(last(t.states)).toMatchObject({ kind: "error", id: "vm-1", canRetry: true });
  });
});

describe("leaving the page", () => {
  it("closes the console without deleting the machine, and goes quiet", async () => {
    const t = setup();
    await t.controller.start();
    t.sockets[0].open();
    const before = t.states.length;

    t.controller.dispose();
    t.sockets[0].onclose?.({ code: 1006, reason: "" } as CloseEvent); // a late event

    expect(t.api.deleteVM).not.toHaveBeenCalled();
    expect(t.sockets[0].closedWith?.code).toBe(1000);
    expect(t.states).toHaveLength(before);
  });

  it("drops a start that finishes after the page is gone", async () => {
    let release!: (v: VM) => void;
    const t = setup({ api: { createVM: () => new Promise<VM>((r) => (release = r)) } });

    const started = t.controller.start();
    t.controller.dispose();
    release(vm("running"));
    await started;

    expect(t.sockets).toHaveLength(0);
    expect(t.kinds()).toEqual(["creating"]);
  });
});
