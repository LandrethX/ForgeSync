#!/usr/bin/env python3
"""Draws the ForgeSync mark into web/public/.

The mark is a sibling of SceneGit's: flat mitred ribbons, only 0/45/90
degree edges, a thin line running with the thick one. Here it's two
ribbons passing each other -- what the controller does.

Nothing in the build runs this; the SVGs it writes are committed and
editable by hand or in any vector editor. Run it only to change the
geometry:

    python3 web/brand/mark.py

The PNG fallbacks (favicon.png, apple-touch-icon.png) are rendered from
the SVGs with a headless browser; see the comment at the end.
"""


# The ForgeSync mark: two mitred ribbons passing each other, drawn the way
# SceneGit's is -- flat, 45 degree cuts, a thin line running with the thick
# one. The favicon leaves the thin lines out, which don't survive 16px.
LIGHT, DARK = "#f66d10", "#f66d10"   # SceneGit's orange, the family colour

T = 56       # ribbon thickness
H = D = 88   # arrowhead: half-height, and reach (45 degree faces)
X0 = -180    # tail
X1 = 92      # where the head starts
OFF = 74     # distance of each ribbon from the middle
THIN = 14
SPACE = 20
TIN, TOUT = 60, 34

def path(points):
    return "M" + " L".join(f"{x:.1f},{y:.1f}" for x, y in points) + " Z"

def arrow():
    return path([(X0 + T, -T/2), (X1, -T/2), (X1, -H), (X1 + D, 0),
                 (X1, H), (X1, T/2), (X0, T/2)])

def line():
    y1 = -T/2 - SPACE
    y0 = y1 - THIN
    return path([(X0 + T + TIN + THIN, y0), (X1 - TOUT, y0),
                 (X1 - TOUT - THIN, y1), (X0 + T + TIN, y1)])

def svg(detail, pad):
    half_w = X1 + D + pad
    half_h = OFF + H + (SPACE + THIN if detail else 0) + pad
    half = max(half_w, half_h)
    one = f'<path d="{arrow()}"/>' + (f'<path d="{line()}"/>' if detail else "")
    return f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="{-half:.0f} {-half:.0f} {2*half:.0f} {2*half:.0f}" role="img" aria-label="ForgeSync">
<style>
  /* The mark follows the page it sits on; --accent in the admin UI. */
  svg {{ --mark: {LIGHT} }}
  @media (prefers-color-scheme: dark) {{ svg {{ --mark: {DARK} }} }}
</style>
<g fill="var(--mark)">
  <g transform="translate(0,{-OFF})">{one}</g>
  <g transform="translate(0,{OFF}) rotate(180)">{one}</g>
</g>
</svg>"""

import os

here = os.path.dirname(os.path.abspath(__file__))
public = os.path.join(here, "..", "public")
open(os.path.join(public, "logo.svg"), "w").write(svg(True, 24))
open(os.path.join(public, "favicon.svg"), "w").write(svg(False, 14))
print("wrote logo.svg and favicon.svg")

# favicon.png (32, transparent) and apple-touch-icon.png (180, on the dark
# tile, since iOS puts nothing behind it and the orange carries best on
# near-black) are rasterised from these, e.g. with
# Playwright's Chromium or rsvg-convert. They only need redoing when the
# geometry above changes.
