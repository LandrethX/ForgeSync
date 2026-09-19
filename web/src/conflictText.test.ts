import { describe, expect, it } from "vitest";
import type { Conflict } from "./api";
import {
  conflictExplanation,
  conflictFix,
  conflictSides,
  conflictTitle,
} from "./conflictText";

function conflict(over: Partial<Conflict>): Conflict {
  return {
    id: 1,
    repository_id: "11111111-1111-1111-1111-111111111111",
    full_name: "alice/demo",
    primary_node: "se",
    kind: "git_diverged",
    ref: "refs/heads/main",
    state: "open",
    details: {},
    detected_at: "2026-09-19T10:00:00Z",
    last_seen_at: "2026-09-19T10:00:00Z",
    ...over,
  };
}

describe("Actions conflicts", () => {
  it("names the variable and shows what each node has", () => {
    const c = conflict({
      kind: "actions_variable_conflict",
      ref: "REGION",
      details: { variable: "REGION", values: { se: "nordics", dk: "baltics" } },
    });
    expect(conflictTitle(c)).toBe("Variable changed differently: REGION");
    expect(conflictSides(c)).toEqual([
      ["dk", "baltics"],
      ["se", "nordics"],
    ]);
    expect(conflictExplanation(c)).toMatch(/doesn't pick one/);
    expect(conflictFix(c)).toMatch(
      /Set the variable REGION on one of the nodes/,
    );
  });

  it("says which nodes are missing a secret, and that nobody can copy one", () => {
    const c = conflict({
      kind: "actions_secret_missing",
      ref: "DEPLOY_KEY",
      details: { secret: "DEPLOY_KEY", missing: ["dk", "de"] },
    });
    expect(conflictTitle(c)).toBe("Secret DEPLOY_KEY is missing on dk, de");
    expect(conflictExplanation(c)).toMatch(
      /^dk and de haven't got the Actions secret DEPLOY_KEY/,
    );
    expect(conflictFix(c)).toMatch(/on dk and de,/);
  });

  it("reads right when only one node is missing the secret", () => {
    const c = conflict({
      kind: "actions_secret_missing",
      ref: "TOKEN",
      details: { secret: "TOKEN", missing: ["uk"] },
    });
    expect(conflictTitle(c)).toBe("Secret TOKEN is missing on uk");
    expect(conflictExplanation(c)).toMatch(/^uk hasn't got the Actions secret/);
  });
});
