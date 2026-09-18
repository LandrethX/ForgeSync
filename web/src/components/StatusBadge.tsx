import type { NodeState, Presence, RepoStatus } from "../api";

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

export const REPO_STATUS: Record<RepoStatus, StateInfo> = {
  same: { tone: "good", label: "Same everywhere", description: "Every node has it, with the same default branch at the same commit." },
  differs: { tone: "warning", label: "Differs", description: "Every node has it, but the default branch or its commit differs." },
  missing: { tone: "serious", label: "Missing on a node", description: "At least one node doesn't have it." },
  unknown: {
    tone: "neutral",
    label: "Not known yet",
    description: "A node hasn't been scanned since it appeared, or a branch couldn't be read.",
  },
  deleted: {
    tone: "neutral",
    label: "Deleted",
    description:
      "Deleted on its primary. The copies on other nodes are archived and deleted after the backup period.",
  },
};

export function RepoStatusBadge({ status }: { status: RepoStatus }) {
  const info = REPO_STATUS[status] ?? REPO_STATUS.unknown;
  return (
    <span className={`status-badge tone-${info.tone}`} title={info.description}>
      <StatusIcon tone={info.tone} />
      <span>{info.label}</span>
    </span>
  );
}

const PRESENCE: Record<Presence, { tone: Tone; label: string }> = {
  present: { tone: "good", label: "Present" },
  absent: { tone: "critical", label: "Not on this node" },
  unknown: { tone: "neutral", label: "Not scanned yet" },
};

/** Icon and label for whether a node has a repository. */
export function PresenceLabel({ presence, detail }: { presence: Presence; detail?: string }) {
  const info = PRESENCE[presence] ?? PRESENCE.unknown;
  return (
    <span className="presence">
      <StatusIcon tone={info.tone} />
      <span>{detail ?? info.label}</span>
    </span>
  );
}
