import { describe, expect, it } from "vitest";
import { match } from "./router";

describe("match", () => {
  it("matches static and parameter segments", () => {
    expect(match("/", "/")).toEqual({});
    expect(match("/nodes", "/nodes/")).toEqual({});
    expect(match("/nodes/:name", "/nodes/se")).toEqual({ name: "se" });
    expect(match("/nodes/:name", "/nodes/a%20b")).toEqual({ name: "a b" });
  });
  it("rejects other paths", () => {
    expect(match("/nodes", "/audit")).toBeNull();
    expect(match("/nodes/:name", "/nodes")).toBeNull();
    expect(match("/nodes/:name", "/nodes/se/extra")).toBeNull();
  });
});

describe("match edge cases", () => {
  it("doesn't let the root route swallow other paths", () => {
    expect(match("/", "/nodes")).toBeNull();
    expect(match("/", "")).toEqual({});
  });
});
