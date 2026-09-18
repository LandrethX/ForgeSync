import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Login } from "./Login";

type Reply = { status: number; body: unknown; headers?: Record<string, string> };

/** Routes fetch calls by path; records every call. */
function mockApi(routes: Record<string, Reply>) {
  const fn = vi.fn(async (url: string) => {
    const r = routes[url.replace(/^\/api\/v1/, "")];
    if (!r) return new Response("{}", { status: 404 });
    return new Response(JSON.stringify(r.body), { status: r.status, headers: r.headers });
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

describe("Login with SceneID", () => {
  it("links to the SceneID sign-in, keeping the current page as the return address", async () => {
    mockApi({ "/auth/config": { status: 200, body: { sceneid: true, token_sign_in: false } } });
    render(<Login onSignedIn={vi.fn()} />);
    const link = await screen.findByRole("link", { name: "Sign in with SceneID" });
    expect(link.getAttribute("href")).toBe("/api/v1/auth/login?return_to=%2Fnodes%2Fse");
    expect(screen.queryByLabelText("Admin token")).toBeNull();
  });

  it("offers the admin token only as a break-glass option when allowed", async () => {
    mockApi({ "/auth/config": { status: 200, body: { sceneid: true, token_sign_in: true } } });
    render(<Login onSignedIn={vi.fn()} />);
    await screen.findByRole("link", { name: "Sign in with SceneID" });
    expect(screen.getByText("Use the admin token instead")).toBeTruthy();
  });

  it.each([
    ["no_role", "doesn't have a ForgeSync role"],
    ["cancelled", "cancelled at SceneID"],
    ["unavailable", "Can't reach SceneID"],
    ["something-new", "Sign-in failed"],
  ])("explains signin_error=%s and removes it from the address bar", async (code, text) => {
    window.history.replaceState(null, "", `/?signin_error=${code}`);
    mockApi({ "/auth/config": { status: 200, body: { sceneid: true, token_sign_in: false } } });
    render(<Login onSignedIn={vi.fn()} />);
    expect(screen.getByRole("alert").textContent).toContain(text);
    expect(window.location.search).toBe("");
  });
});

describe("Login with the admin token (SceneID off)", () => {
  it("posts the token with the CSRF header and signs in", async () => {
    const fetch = mockApi({
      "/auth/config": { status: 200, body: { sceneid: false, token_sign_in: true } },
      "/session": { status: 200, body: session },
    });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);

    await userEvent.type(await screen.findByLabelText("Admin token"), " s3cret ");
    await userEvent.click(screen.getByRole("button", { name: "Sign in with token" }));

    expect(onSignedIn).toHaveBeenCalledWith(session);
    const call = fetch.mock.calls.find(([url]) => url === "/api/v1/session") as unknown as [string, RequestInit];
    expect(call[1].method).toBe("POST");
    expect(call[1].body).toBe(JSON.stringify({ token: "s3cret" }));
    expect((call[1].headers as Record<string, string>)["X-ForgeSync-CSRF"]).toBe("1");
  });

  it("explains a wrong token", async () => {
    mockApi({
      "/auth/config": { status: 200, body: { sceneid: false, token_sign_in: true } },
      "/session": { status: 401, body: { message: "invalid admin token" } },
    });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);
    await userEvent.type(await screen.findByLabelText("Admin token"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Sign in with token" }));

    expect((await screen.findByRole("alert")).textContent).toBe("That token isn't valid.");
    expect(onSignedIn).not.toHaveBeenCalled();
  });

  it("explains rate limiting with the wait time", async () => {
    mockApi({
      "/auth/config": { status: 200, body: { sceneid: false, token_sign_in: true } },
      "/session": { status: 429, body: { message: "too many" }, headers: { "Retry-After": "240" } },
    });
    render(<Login onSignedIn={vi.fn()} />);
    await userEvent.type(await screen.findByLabelText("Admin token"), "x");
    await userEvent.click(screen.getByRole("button", { name: "Sign in with token" }));

    expect((await screen.findByRole("alert")).textContent).toBe("Too many failed attempts. Try again in 4m.");
  });
});
