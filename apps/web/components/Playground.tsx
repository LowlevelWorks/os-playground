"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { createApi } from "@/lib/api";
import { API_BASE, ENTER_AS_LF, wsBase } from "@/lib/config";
import { SessionController, type SessionState } from "@/lib/session";
import { tokenPersisted } from "@/lib/token";
import { BootRail, type LampState, type Stage } from "./BootRail";
import { TerminalView, type TerminalHandle } from "./TerminalView";

// In localStorage, not sessionStorage, so a new tab can offer to resume too.
// Only this browser's token can open the machine, so the ID is no secret.
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

const noSubscription = () => () => {};

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

function statusLine(s: SessionState, resumable: boolean): string {
    switch (s.kind) {
        case "idle":
            return resumable ? "A session from this browser is still running." : "No machine running.";
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
    // Storage does not exist during the static build, so the server snapshot
    // assumes it works; the browser's answer never changes after the first read.
    const storageBlocked = useSyncExternalStore(
        noSubscription,
        () => !tokenPersisted(),
        () => false,
    );

    useEffect(() => {
        const api = createApi(API_BASE);
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

                if (s.kind === "creating") termRef.current?.reset();
                if (s.kind === "live") termRef.current?.focus();
                if (s.kind !== "idle") setResumeId(null);
                prev = s;
                setState(s);
            },
        });
        controllerRef.current = controller;

        // Offer to pick up a session this tab started before a reload.
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

        return () => {
            probe.abort();
            controller.dispose();
            controllerRef.current = null;
        };
    }, []);

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

    const busy = state.kind === "creating" || state.kind === "starting" || state.kind === "connecting";
    const live = state.kind === "live";
    const isError = state.kind === "error";
    const canRetry = isError && state.canRetry;
    const resumeMine = isError ? state.resumeId : undefined;
    const showStart =
        state.kind === "idle" || state.kind === "ended" || (isError && !state.canRetry && !resumeMine);

    return (
        <section className="machine" data-state={state.kind} aria-label="Machine">
            <div className="panel">
                <BootRail stages={stages(state, lastStage)} />
                <p className="status" role="status" aria-live="polite" data-tone={isError ? "error" : undefined}>
                    {statusLine(state, resumeId !== null)}
                </p>
                {storageBlocked && (
                    <p className="status" data-tone="error">
                        Your browser is blocking storage, so a reload will lose this session.
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
                {showStart && (
                    <button type="button" className="btn primary" onClick={start}>
                        Start session
                    </button>
                )}
                {state.kind === "idle" && resumeId && (
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
