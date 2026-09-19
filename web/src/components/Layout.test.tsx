import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Session } from "../api";
import {
  LeadershipContext,
  NodeStreamContext,
  type Leadership,
} from "../hooks";
import { Layout } from "./Layout";

const session: Session = {
  subject: "s",
  username: "alice",
  name: "Alice Andersson",
  role: "administrator",
  source: "account",
  expires_at: new Date(Date.now() + 3600_000).toISOString(),
};

function show(leadership: Leadership) {
  render(
    <NodeStreamContext.Provider value={{ nodes: [], connection: "live" }}>
      <LeadershipContext.Provider value={leadership}>
        <Layout connection="live" session={session} onSignOut={() => {}}>
          <p>the page</p>
        </Layout>
      </LeadershipContext.Provider>
    </NodeStreamContext.Provider>,
  );
}

describe("standby banner", () => {
  it("says so on a standby, and where to go instead", () => {
    show({
      role: "standby",
      leaderName: "forgesync-b",
      leaderURL: "http://b:8091",
      readOnly: true,
    });
    // The connection indicator is a status too, so find the banner itself.
    expect(screen.getByText(/This controller is on standby/)).toBeTruthy();
    expect(screen.getByText(/is the one doing the work/).textContent).toContain(
      "forgesync-b",
    );
    expect(
      screen
        .getByRole("link", { name: /Open forgesync-b/ })
        .getAttribute("href"),
    ).toBe("http://b:8091");
  });

  it("says nothing on the controller doing the work", () => {
    show({ role: "leader", leaderName: "forgesync-a", readOnly: false });
    expect(screen.queryByText(/on standby/)).toBeNull();
  });

  it("says nothing while the role isn't known yet", () => {
    show({ readOnly: false });
    expect(screen.queryByText(/on standby/)).toBeNull();
  });
});
