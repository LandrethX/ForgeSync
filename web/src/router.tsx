// A minimal history-API router: the app has a handful of flat routes, which
// doesn't justify a routing dependency.
import { useEffect, useState, type AnchorHTMLAttributes, type MouseEvent } from "react";

const NAVIGATE_EVENT = "forgesync:navigate";

export function navigate(to: string) {
  if (to === window.location.pathname) return;
  window.history.pushState(null, "", to);
  window.dispatchEvent(new Event(NAVIGATE_EVENT));
}

export function usePath(): string {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const update = () => setPath(window.location.pathname);
    window.addEventListener("popstate", update);
    window.addEventListener(NAVIGATE_EVENT, update);
    return () => {
      window.removeEventListener("popstate", update);
      window.removeEventListener(NAVIGATE_EVENT, update);
    };
  }, []);
  return path;
}

/** Matches "/nodes/:name"-style patterns; returns the params or null. */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = segments(pattern);
  const a = segments(path);
  if (p.length !== a.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    const seg = p[i] ?? "";
    const val = a[i] ?? "";
    if (seg.startsWith(":")) {
      if (!val) return null;
      params[seg.slice(1)] = decodeURIComponent(val);
    } else if (seg !== val) {
      return null;
    }
  }
  return params;
}

// "/" -> [""], "/nodes/" -> ["", "nodes"]: trailing slashes don't matter.
function segments(path: string): string[] {
  const trimmed = path.replace(/\/+$/, "");
  return trimmed === "" ? [""] : trimmed.split("/");
}

type LinkProps = AnchorHTMLAttributes<HTMLAnchorElement> & { to: string };

/** An <a> that navigates client-side, but still opens new tabs normally. */
export function Link({ to, onClick, ...rest }: LinkProps) {
  const handle = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e);
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  };
  return <a href={to} onClick={handle} {...rest} />;
}
