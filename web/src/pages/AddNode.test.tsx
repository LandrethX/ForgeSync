import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, api } from "../api";
import { AddNode } from "./AddNode";

function open() {
  render(<AddNode onAdded={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: "Add a node" }));
}

function fill() {
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "se" } });
  fireEvent.change(screen.getByLabelText("Address"), {
    target: { value: "https://forgejo-se.example.org" },
  });
  fireEvent.change(screen.getByLabelText("API token"), {
    target: { value: "a-token" },
  });
}

describe("adding a node", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("says what the node still needs before anything is typed", () => {
    open();
    expect(screen.getByText(/ALLOWED_HOST_LIST/)).toBeTruthy();
    expect(screen.getByText(/LFS_START_SERVER/)).toBeTruthy();
  });

  it("checks without storing anything", async () => {
    const check = vi.spyOn(api, "checkNode").mockResolvedValue({
      ok: true,
      service_user: "forgesync",
      findings: [
        { check: "The node answers", ok: true, blocking: true, detail: "Forgejo 16" },
      ],
    });
    const add = vi.spyOn(api, "addNode");
    open();
    fill();
    fireEvent.click(screen.getByRole("button", { name: "Check" }));
    await waitFor(() => expect(check).toHaveBeenCalled());
    expect(add).not.toHaveBeenCalled();
    expect(screen.getByText("The node answers")).toBeTruthy();
  });

  // The report is the point of a refusal, so it has to be shown rather
  // than replaced by a one-line error.
  it("shows why a node was refused", async () => {
    vi.spyOn(api, "addNode").mockRejectedValue(
      new ApiError(422, "unprocessable", undefined, {
        ok: false,
        findings: [
          {
            check: "That account is a site admin",
            ok: false,
            blocking: true,
            detail: "make forgesync an administrator on the node",
          },
        ],
      }),
    );
    open();
    fill();
    fireEvent.click(screen.getByRole("button", { name: "Add the node" }));
    await waitFor(() =>
      expect(screen.getByText("That account is a site admin")).toBeTruthy(),
    );
    expect(
      screen.getByText(/make forgesync an administrator on the node/),
    ).toBeTruthy();
    expect(screen.getByText(/Nothing was stored/)).toBeTruthy();
  });

  it("says what happens next once the node is added", async () => {
    vi.spyOn(api, "addNode").mockResolvedValue({
      ok: true,
      service_user: "forgesync",
      findings: [],
    });
    open();
    fill();
    fireEvent.click(screen.getByRole("button", { name: "Add the node" }));
    await waitFor(() => expect(screen.getByText(/was added/)).toBeTruthy());
    // The token is cleared, never shown again.
    expect((screen.getByLabelText("API token") as HTMLInputElement).value).toBe("");
  });

  it("will not submit without the three things it needs", () => {
    open();
    expect(
      (screen.getByRole("button", { name: "Add the node" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    fill();
    expect(
      (screen.getByRole("button", { name: "Add the node" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });
});
