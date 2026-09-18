import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Login } from "./Login";

function mockFetch(status: number, body: unknown, headers: Record<string, string> = {}) {
  const fn = vi.fn(async () => new Response(JSON.stringify(body), { status, headers }));
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => vi.unstubAllGlobals());

describe("Login", () => {
  it("posts the token with the CSRF header and signs in", async () => {
    const fetch = mockFetch(200, { subject: "web:admin", expires_at: "2026-09-18T17:00:00Z" });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);

    await userEvent.type(screen.getByLabelText("Admin token"), " s3cret ");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(onSignedIn).toHaveBeenCalledOnce();
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/session");
    expect(init.method).toBe("POST");
    expect(init.body).toBe(JSON.stringify({ token: "s3cret" }));
    expect((init.headers as Record<string, string>)["X-ForgeSync-CSRF"]).toBe("1");
  });

  it("explains a wrong token", async () => {
    mockFetch(401, { message: "invalid admin token" });
    const onSignedIn = vi.fn();
    render(<Login onSignedIn={onSignedIn} />);
    await userEvent.type(screen.getByLabelText("Admin token"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect((await screen.findByRole("alert")).textContent).toBe("That token isn't valid.");
    expect(onSignedIn).not.toHaveBeenCalled();
  });

  it("explains rate limiting with the wait time", async () => {
    mockFetch(429, { message: "too many" }, { "Retry-After": "240" });
    render(<Login onSignedIn={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Admin token"), "x");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect((await screen.findByRole("alert")).textContent).toBe("Too many failed attempts. Try again in 4m.");
  });

  it("keeps the button disabled until a token is entered", () => {
    render(<Login onSignedIn={vi.fn()} />);
    expect((screen.getByRole("button", { name: "Sign in" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
