import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Repository, Role, Session } from "../api";
import { NodeStreamContext, SessionContext } from "../hooks";
import { Repositories } from "./Repositories";
import { RepositoryDetail } from "./RepositoryDetail";

const demo: Repository = {
  id: "11111111-1111-1111-1111-111111111111",
  full_name: "alice/demo",
  primary_node: "",
  primary_source: "",
  first_seen_at: "2026-09-18T10:00:00Z",
  status: "missing",
  nodes: [
    {
      node: "dk",
      presence: "present",
      stale: false,
      replica: {
        node: "dk",
        present: true,
        forgejo_id: 3,
        private: true,
        fork: false,
        mirror: false,
        archived: false,
        empty: false,
        default_branch: "main",
        head_sha: "0123456789abcdef",
        checked_at: "2026-09-18T10:00:00Z",
      },
    },
    { node: "se", presence: "absent", stale: false },
  ],
};

function session(role: Role): Session {
  return {
    subject: "x",
    username: "u",
    name: "U",
    role,
    source: "sceneid",
    expires_at: "2026-09-18T18:00:00Z",
  };
}

function wrap(role: Role, children: ReactNode) {
  return (
    <SessionContext.Provider value={session(role)}>
      <NodeStreamContext.Provider
        value={{ nodes: undefined, connection: "live" }}
      >
        {children}
      </NodeStreamContext.Provider>
    </SessionContext.Provider>
  );
}

function mockApi() {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    const path = url.replace(/^\/api\/v1/, "");
    let body: unknown = {};
    if (path.startsWith("/repositories?"))
      body = { total: 1, counts: { missing: 1, same: 4 }, items: [demo] };
    else if (path === "/inventory")
      body = { running: false, interval_seconds: 300, nodes: [] };
    else if (
      path === `/repositories/${demo.id}` &&
      !init?.method?.startsWith("PUT")
    )
      body = demo;
    else if (path === `/repositories/${demo.id}/primary`)
      body = { primary_node: "dk", previous: "" };
    else if (path.startsWith("/conflicts?"))
      body = { total: 0, counts: {}, items: [] };
    return new Response(JSON.stringify(body), { status: 200 });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => vi.unstubAllGlobals());

describe("Repositories", () => {
  it("shows each node's side with an icon and text, and status counts", async () => {
    mockApi();
    render(wrap("viewer", <Repositories />));
    const row = (
      await screen.findByRole("link", { name: "alice/demo" })
    ).closest("tr")!;
    const cells = within(row);
    expect(cells.getByText("Missing on a node")).toBeTruthy();
    expect(cells.getByText("0123456")).toBeTruthy(); // dk: short head commit
    expect(cells.getByText("Not on this node")).toBeTruthy(); // se
    expect(
      screen
        .getByRole("button", { name: /All\s*5/ })
        .getAttribute("aria-pressed"),
    ).toBe("true");
    expect(screen.queryByRole("button", { name: "Scan now" })).toBeNull(); // viewers can't scan
  });

  it("lets operators start a scan", async () => {
    const fetch = mockApi();
    render(wrap("operator", <Repositories />));
    await userEvent.click(
      await screen.findByRole("button", { name: "Scan now" }),
    );
    const call = fetch.mock.calls.find(
      ([u]) => u === "/api/v1/inventory/scan",
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
    expect(
      (call[1].headers as Record<string, string>)["X-ForgeSync-CSRF"],
    ).toBe("1");
  });
});

describe("RepositoryDetail", () => {
  it("only shows the primary as text to non-administrators", async () => {
    mockApi();
    render(wrap("operator", <RepositoryDetail id={demo.id} />));
    await screen.findByText("Only administrators can change it.", {
      exact: false,
    });
    expect(screen.queryByLabelText("Primary")).toBeNull();
  });

  it("lets administrators set the primary", async () => {
    const fetch = mockApi();
    render(wrap("administrator", <RepositoryDetail id={demo.id} />));
    const select = await screen.findByLabelText("Primary");
    expect(
      within(select).getByRole("option", { name: "se (doesn't have it)" }),
    ).toBeTruthy();
    await userEvent.selectOptions(select, "dk");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect((await screen.findByRole("status")).textContent).toBe(
      "Primary set to dk.",
    );
    const call = fetch.mock.calls.find(
      ([u]) => u === `/api/v1/repositories/${demo.id}/primary`,
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("PUT");
    expect(call[1].body).toBe(JSON.stringify({ node: "dk" }));
  });
});

describe("ReplicationPanel", () => {
  const withReplication = (enabled: boolean): Repository => ({
    ...demo,
    primary_node: "dk",
    replication: {
      enabled,
      replicas: [
        {
          node: "se",
          state: "conflict",
          detail: "1 ref(s) need a person; see Conflicts",
          last_attempt_at: "2026-09-18T10:00:00Z",
          out_of_sync_since: "2026-09-18T09:00:00Z",
          refs_updated: 2,
        },
      ],
    },
  });

  function mockRepo(repo: Repository) {
    const fn = vi.fn(async (url: string, init?: RequestInit) => {
      const path = url.replace(/^\/api\/v1/, "");
      if (
        path === `/repositories/${repo.id}` &&
        (init?.method ?? "GET") === "GET"
      )
        return new Response(JSON.stringify(repo));
      if (path.endsWith("/replicate"))
        return new Response(JSON.stringify({ queued: true, running: false }), {
          status: 202,
        });
      if (path.startsWith("/conflicts?"))
        return new Response(
          JSON.stringify({ total: 0, counts: {}, items: [] }),
        );
      return new Response("{}");
    });
    vi.stubGlobal("fetch", fn);
    return fn;
  }

  it("says when replication is off", async () => {
    mockRepo(withReplication(false));
    render(wrap("administrator", <RepositoryDetail id={demo.id} />));
    expect(
      await screen.findByText(/replication.enabled in its config/),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Replicate now" })).toBeNull();
  });

  it("shows each replica's state with a label and detail", async () => {
    mockRepo(withReplication(true));
    render(wrap("viewer", <RepositoryDetail id={demo.id} />));
    const row = (await screen.findByText("Conflict")).closest("tr")!;
    expect(
      within(row).getByText("1 ref(s) need a person; see Conflicts"),
    ).toBeTruthy();
    expect(within(row).getByText("Never")).toBeTruthy(); // never in sync
    expect(screen.queryByRole("button", { name: "Replicate now" })).toBeNull(); // viewers can't
  });

  it("lets operators start replication", async () => {
    const fetch = mockRepo(withReplication(true));
    render(wrap("operator", <RepositoryDetail id={demo.id} />));
    await userEvent.click(
      await screen.findByRole("button", { name: "Replicate now" }),
    );
    expect(await screen.findByText("Replication started.")).toBeTruthy();
    const call = fetch.mock.calls.find(([u]) =>
      (u as string).endsWith("/replicate"),
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
  });
});

describe("deleted repositories", () => {
  it("say the copies are archived, and until when", async () => {
    const deleted: Repository = {
      ...demo,
      primary_node: "se",
      primary_source: "owner",
      status: "deleted",
      deleted_at: "2026-09-19T10:30:00Z",
      archives: [
        {
          id: 1,
          node: "dk",
          original_name: "alice/demo",
          archived_name: "alice--demo--20260919-103000",
          state: "archived",
          archived_at: "2026-09-19T10:30:00Z",
          delete_after: "2026-10-19T10:30:00Z",
        },
      ],
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        const path = url.replace(/^\/api\/v1/, "");
        const body =
          path === `/repositories/${demo.id}`
            ? deleted
            : path.startsWith("/conflicts?")
              ? { total: 0, counts: {}, items: [] }
              : {};
        return new Response(JSON.stringify(body), { status: 200 });
      }),
    );
    render(wrap("viewer", <RepositoryDetail id={demo.id} />));
    expect(
      await screen.findByRole("heading", { name: "Deleted on the primary" }),
    ).toBeTruthy();
    expect(screen.getByText("alice--demo--20260919-103000")).toBeTruthy();
    expect(
      screen.getByText(/in the archive organization · deleted after/),
    ).toBeTruthy();
    expect(screen.getAllByText("Deleted").length).toBeGreaterThan(0);
  });
});

describe("IssuesPanel", () => {
  it("lists replicated issues with their number on each node", async () => {
    const synced: Repository = {
      ...demo,
      primary_node: "se",
      primary_source: "owner",
      status: "same",
      replication: { enabled: true, replicas: [] },
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        const path = url.replace(/^\/api\/v1/, "");
        let body: unknown = {};
        if (path === `/repositories/${demo.id}`) body = synced;
        else if (path === `/repositories/${demo.id}/issues`)
          body = {
            issues: [
              {
                id: "a",
                title: "Crash on start",
                state: "open",
                author: "bob",
                origin_node: "dk",
                created_at: "2026-09-19T10:00:00Z",
                copies: {
                  dk: { number: 2, forgejo_id: 1 },
                  se: { number: 3, forgejo_id: 2 },
                },
                comments: 4,
                numbers_differ: true,
              },
            ],
          };
        else if (path.startsWith("/conflicts?"))
          body = { total: 0, counts: {}, items: [] };
        return new Response(JSON.stringify(body), { status: 200 });
      }),
    );
    render(wrap("viewer", <RepositoryDetail id={demo.id} />));
    const panel = (
      await screen.findByRole("heading", { name: "Issues" })
    ).closest("section")!;
    const row = within(panel)
      .getByRole("rowheader", { name: /Crash on start/ })
      .closest("tr")!;
    expect(within(row).getByText("#2")).toBeTruthy();
    expect(within(row).getByText("#3")).toBeTruthy();
    expect(within(row).getByText("4")).toBeTruthy();
    expect(
      within(panel).getByText(/1 has a different number on some node/),
    ).toBeTruthy();
  });
});
