import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Login } from "./Login";

type Reply = {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
};

/** Routes fetch calls by path; records every call. */
function mockApi(routes: Record<string, Reply>) {
  const fn = vi.fn(async (url: string) => {
    const r = routes[url.replace(/^\/api\/v1/, "")];
    if (!r) return new Response("{}", { status: 404 });
    return new Response(JSON.stringify(r.body), {
      status: r.status,
      headers: r.headers,
    });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

const session = {
  subject: "admin-token",
  username: "admin-token",
  name: "Admin token",
  role: "administrator",
  source: "web-token",
  expires_at: "2026-09-18T17:00:00Z",
};

beforeEach(() => window.history.replaceState(null, "", "/nodes/se"));
afterEach(() => vi.unstubAllGlobals());

describe("signing in with a ForgeSync account", () => {
  it("sends the username and password, and keeps the token out of the way", async () => {
    const fetch = mockApi({
      "/auth/config": { status: 200, body: { token_sign_in: true } },
      "/session": { status: 200, body: session },
    });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);

    // There is no SceneID button: SceneID signs people in to the nodes.
    expect(screen.queryByRole("link", { name: /SceneID/ })).toBeNull();
    await userEvent.type(
      await screen.findByLabelText("ForgeSync account"),
      "khav",
    );
    await userEvent.type(screen.getByLabelText("Password"), "a-long-one-here");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const call = fetch.mock.calls.find(
      ([u]) => u === "/api/v1/session",
    ) as unknown as [string, RequestInit];
    expect(JSON.parse(String(call[1].body))).toEqual({
      username: "khav",
      password: "a-long-one-here",
    });
    expect(onSignedIn).toHaveBeenCalledWith(session);
    // The token is there, but folded away as the break-glass option.
    expect(screen.getByText("Use the admin token instead")).toBeTruthy();
  });

  it("says the same thing whichever part was wrong", async () => {
    mockApi({
      "/auth/config": { status: 200, body: { token_sign_in: true } },
      "/session": {
        status: 401,
        body: { message: "that username and password don't match an account" },
      },
    });
    render(<Login onSignedIn={vi.fn()} />);
    await userEvent.type(
      await screen.findByLabelText("ForgeSync account"),
      "khav",
    );
    await userEvent.type(screen.getByLabelText("Password"), "not-the-one");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "That username and password don't match an account.",
    );
  });
});

describe("Login with the admin token", () => {
  it("posts the token with the CSRF header and signs in", async () => {
    const fetch = mockApi({
      "/auth/config": {
        status: 200,
        body: { sceneid: false, token_sign_in: true },
      },
      "/session": { status: 200, body: session },
    });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);

    await userEvent.type(
      await screen.findByLabelText("Admin token"),
      " s3cret ",
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Sign in with token" }),
    );

    expect(onSignedIn).toHaveBeenCalledWith(session);
    const call = fetch.mock.calls.find(
      ([url]) => url === "/api/v1/session",
    ) as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
    expect(call[1].body).toBe(JSON.stringify({ token: "s3cret" }));
    expect(
      (call[1].headers as Record<string, string>)["X-ForgeSync-CSRF"],
    ).toBe("1");
  });

  it("explains a wrong token", async () => {
    mockApi({
      "/auth/config": {
        status: 200,
        body: { sceneid: false, token_sign_in: true },
      },
      "/session": { status: 401, body: { message: "invalid admin token" } },
    });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);
    await userEvent.type(await screen.findByLabelText("Admin token"), "nope");
    await userEvent.click(
      screen.getByRole("button", { name: "Sign in with token" }),
    );

    expect((await screen.findByRole("alert")).textContent).toBe(
      "That token isn't valid.",
    );
    expect(onSignedIn).not.toHaveBeenCalled();
  });

  it("explains rate limiting with the wait time", async () => {
    mockApi({
      "/auth/config": {
        status: 200,
        body: { sceneid: false, token_sign_in: true },
      },
      "/session": {
        status: 429,
        body: { message: "too many" },
        headers: { "Retry-After": "240" },
      },
    });
    render(<Login onSignedIn={vi.fn()} />);
    await userEvent.type(await screen.findByLabelText("Admin token"), "x");
    await userEvent.click(
      screen.getByRole("button", { name: "Sign in with token" }),
    );

    expect((await screen.findByRole("alert")).textContent).toBe(
      "Too many failed attempts. Try again in 4m.",
    );
  });
});
