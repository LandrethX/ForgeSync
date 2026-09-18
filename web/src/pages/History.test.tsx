import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HistoryEvent } from "../api";
import { describe as describeEvent, targetLink } from "../eventText";
import { History } from "./History";

const events: HistoryEvent[] = [
  { id: "n4", at: "2026-09-18T10:05:00Z", category: "node", actor: "forgesync", action: "node.state_changed", target: "dk", details: { from: "HEALTHY", to: "UNREACHABLE", error: "refused" } },
  { id: "a3", at: "2026-09-18T10:04:00Z", category: "repo", actor: "sceneid:alice", action: "repo.set_primary", target: "alice/demo", details: { repository_id: "11111111-1111-1111-1111-111111111111", from: "", to: "se" } },
  { id: "a2", at: "2026-09-18T10:03:00Z", category: "conflict", actor: "sceneid:bob", action: "conflict.acknowledged", target: "alice/demo", details: { conflict_id: 7, note: "on it" } },
];

describe("event text", () => {
  it("describes known actions and links to their targets", () => {
    expect(describeEvent(events[0]!)).toBe("Healthy → Unreachable");
    expect(describeEvent(events[1]!)).toBe("Primary changed from not set to se");
    expect(describeEvent(events[2]!)).toBe("Acknowledged a conflict: “on it”");
    expect(describeEvent({ ...events[0]!, action: "future.thing" })).toBe("future.thing");
    expect(targetLink(events[0]!)).toBe("/nodes/dk");
    expect(targetLink(events[1]!)).toBe("/repositories/11111111-1111-1111-1111-111111111111");
    expect(targetLink(events[2]!)).toBe("/conflicts/7");
  });
});

function mockApi() {
  const fn = vi.fn(async (url: string) => {
    const u = new URL(url, "http://x");
    if (u.pathname === "/api/v1/history/actors") return new Response(JSON.stringify(["forgesync", "sceneid:alice"]));
    if (u.pathname === "/api/v1/history") {
      const body = u.searchParams.get("cursor")
        ? { items: [{ ...events[2]!, id: "a1", action: "session.sign_in", category: "session", target: "10.0.0.9", details: {} }] }
        : { items: events, next_cursor: "c1" };
      return new Response(JSON.stringify(body));
    }
    return new Response("{}");
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

const historyCalls = (fetch: ReturnType<typeof mockApi>) =>
  fetch.mock.calls.map(([u]) => new URL(u as string, "http://x")).filter((u) => u.pathname === "/api/v1/history");

beforeEach(() => window.history.replaceState(null, "", "/audit"));
afterEach(() => vi.unstubAllGlobals());

describe("History page", () => {
  it("lists events with readable text and links", async () => {
    mockApi();
    render(<History />);
    const row = (await screen.findByText("Healthy → Unreachable")).closest("tr")!;
    expect(within(row).getByRole("link", { name: "dk" }).getAttribute("href")).toBe("/nodes/dk");
    expect(screen.getAllByRole("link", { name: "alice/demo" })[0]!.getAttribute("href")).toBe("/repositories/11111111-1111-1111-1111-111111111111");
  });

  it("sends filters to the API and keeps them in the address bar", async () => {
    const fetch = mockApi();
    render(<History />);
    await screen.findByText("Healthy → Unreachable");
    // Default range: last 7 days.
    expect(historyCalls(fetch)[0]!.searchParams.get("from")).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: "Sign-ins" }));
    await userEvent.selectOptions(screen.getByLabelText("Who"), "sceneid:alice");
    await waitFor(() => {
      const last = historyCalls(fetch).at(-1)!;
      expect(last.searchParams.get("category")).toBe("session");
      expect(last.searchParams.get("actor")).toBe("sceneid:alice");
    });
    expect(window.location.search).toBe("?category=session&actor=sceneid%3Aalice");
    const csv = screen.getByRole("link", { name: "Export CSV" }).getAttribute("href")!;
    expect(csv).toContain("/api/v1/history/export?category=session&actor=sceneid%3Aalice");
    expect(csv).toContain("format=csv");
  });

  it("loads older entries with the cursor", async () => {
    const fetch = mockApi();
    render(<History />);
    await userEvent.click(await screen.findByRole("button", { name: "Load older entries" }));
    expect(await screen.findByText("Signed in")).toBeTruthy();
    expect(historyCalls(fetch).at(-1)!.searchParams.get("cursor")).toBe("c1");
    expect(screen.queryByRole("button", { name: "Load older entries" })).toBeNull();
  });
});
