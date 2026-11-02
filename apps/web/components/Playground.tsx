"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { createApi } from "@/lib/api";
import { createAuthApi, loginMessage, withoutLoginParam, type User } from "@/lib/auth";
import { API_BASE, ENTER_AS_LF, wsBase } from "@/lib/config";
import { SessionController, type SessionState } from "@/lib/session";
import { BootRail, type LampState, type Stage } from "./BootRail";
import { TerminalView, type TerminalHandle } from "./TerminalView";

// In localStorage, not sessionStorage, so a new tab can offer to resume too.
// Only the signed-in owner can open the machine, so the ID is no secret.
const STORAGE_KEY = "os-playground.vm";

function store(id: string | null) {
    try {
        if (id) localStorage.setItem(STORAGE_KEY, id);
        else localStorage.removeItem(STORAGE_KEY);
    } catch {
        /* storage can be blocked; resuming is a convenience only */
    }
}

function stored(): string | null {
    try {
        return localStorage.getItem(STORAGE_KEY);
    } catch {
        return null;
    }
}

const authApi = createAuthApi(API_BASE);

/** What the page learns from its own URL when it loads. */
interface Landing {
    /** Why a sign-in that just came back did not complete, if it did not. */
    loginNote: string | null;
    /** Where "Sign in" goes; it returns to this page. */
    loginHref: string;
    /** This page's URL without ?login=, and whether that differs. */
    cleanHref: string;
    hadLoginParam: boolean;
}

let landing: Landing | undefined;

/** Read once per page load and cached, so the snapshot is stable. */
function landingSnapshot(): Landing {
    if (!landing) {
        const params = new URLSearchParams(window.location.search);
        const cleanHref = withoutLoginParam(window.location.href);
        landing = {
            loginNote: loginMessage(params.get("login")),
            loginHref: authApi.loginUrl(cleanHref),
            cleanHref,
            hadLoginParam: params.has("login"),
        };
    }
    return landing;
}

const noSubscription = () => () => {};

/** Who is signed in, as far as the page knows. */
type Auth =
    | { kind: "checking" }
    | { kind: "signedOut" }
    | { kind: "signedIn"; user: User }
    | { kind: "unreachable" };

type StageIndex = 0 | 1 | 2 | 3;

/** Which stage a state belongs to, or null when no rail is needed. */
function stageOf(s: SessionState): StageIndex | null {
    switch (s.kind) {
        case "creating":
            return 0;
        case "starting":
            return 1;
        case "connecting":
            return 2;
        case "live":
        case "ending":
            return 3;
        default:
            return null;
    }
}

function stages(state: SessionState, lastStage: StageIndex): Stage[] {
    const labels = ["Reserve", "Boot", "Attach", "Live"];
    const current = stageOf(state);
    return labels.map((label, i) => {
        let lamp: LampState = "off";
        if (state.kind === "error" || state.kind === "ended") {
            if (state.kind === "ended") lamp = i <= 3 ? "done" : "off";
            else lamp = i < lastStage ? "done" : i === lastStage ? "failed" : "off";
        } else if (current !== null) {
            lamp = i < current ? "done" : i === current ? (current === 3 ? "done" : "active") : "off";
        }
        return { label, state: lamp };
    });
}

function statusLine(s: SessionState, resumable: boolean, auth: Auth): string {
    if (s.kind === "idle") {
        switch (auth.kind) {
            case "checking":
                return "Checking sign-in…";
            case "signedOut":
                return "Sign in with Google to start a machine.";
            case "unreachable":
                return "Can't reach the server. Check that the API is running.";
        }
    }
    switch (s.kind) {
        case "idle":
            return resumable ? "Your session is still running." : "No machine running.";
        case "creating":
            return "Reserving a machine…";
        case "starting":
            return "Booting the machine…";
        case "connecting":
            return "Attaching to the console…";
        case "live":
            return "Live. Press Enter to see the prompt.";
        case "ending":
            return "Ending the session…";
        case "ended":
            return s.message;
        case "error":
            return s.message;
    }
}

export function Playground() {
    const termRef = useRef<TerminalHandle>(null);
    const controllerRef = useRef<SessionController | null>(null);
    const encoder = useMemo(() => new TextEncoder(), []);

    const [state, setState] = useState<SessionState>({ kind: "idle" });
    const [resumeId, setResumeId] = useState<string | null>(null);
    const [lastStage, setLastStage] = useState<StageIndex>(0);
    const [auth, setAuth] = useState<Auth>({ kind: "checking" });
    // null while prerendering, where there is no URL to read.
    const page = useSyncExternalStore(noSubscription, landingSnapshot, () => null);
    const loginNote = page?.loginNote ?? null;
    const loginHref = page?.loginHref ?? null;

    const api = useMemo(() => createApi(API_BASE), []);

    // A reload must not repeat the sign-in message.
    useEffect(() => {
        if (page?.hadLoginParam) window.history.replaceState(null, "", page.cleanHref);
    }, [page]);

    // Who is signed in.
    useEffect(() => {
        const probe = new AbortController();
        authApi
            .me(probe.signal)
            .then((user) => {
                if (!probe.signal.aborted) setAuth(user ? { kind: "signedIn", user } : { kind: "signedOut" });
            })
            .catch(() => {
                if (!probe.signal.aborted) setAuth({ kind: "unreachable" });
            });
        return () => probe.abort();
    }, []);

    useEffect(() => {
        let prev: SessionState = { kind: "idle" };

        const controller = new SessionController({
            api,
            wsBase: wsBase(),
            onOutput: (data) => termRef.current?.write(data),
            onState: (s) => {
                const idx = stageOf(s);
                if (idx !== null) setLastStage(idx);

                if (s.kind === "starting" || s.kind === "connecting" || s.kind === "live") {
                    store(s.id);
                } else if (s.kind === "ended") {
                    store(null);
                } else if (s.kind === "error" && s.gone) {
                    store(null);
                } else if (s.kind === "idle" && prev.kind === "ending") {
                    store(null);
                }

                if (s.kind === "error" && s.signedOut) setAuth({ kind: "signedOut" });
                if (s.kind === "creating") termRef.current?.reset();
                if (s.kind === "live") termRef.current?.focus();
                if (s.kind !== "idle") setResumeId(null);
                prev = s;
                setState(s);
            },
        });
        controllerRef.current = controller;

        return () => {
            controller.dispose();
            controllerRef.current = null;
        };
    }, [api]);

    // Once signed in, offer to pick up a session started before a reload.
    const signedIn = auth.kind === "signedIn";
    useEffect(() => {
        if (!signedIn) return;
        const id = stored();
        const probe = new AbortController();
        if (id) {
            api
                .getVM(id)
                .then((vm) => {
                    if (probe.signal.aborted) return;
                    if (vm.status === "running") setResumeId(id);
                    else store(null);
                })
                .catch(() => {
                    if (!probe.signal.aborted) store(null);
                });
        }

        return () => probe.abort();
    }, [api, signedIn]);

    const onData = useCallback(
        (data: string) => {
            const text = ENTER_AS_LF ? data.replace(/\r/g, "\n") : data;
            controllerRef.current?.send(encoder.encode(text));
        },
        [encoder],
    );

    const start = () => void controllerRef.current?.start();
    const resume = () => resumeId && void controllerRef.current?.resume(resumeId);
    const end = () => void controllerRef.current?.end();
    // The machine keeps running until its idle timeout; signing back in
    // offers to resume it. Reloading drops the open console with the page.
    const signOut = async () => {
        await authApi.signOut().catch(() => {});
        window.location.reload();
    };

    const busy = state.kind === "creating" || state.kind === "starting" || state.kind === "connecting";
    const live = state.kind === "live";
    const isError = state.kind === "error";
    const canRetry = isError && state.canRetry;
    const resumeMine = isError ? state.resumeId : undefined;
    const showStart =
        signedIn &&
        (state.kind === "idle" || state.kind === "ended" || (isError && !state.canRetry && !resumeMine && !state.signedOut));
    const showSignIn = auth.kind === "signedOut" && loginHref !== null;

    return (
        <section className="machine" data-state={state.kind} aria-label="Machine">
            {auth.kind === "signedIn" && (
                <div className="account">
                    <span>
                        Signed in as <strong>{auth.user.name ?? auth.user.email ?? "you"}</strong>
                    </span>
                    <button type="button" className="btn small" onClick={() => void signOut()}>
                        Sign out
                    </button>
                </div>
            )}
            <div className="panel">
                <BootRail stages={stages(state, lastStage)} />
                <p className="status" role="status" aria-live="polite" data-tone={isError ? "error" : undefined}>
                    {statusLine(state, resumeId !== null, auth)}
                </p>
                {loginNote && (
                    <p className="status" data-tone="error">
                        {loginNote}
                    </p>
                )}
            </div>

            <div className="screen">
                <TerminalView ref={termRef} onData={onData} enabled={live} />
                {state.kind === "idle" && (
                    <div className="screen-empty">
                        <p>The console appears here once a machine is running.</p>
                    </div>
                )}
            </div>

            <div className="controls">
                {showSignIn && (
                    <a className="btn primary" href={loginHref}>
                        Sign in with Google
                    </a>
                )}
                {showStart && (
                    <button type="button" className="btn primary" onClick={start}>
                        Start session
                    </button>
                )}
                {signedIn && state.kind === "idle" && resumeId && (
                    <button type="button" className="btn" onClick={resume}>
                        Resume session
                    </button>
                )}
                {resumeMine && (
                    <button type="button" className="btn primary" onClick={resume2(resumeMine)}>
                        Resume it
                    </button>
                )}
                {canRetry && (
                    <button type="button" className="btn primary" onClick={state.id ? resume2(state.id) : start}>
                        Try again
                    </button>
                )}
                {busy && (
                    <button type="button" className="btn" onClick={end}>
                        Cancel
                    </button>
                )}
                {(live || (isError && state.id && state.canRetry)) && (
                    <button type="button" className="btn danger" onClick={end}>
                        End session
                    </button>
                )}
            </div>
        </section>
    );

    function resume2(id: string) {
        return () => void controllerRef.current?.resume(id);
    }
}
