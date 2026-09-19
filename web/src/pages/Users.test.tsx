import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Role, Session, User } from "../api";
import { NodeStreamContext, SessionContext } from "../hooks";
import { Users } from "./Users";

const alice: User = {
  id: "33333333-3333-3333-3333-333333333333",
  sub: "sub-a",
  login: "alice",
  home_node: "dk",
  home_source: "registration",
  first_seen_at: "2026-09-18T10:00:00Z",
  accounts: [
    { node: "dk", login: "alice", present: true, created_by_forgesync: false },
    { node: "se", login: "alice", present: true, created_by_forgesync: true },
  ],
};

function session(role: Role): Session {
  return {
    subject: "x",
    username: "u",
    name: "U",
    role,
    source: "account",
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
    const post = init?.method === "POST";
    let body: unknown = {};
    if (path === `/users/${alice.id}/home`)
      body = { home_node: "se", previous: "dk" };
    else if (path === `/users/${alice.id}/nodes`)
      body = { login: "alice", created: ["de"] };
    else if (path === "/users" && post)
      body = {
        login: "dave",
        created: ["se", "dk"],
        refused: { de: "no token" },
      };
    else if (path.startsWith("/users")) body = { total: 1, items: [alice] };
    return new Response(JSON.stringify(body), { status: 200 });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => vi.unstubAllGlobals());

describe("Users", () => {
  it("shows each user's primary site and where they have accounts", async () => {
    mockApi();
    render(wrap("viewer", <Users />));
    const row = (
      await screen.findByRole("rowheader", { name: "alice" })
    ).closest("tr")!;
    expect(within(row).getByText("dk")).toBeTruthy();
    expect(within(row).getByText(/Where they registered/)).toBeTruthy();
    expect(within(row).getByText("Registered")).toBeTruthy();
    expect(within(row).getByText("Created by ForgeSync")).toBeTruthy();
    expect(screen.queryByLabelText("Primary site for alice")).toBeNull();
  });

  it("lets administrators change a primary site", async () => {
    const fetch = mockApi();
    render(wrap("administrator", <Users />));
    const select = await screen.findByLabelText("Primary site for alice");
    await userEvent.selectOptions(select, "se");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const call = fetch.mock.calls.find(
      ([u]) => u === `/api/v1/users/${alice.id}/home`,
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("PUT");
    expect(call[1].body).toBe('{"node":"se"}');
  });
});

describe("adding a user", () => {
  it("is only offered to administrators", async () => {
    mockApi();
    render(wrap("operator", <Users />));
    await screen.findByRole("rowheader", { name: "alice" });
    expect(screen.queryByRole("heading", { name: "Add a user" })).toBeNull();
  });

  it("creates a SceneID account on every node, with no password", async () => {
    const fetch = mockApi();
    render(
      <SessionContext.Provider value={session("administrator")}>
        <NodeStreamContext.Provider
          value={{
            nodes: [
              {
                name: "se",
                url: "http://se",
                site: "SE",
                state: "HEALTHY",
                last_checked: "2026-09-19T10:00:00Z",
                consecutive_failures: 0,
              },
              {
                name: "dk",
                url: "http://dk",
                site: "DK",
                state: "HEALTHY",
                last_checked: "2026-09-19T10:00:00Z",
                consecutive_failures: 0,
              },
            ],
            connection: "live",
          }}
        >
          <Users />
        </NodeStreamContext.Provider>
      </SessionContext.Provider>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Add a user" }));
    expect(screen.queryByLabelText(/password/i)).toBeNull();
    await userEvent.type(screen.getByLabelText("Username"), "dave");
    await userEvent.type(screen.getByLabelText("SceneID subject"), "dave-sub");
    await userEvent.selectOptions(screen.getByLabelText("Primary site"), "dk");
    await userEvent.click(screen.getByRole("button", { name: "Add the user" }));

    const call = fetch.mock.calls.find(
      ([u, init]) =>
        u === "/api/v1/users" && (init as RequestInit)?.method === "POST",
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
    expect(JSON.parse(String(call[1].body))).toMatchObject({
      login: "dave",
      subject: "dave-sub",
      home: "dk",
    });
    // What happened on each node, including what was refused.
    expect(await screen.findByText(/created on se, dk/)).toBeTruthy();
    expect(screen.getByText(/no token/)).toBeTruthy();
  });

  it("offers to create the account on the nodes that haven't got it", async () => {
    const fetch = mockApi();
    render(
      <SessionContext.Provider value={session("administrator")}>
        <NodeStreamContext.Provider
          value={{
            nodes: [
              {
                name: "dk",
                url: "http://dk",
                site: "DK",
                state: "HEALTHY",
                last_checked: "2026-09-19T10:00:00Z",
                consecutive_failures: 0,
              },
              {
                name: "se",
                url: "http://se",
                site: "SE",
                state: "HEALTHY",
                last_checked: "2026-09-19T10:00:00Z",
                consecutive_failures: 0,
              },
              {
                name: "de",
                url: "http://de",
                site: "DE",
                state: "HEALTHY",
                last_checked: "2026-09-19T10:00:00Z",
                consecutive_failures: 0,
              },
            ],
            connection: "live",
          }}
        >
          <Users />
        </NodeStreamContext.Provider>
      </SessionContext.Provider>,
    );
    const row = (
      await screen.findByRole("rowheader", { name: "alice" })
    ).closest("tr")!;
    expect(within(row).getByText("Missing on de")).toBeTruthy();
    await userEvent.click(
      within(row).getByRole("button", { name: "Create there" }),
    );
    const call = fetch.mock.calls.find(
      ([u]) => u === `/api/v1/users/${alice.id}/nodes`,
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
    expect(await within(row).findByText(/created on de/)).toBeTruthy();
  });
});
