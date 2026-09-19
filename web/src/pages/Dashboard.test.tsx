import { render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Controller } from "../api";
import { NodeStreamContext } from "../hooks";
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
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
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
      return new Response(JSON.stringify(body), { status: 200 });
    }),
  );
}

afterEach(() => vi.unstubAllGlobals());

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
