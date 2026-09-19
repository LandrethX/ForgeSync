import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Controller, Role, Session } from "../api";
import { NodeStreamContext, SessionContext } from "../hooks";
import { Dashboard } from "./Dashboard";

const controllers: Controller[] = [
  {
    name: "forgesync-a",
    url: "http://forgesync.test:8090",
    address: "192.0.2.10",
    version: "dev",
    role: "leader",
    self: false,
    started_at: new Date(Date.now() - 3600_000).toISOString(),
    last_seen_at: new Date(Date.now() - 2000).toISOString(),
  },
  {
    name: "forgesync-b",
    url: "http://forgesync-b.test:8091",
    address: "192.0.2.10",
    version: "dev",
    role: "standby",
    self: true,
    started_at: new Date(Date.now() - 600_000).toISOString(),
    last_seen_at: new Date(Date.now() - 1000).toISOString(),
  },
];

function stubApi(extra: Partial<Record<string, unknown>> = {}) {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    const path = String(url).replace(/^\/api\/v1/, "");
    let body: unknown = {};
    if (path === "/overview")
      body = {
        version: "dev",
        commit: "unknown",
        started_at: new Date(Date.now() - 600_000).toISOString(),
        role: "standby",
        leader: { name: "forgesync-a", url: "http://forgesync.test:8090" },
        database: { ok: true },
        nodes: { total: 0 },
        open_conflicts: 0,
        replication: { enabled: true, counts: {} },
        controllers,
        ...extra,
      };
    else if (path.startsWith("/transitions")) body = [];
    else if (path === "/leadership")
      body = {
        controller: "forgesync-b",
        message: "forgesync-b will take over within a lease",
      };
    void init;
    return new Response(JSON.stringify(body), { status: 200 });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => vi.unstubAllGlobals());

function wrapAs(role: Role) {
  const session: Session = {
    subject: "x",
    username: "u",
    name: "U",
    role,
    source: "sceneid",
    expires_at: "2026-09-20T18:00:00Z",
  };
  return (
    <SessionContext.Provider value={session}>
      <NodeStreamContext.Provider value={{ nodes: [], connection: "live" }}>
        <Dashboard />
      </NodeStreamContext.Provider>
    </SessionContext.Provider>
  );
}

describe("choosing which controller leads", () => {
  it("lets an administrator promote the standby from its own card", async () => {
    const fetch = stubApi();
    render(wrapAs("administrator"));
    const cards = within(
      (
        await screen.findByRole("heading", { name: "Sync controllers" })
      ).closest("section")!,
    ).getAllByRole("article");
    // The leader has no button: it's already doing the work.
    expect(
      within(cards[0]!).queryByRole("button", { name: "Make this the leader" }),
    ).toBeNull();
    await userEvent.click(
      within(cards[1]!).getByRole("button", { name: "Make this the leader" }),
    );
    const call = fetch.mock.calls.find(
      ([u]) => u === "/api/v1/leadership",
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("PUT");
    expect(JSON.parse(String(call[1].body))).toEqual({
      controller: "forgesync-b",
    });
    expect(
      await screen.findByText(/will take over within a lease/),
    ).toBeTruthy();
  });

  it("doesn't offer it to an operator", async () => {
    stubApi();
    render(wrapAs("operator"));
    await screen.findByRole("heading", { name: "Sync controllers" });
    expect(
      screen.queryByRole("button", { name: "Make this the leader" }),
    ).toBeNull();
  });

  it("says who chose the current one, and offers to go back", async () => {
    const fetch = stubApi({
      chosen: {
        controller: "forgesync-b",
        chosen_by: "sceneid:alice",
        chosen_at: new Date(Date.now() - 60_000).toISOString(),
      },
      controllers: [
        { ...controllers[0]!, role: "standby" },
        { ...controllers[1]!, role: "leader", chosen: true },
      ],
    });
    render(wrapAs("administrator"));
    expect(
      await screen.findByText(
        /forgesync-b was chosen to lead by sceneid:alice/,
      ),
    ).toBeTruthy();
    expect(screen.getByText("Chosen")).toBeTruthy();
    await userEvent.click(
      screen.getByRole("button", { name: "Follow the configured order again" }),
    );
    const call = fetch.mock.calls.find(
      ([u]) => u === "/api/v1/leadership",
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("DELETE");
  });
});

describe("controller cards", () => {
  it("shows every controller with its address and role", async () => {
    stubApi();
    render(
      <NodeStreamContext.Provider value={{ nodes: [], connection: "live" }}>
        <Dashboard />
      </NodeStreamContext.Provider>,
    );
    const heading = await screen.findByRole("heading", {
      name: "Sync controllers",
    });
    const section = heading.closest("section")!;
    const cards = within(section).getAllByRole("article");
    expect(cards).toHaveLength(2);

    const a = within(cards[0]!);
    expect(a.getByRole("heading", { name: /forgesync-a/ })).toBeTruthy();
    expect(a.getByText("Leader")).toBeTruthy();
    expect(a.getByText("192.0.2.10")).toBeTruthy();
    expect(
      a.getByRole("link", { name: "http://forgesync.test:8090" }),
    ).toBeTruthy();

    const b = within(cards[1]!);
    // The one you're looking at says so, so two tabs can't be confused.
    expect(b.getByRole("heading", { name: /forgesync-b/ })).toBeTruthy();
    expect(b.getByText("· this one")).toBeTruthy();
    expect(b.getByText("Standby")).toBeTruthy();
  });

  it("doesn't promise that a controller nobody has heard from is ready", async () => {
    stubApi({
      controllers: [{ ...controllers[0]!, role: "unknown", self: false }],
    });
    render(
      <NodeStreamContext.Provider value={{ nodes: [], connection: "live" }}>
        <Dashboard />
      </NodeStreamContext.Provider>,
    );
    expect(await screen.findByText("Not heard from")).toBeTruthy();
    expect(screen.queryByText("Standby")).toBeNull();
  });
});
