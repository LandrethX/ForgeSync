import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Account, Role, Session } from "../api";
import { SessionContext } from "../hooks";
import { Accounts } from "./Accounts";

const khav: Account = {
  id: "a1",
  username: "khav",
  full_name: "K",
  role: "administrator",
  disabled: false,
  created_at: new Date(Date.now() - 86_400_000).toISOString(),
  created_by: "token",
  updated_at: new Date().toISOString(),
  last_sign_in: new Date(Date.now() - 3600_000).toISOString(),
};

function session(role: Role): Session {
  return {
    subject: "a1",
    username: "khav",
    name: "K",
    role,
    source: "account",
    expires_at: "2026-09-20T18:00:00Z",
  };
}

function stubApi() {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    const path = String(url).replace(/^\/api\/v1/, "");
    void init;
    let body: unknown = {};
    if (path === "/accounts" && (init?.method ?? "GET") === "GET")
      body = { total: 1, items: [khav] };
    else if (path === "/accounts") body = { ...khav, username: "ann" };
    else body = { message: "done" };
    return new Response(JSON.stringify(body), { status: 200 });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => vi.unstubAllGlobals());

describe("ForgeSync accounts", () => {
  it("says what these accounts are, and are not", async () => {
    stubApi();
    render(
      <SessionContext.Provider value={session("administrator")}>
        <Accounts />
      </SessionContext.Provider>,
    );
    expect(
      await screen.findByText(/never copied to a Forgejo node/),
    ).toBeTruthy();
    const row = (
      await screen.findByRole("rowheader", { name: /khav/ })
    ).closest("tr")!;
    expect(within(row).getByText("This is you")).toBeTruthy();
  });

  it("adds one with a role and a long enough password", async () => {
    const fetch = stubApi();
    render(
      <SessionContext.Provider value={session("administrator")}>
        <Accounts />
      </SessionContext.Provider>,
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Add an account" }),
    );
    await userEvent.type(screen.getByLabelText("Username"), "ann");
    const submit = screen.getByRole("button", { name: "Add the account" });
    await userEvent.type(screen.getByLabelText("Password"), "short");
    // 12 characters is the one rule.
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    await userEvent.clear(screen.getByLabelText("Password"));
    await userEvent.type(
      screen.getByLabelText("Password"),
      "a-long-enough-one",
    );
    await userEvent.selectOptions(screen.getByLabelText("Role"), "operator");
    await userEvent.click(submit);

    const call = fetch.mock.calls.find(
      ([u, init]) =>
        u === "/api/v1/accounts" && (init as RequestInit)?.method === "POST",
    ) as unknown as [string, RequestInit];
    expect(JSON.parse(String(call[1].body))).toMatchObject({
      username: "ann",
      password: "a-long-enough-one",
      role: "operator",
    });
  });

  it("is not for operators", async () => {
    stubApi();
    render(
      <SessionContext.Provider value={session("operator")}>
        <Accounts />
      </SessionContext.Provider>,
    );
    expect(
      await screen.findByText(/needs the administrator role/),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });
});
