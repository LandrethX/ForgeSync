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
      <NodeStreamContext.Provider value={{ nodes: undefined, connection: "connecting" }}>
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
    expect(dk.getByRole("link", { name: "dk" }).getAttribute("href")).toBe("/nodes/dk");
    expect(dk.getByText("Unreachable")).toBeTruthy();
    expect(dk.getByText("Never")).toBeTruthy();

    const se = within(rows[1]!);
    expect(se.getByText("Healthy")).toBeTruthy();
    expect(se.getByText("16.0.5")).toBeTruthy();
  });
});
