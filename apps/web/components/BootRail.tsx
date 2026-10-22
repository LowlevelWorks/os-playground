export type LampState = "off" | "active" | "done" | "failed";

export interface Stage {
    label: string;
    state: LampState;
}

const STATE_TEXT: Record<LampState, string> = {
    off: "waiting",
    active: "in progress",
    done: "done",
    failed: "failed",
};

export function BootRail({ stages }: { stages: Stage[] }) {
    return (
        <ol className="rail" aria-label="Session progress">
            {stages.map((s) => (
                <li key={s.label} className="rail-item" data-state={s.state}>
                    <span className="lamp" aria-hidden="true" />
                    <span className="rail-label">{s.label}</span>
                    <span className="sr-only">: {STATE_TEXT[s.state]}</span>
                </li>
            ))}
        </ol>
    );
}
