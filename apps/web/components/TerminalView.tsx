"use client";

import { forwardRef, useEffect, useImperativeHandle, useRef } from "react";
import type { Terminal } from "@xterm/xterm";

export const COLS = 80;
export const ROWS = 25;
const CHAR_WIDTH = 0.6; // JetBrains Mono advance width in em
const MIN_FONT = 9;
const MAX_FONT = 18;

export interface TerminalHandle {
    write(data: Uint8Array): void;
    reset(): void;
    focus(): void;
}

interface Props {
    /** Keystrokes from the visitor, as the string xterm produced. */
    onData: (data: string) => void;
    /** When false, typing is ignored (no machine attached). */
    enabled: boolean;
}

export function fontSizeFor(width: number): number {
    const size = Math.floor((width - 8) / (COLS * CHAR_WIDTH));
    return Math.min(MAX_FONT, Math.max(MIN_FONT, size));
}

export const TerminalView = forwardRef<TerminalHandle, Props>(function TerminalView(
    { onData, enabled },
    ref,
) {
    const host = useRef<HTMLDivElement>(null);
    const term = useRef<Terminal | null>(null);
    const pending = useRef<Uint8Array[]>([]);
    const onDataRef = useRef(onData);
    const enabledRef = useRef(enabled);
    onDataRef.current = onData;
    enabledRef.current = enabled;

    useImperativeHandle(ref, () => ({
        write(data) {
            if (term.current) term.current.write(data);
            else pending.current.push(data);
        },
        reset() {
            pending.current = [];
            term.current?.reset();
        },
        focus() {
            term.current?.focus();
        },
    }));

    useEffect(() => {
        const el = host.current;
        if (!el) return;
        let cancelled = false;
        let observer: ResizeObserver | undefined;
        let created: Terminal | undefined;

        (async () => {
            const [{ Terminal }] = await Promise.all([
                import("@xterm/xterm"),
                // Measuring a fallback font would put the character grid out of line.
                document.fonts?.load("400 14px 'JetBrains Mono'").catch(() => undefined),
            ]);
            if (cancelled) return;

            const t = new Terminal({
                cols: COLS,
                rows: ROWS,
                convertEol: true,
                cursorBlink: true,
                fontFamily: "'JetBrains Mono', ui-monospace, monospace",
                fontSize: fontSizeFor(el.clientWidth),
                scrollback: 0,
                theme: { background: "#12171C", foreground: "#E4E8EC", cursor: "#E8A21B" },
            });
            t.open(el);
            t.onData((d) => {
                if (enabledRef.current) onDataRef.current(d);
            });
            created = t;
            term.current = t;

            for (const chunk of pending.current) t.write(chunk);
            pending.current = [];

            observer = new ResizeObserver(() => {
                const size = fontSizeFor(el.clientWidth);
                if (t.options.fontSize !== size) t.options.fontSize = size;
            });
            observer.observe(el);
        })();

        return () => {
            cancelled = true;
            observer?.disconnect();
            created?.dispose();
            term.current = null;
        };
    }, []);

    return (
        <div
            ref={host}
            className="term-host"
            role="group"
            aria-label="Machine console, 80 columns by 25 rows"
        />
    );
});
