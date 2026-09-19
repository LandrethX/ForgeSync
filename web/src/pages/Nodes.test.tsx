import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Node } from "../api";
import { NodeStreamContext } from "../hooks";
import { Nodes } from "./Nodes";

const nodes: Node[] = [
  {
    name: "dk",
    url: "http://forgejo-dk.test:3002",
    site: "DK",
    state: "UNREACHABLE",
    last_checked: new Date().toISOString(),
    consecutive_failures: 3,
    last_error: "connection refused",
  },
  {
    name: "se",
    url: "http://forgejo-se.test:3001",
    site: "SE",
    state: "HEALTHY",
    version: "16.0.5",
    last_checked: new Date().toISOString(),
    last_seen: new Date().toISOString(),
    consecutive_failures: 0,
  },
];

describe("Nodes", () => {
  it("waits for the first health check", () => {
    render(
      <NodeStreamContext.Provider
        value={{ nodes: undefined, connection: "connecting" }}
      >
        <Nodes />
      </NodeStreamContext.Provider>,
    );
    expect(screen.getByText(/Waiting for the first health check/)).toBeTruthy();
  });

  it("lists every node with its status label and a link to its page", () => {
    render(
      <NodeStreamContext.Provider value={{ nodes, connection: "live" }}>
        <Nodes />
      </NodeStreamContext.Provider>,
    );
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);

    const dk = within(rows[0]!);
    expect(dk.getByRole("link", { name: "dk" }).getAttribute("href")).toBe(
      "/nodes/dk",
    );
    expect(dk.getByText("Unreachable")).toBeTruthy();
    expect(dk.getByText("Never")).toBeTruthy();

    const se = within(rows[1]!);
    expect(se.getByText("Healthy")).toBeTruthy();
    expect(se.getByText("16.0.5")).toBeTruthy();
  });
});

/** Answers each endpoint the page asks for, by path. */
function stubApi(bodies: Record<string, unknown>) {
  return async (url: string) => {
    const path = String(url).replace(/^\/api\/v1/, "");
    const body = bodies[path];
    if (body === undefined) return new Response("{}", { status: 404 });
    return new Response(JSON.stringify(body), { status: 200 });
  };
}

const noSources = { enabled: false, pairs: [] };

describe("Nodes with webhooks", () => {
  it("shows whether each node's webhook is installed and delivering", async () => {
    const { vi } = await import("vitest");
    vi.stubGlobal(
      "fetch",
      vi.fn(
        stubApi({
          "/replication/sources": noSources,
          "/webhooks": {
            enabled: true,
            nodes: [
              {
                node: "dk",
                installed: false,
                error: "connection refused",
                deliveries: 0,
                rejected: 0,
              },
              {
                node: "se",
                installed: true,
                hook_id: 3,
                deliveries: 12,
                rejected: 1,
                last_delivery_at: new Date().toISOString(),
              },
            ],
          },
        }),
      ),
    );
    render(
      <NodeStreamContext.Provider value={{ nodes, connection: "live" }}>
        <Nodes />
      </NodeStreamContext.Provider>,
    );
    expect(
      await screen.findByRole("columnheader", { name: "Webhook" }),
    ).toBeTruthy();
    const rows = screen.getAllByRole("row").slice(1);
    expect(within(rows[0]!).getByText("Not installed")).toBeTruthy();
    expect(within(rows[1]!).getByText("Installed")).toBeTruthy();
    expect(within(rows[1]!).getByText(/12 so far\) · 1 rejected/)).toBeTruthy();
    vi.unstubAllGlobals();
  });
});

describe("what each node has sent", () => {
  it("shows when each node last copied to each other node", async () => {
    const { vi } = await import("vitest");
    const minuteAgo = new Date(Date.now() - 60_000).toISOString();
    vi.stubGlobal(
      "fetch",
      vi.fn(
        stubApi({
          "/webhooks": { enabled: false, nodes: [] },
          "/replication/sources": {
            enabled: true,
            pairs: [
              {
                from: "se",
                to: "dk",
                repositories: 4,
                in_sync: 3,
                last_success_at: minuteAgo,
                last_attempt_at: minuteAgo,
              },
            ],
          },
        }),
      ),
    );
    render(
      <NodeStreamContext.Provider value={{ nodes, connection: "live" }}>
        <Nodes />
      </NodeStreamContext.Provider>,
    );
    const heading = await screen.findByRole("heading", {
      name: "Copied from each node",
    });
    expect(heading).toBeTruthy();
    // One row, for the only node that has sent anything.
    const table = screen.getAllByRole("table")[1]!;
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(1);
    const se = within(rows[0]!);
    expect(se.getByRole("link", { name: "se" })).toBeTruthy();
    expect(se.getByText("1m ago")).toBeTruthy();
    expect(se.getByText("3 of 4 in step")).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("says nothing has been copied yet when nothing has", async () => {
    const { vi } = await import("vitest");
    vi.stubGlobal(
      "fetch",
      vi.fn(
        stubApi({
          "/webhooks": { enabled: false, nodes: [] },
          "/replication/sources": { enabled: true, pairs: [] },
        }),
      ),
    );
    render(
      <NodeStreamContext.Provider value={{ nodes, connection: "live" }}>
        <Nodes />
      </NodeStreamContext.Provider>,
    );
    expect(await screen.findByText(/Nothing has been copied yet/)).toBeTruthy();
    vi.unstubAllGlobals();
  });
});
