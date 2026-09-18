import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Conflict, Role, Session } from "../api";
import { NodeStreamContext, SessionContext } from "../hooks";
import { ConflictDetail } from "./ConflictDetail";
import { Conflicts } from "./Conflicts";

const diverged: Conflict = {
  id: 7,
  repository_id: "11111111-1111-1111-1111-111111111111",
  full_name: "alice/demo",
  primary_node: "se",
  kind: "git_diverged",
  ref: "refs/heads/main",
  state: "open",
  details: {
    branch: "main",
    heads: { se: "a1b2c3d4e5f6a7b8", dk: "9f8e7d6c5b4a3f2e" },
    relations: [{ a: "dk", b: "se", relation: "diverged" }],
    primary: "se",
  },
  detected_at: "2026-09-18T10:00:00Z",
  last_seen_at: "2026-09-18T10:05:00Z",
};

function wrap(role: Role, children: ReactNode) {
  const session: Session = { subject: "x", username: "bob", name: "Bob", role, source: "sceneid", expires_at: "2026-09-18T18:00:00Z" };
  return (
    <SessionContext.Provider value={session}>
      <NodeStreamContext.Provider value={{ nodes: undefined, connection: "live" }}>{children}</NodeStreamContext.Provider>
    </SessionContext.Provider>
  );
}

function mockApi(items: Conflict[], counts = { open: items.length, cleared: 3 }) {
  let current = { ...diverged };
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    const path = url.replace(/^\/api\/v1/, "");
    let body: unknown = {};
    if (path.startsWith("/conflicts?")) body = { total: items.length, counts, items };
    else if (path === "/conflicts/7") body = current;
    else if (path === "/conflicts/7/acknowledge") {
      const note = JSON.parse(String(init?.body)).note as string;
      current = { ...current, acknowledged_by: "sceneid:bob", acknowledged_at: "2026-09-18T11:00:00Z", note };
      body = current;
    }
    return new Response(JSON.stringify(body), { status: 200 });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => vi.unstubAllGlobals());

describe("Conflicts", () => {
  it("lists open conflicts with each node's head", async () => {
    mockApi([diverged]);
    render(wrap("viewer", <Conflicts />));
    const row = (await screen.findByRole("link", { name: "Diverged history: main" })).closest("tr")!;
    expect(within(row).getByText("se: a1b2c3d")).toBeTruthy();
    expect(within(row).getByText("dk: 9f8e7d6")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Cleared\s*3/ })).toBeTruthy();
  });

  it("says so when nothing is open", async () => {
    mockApi([], { open: 0, cleared: 0 });
    render(wrap("viewer", <Conflicts />));
    expect(await screen.findByText("No open conflicts.")).toBeTruthy();
  });
});

describe("ConflictDetail", () => {
  it("explains the divergence and marks the primary", async () => {
    mockApi([diverged]);
    render(wrap("viewer", <ConflictDetail id={7} />));
    expect(await screen.findByText("dk and se have diverged: each has commits the other doesn't.")).toBeTruthy();
    expect(screen.getByText("Primary").closest("tr")?.textContent).toContain("se");
    expect(screen.getByText(/ForgeSync never overwrites diverged history/)).toBeTruthy();
    // Viewers can't acknowledge.
    expect(screen.queryByRole("button", { name: "Acknowledge" })).toBeNull();
  });

  it("lets operators acknowledge with a note", async () => {
    const fetch = mockApi([diverged]);
    render(wrap("operator", <ConflictDetail id={7} />));
    await userEvent.type(await screen.findByLabelText("Note for others (optional)"), "Bob is merging it");
    await userEvent.click(screen.getByRole("button", { name: "Acknowledge" }));
    expect((await screen.findByRole("status")).textContent).toBe("Saved.");
    const call = fetch.mock.calls.find(([u]) => u === "/api/v1/conflicts/7/acknowledge") as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
    expect(call[1].body).toBe(JSON.stringify({ note: "Bob is merging it" }));
    expect(await screen.findByText("Bob is merging it", { selector: "q" })).toBeTruthy();
  });
});

describe("replication conflict text", () => {
  it("names the ref and explains what happened", async () => {
    const { conflictTitle, conflictExplanation, conflictFix } = await import("../conflictText");
    const ahead: Conflict = { ...diverged, kind: "git_replica_ahead", details: { branch: "main", primary: "se", heads: { se: "a", dk: "b" } } };
    expect(conflictTitle(ahead)).toBe("Replica has its own commits: main");
    expect(conflictExplanation(ahead)).toContain("pushed commits to a replica that se doesn't have");
    expect(conflictFix(ahead)).toContain("over to se by itself");
    const tag: Conflict = { ...diverged, kind: "git_primary_rewrote", ref: "refs/tags/v1", details: { tag: "v1", primary: "se" } };
    expect(conflictTitle(tag)).toBe("History rewritten on the primary: tag v1");
  });
});
