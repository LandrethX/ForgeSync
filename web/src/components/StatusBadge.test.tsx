import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { NodeState } from "../api";
import { StatusBadge, STATES } from "./StatusBadge";

describe("StatusBadge", () => {
  it.each(Object.keys(STATES) as NodeState[])("shows a text label and a hidden icon for %s", (state) => {
    const { container } = render(<StatusBadge state={state} />);
    expect(screen.getByText(STATES[state].label)).toBeTruthy();
    const icon = container.querySelector("svg");
    expect(icon?.getAttribute("aria-hidden")).toBe("true");
  });

  it("gives every tone a distinct icon shape", () => {
    const shapes = new Map<string, string>();
    for (const state of Object.keys(STATES) as NodeState[]) {
      const { container, unmount } = render(<StatusBadge state={state} />);
      shapes.set(STATES[state].tone, container.querySelector("svg")?.innerHTML ?? "");
      unmount();
    }
    expect(new Set(shapes.values()).size).toBe(shapes.size);
  });
});
