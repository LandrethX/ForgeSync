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
  return { subject: "x", username: "u", name: "U", role, source: "sceneid", expires_at: "2026-09-18T18:00:00Z" };
}

function wrap(role: Role, children: ReactNode) {
  return (
    <SessionContext.Provider value={session(role)}>
      <NodeStreamContext.Provider value={{ nodes: undefined, connection: "live" }}>{children}</NodeStreamContext.Provider>
    </SessionContext.Provider>
  );
}

function mockApi() {
  const fn = vi.fn(async (url: string) => {
    const path = url.replace(/^\/api\/v1/, "");
    let body: unknown = {};
    if (path.startsWith("/users") && !path.endsWith("/home")) body = { total: 1, items: [alice] };
    else if (path === `/users/${alice.id}/home`) body = { home_node: "se", previous: "dk" };
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
    const row = (await screen.findByRole("rowheader", { name: "alice" })).closest("tr")!;
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
    const call = fetch.mock.calls.find(([u]) => u === `/api/v1/users/${alice.id}/home`) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("PUT");
    expect(call[1].body).toBe('{"node":"se"}');
  });
});
