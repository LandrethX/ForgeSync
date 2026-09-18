import type { NodeState } from "../api";

export type Tone = "good" | "warning" | "serious" | "critical" | "neutral";

interface StateInfo {
  tone: Tone;
  label: string;
  description: string;
}

export const STATES: Record<NodeState, StateInfo> = {
  HEALTHY: { tone: "good", label: "Healthy", description: "Forgejo is healthy and the ForgeSync token works." },
  DEGRADED: { tone: "warning", label: "Degraded", description: "Forgejo answers but reports a problem." },
  SUSPECT: {
    tone: "warning",
    label: "Suspect",
    description: "The last check failed. It becomes Unreachable if the next ones fail too.",
  },
  AUTH_ERROR: {
    tone: "serious",
    label: "Auth error",
    description: "Forgejo rejects the ForgeSync token, or it belongs to the wrong account.",
  },
  UNREACHABLE: { tone: "critical", label: "Unreachable", description: "Several checks in a row got no answer." },
  UNKNOWN: { tone: "neutral", label: "Unknown", description: "Not checked yet." },
};

export function stateInfo(state: NodeState): StateInfo {
  return STATES[state] ?? STATES.UNKNOWN;
}

/** Status is never conveyed by colour alone: every tone has its own icon, and the label is always shown. */
export function StatusIcon({ tone }: { tone: Tone }) {
  const common = { width: 16, height: 16, viewBox: "0 0 16 16", "aria-hidden": true, className: `status-icon tone-${tone}` };
  switch (tone) {
    case "good":
      return (
        <svg {...common}>
          <circle cx="8" cy="8" r="7" fill="currentColor" />
          <path d="M4.8 8.3l2.1 2.1 4.3-4.6" fill="none" stroke="var(--icon-ink)" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "warning":
      return (
        <svg {...common}>
          <path d="M8 1.5l7 12.5H1z" fill="currentColor" strokeLinejoin="round" />
          <path d="M8 6v3.6" stroke="var(--icon-ink-dark)" strokeWidth="1.8" strokeLinecap="round" />
          <circle cx="8" cy="11.8" r="1" fill="var(--icon-ink-dark)" />
        </svg>
      );
    case "serious":
      return (
        <svg {...common}>
          <path d="M5.1 1h5.8L15 5.1v5.8L10.9 15H5.1L1 10.9V5.1z" fill="currentColor" />
          <path d="M8 4.5v4.2" stroke="var(--icon-ink-dark)" strokeWidth="1.8" strokeLinecap="round" />
          <circle cx="8" cy="11.3" r="1" fill="var(--icon-ink-dark)" />
        </svg>
      );
    case "critical":
      return (
        <svg {...common}>
          <circle cx="8" cy="8" r="7" fill="currentColor" />
          <path d="M5.5 5.5l5 5M10.5 5.5l-5 5" stroke="var(--icon-ink)" strokeWidth="1.8" strokeLinecap="round" />
        </svg>
      );
    default:
      return (
        <svg {...common}>
          <circle cx="8" cy="8" r="6.2" fill="none" stroke="currentColor" strokeWidth="1.6" strokeDasharray="2.4 2" />
        </svg>
      );
  }
}

export function StatusBadge({ state }: { state: NodeState }) {
  const info = stateInfo(state);
  return (
    <span className={`status-badge tone-${info.tone}`} title={info.description}>
      <StatusIcon tone={info.tone} />
      <span>{info.label}</span>
    </span>
  );
}
