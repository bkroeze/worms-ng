# Browser support and release evidence

The browser client is a Gio WebAssembly application served by `worms-server`. The
server and the client are released together: the embedded client calls the same
`/api/v1` origin that serves `index.html`.

## Supported matrix

The supported release matrix is intentionally narrow. A browser is supported only
when it passes the real-browser smoke procedure below at every listed viewport.
The browser runs in a normal (non-emulated) Chromium engine; device scale may be
1 or 2.

| Browser | Versions | Viewports (CSS px) | Status |
| --- | --- | --- | --- |
| Google Chrome / Chromium | 120+ | 1280x720, 320x480 | release-gated |
| Microsoft Edge | 120+ (Chromium) | 1280x720, 320x480 | release-gated |
| Chrome Android | 120+ | — | not release-gated; use Chromium 320x480 smoke |
| Firefox | current | — | not supported/release-tested |
| Safari/WebKit | current | — | not supported/release-tested |

The release gate intentionally covers one desktop and one narrow responsive
profile. The 320x480 profile is a CSS viewport smoke, not a claim that every
mobile browser/device combination is supported. Device scale may be 1 or 2.
Both profiles use checked-in coordinate defaults; set `BROWSER_START_X`,
`BROWSER_START_Y`, `BROWSER_BRAINS_X`, `BROWSER_BRAINS_Y`, `BROWSER_INSPECT_X`,
and `BROWSER_INSPECT_Y` only when validating a different browser image.

The minimum release-gated desktop viewport is 1280x720. Smaller desktop
windows are not supported outside the explicit 320x480 responsive smoke.

## Reproducible browser smoke

Install Chromium and the versioned `agent-browser` CLI once. A clean checkout can
then run the isolated release smoke with:

```sh
npx --yes agent-browser install
BROWSER_ARTIFACT_DIR=dist/browser-smoke make browser-smoke
```

`make browser-smoke` builds fresh content-hashed WASM assets and a server,
creates a temporary SQLite database, serves them on an ephemeral loopback port,
and runs `scripts/browser-smoke.sh`. The script uses real Chromium pointer and
keyboard events (not a Gio model method or an API-only transition), records
snapshots/screenshots, browser console/page-error output, HAR/trace, and every
request. Temporary server/database files are retained when the command fails;
set `BROWSER_KEEP_TEMP=1` to retain them on success as well.

To repeat the release matrix locally:

```sh
for viewport in 1280x720 320x480; do
  BROWSER_VIEWPORT="$viewport" BROWSER_ARTIFACT_DIR="dist/browser-$viewport" \
    make browser-smoke
done
```

The script accepts `AGENT_BROWSER=agent-browser` (or an equivalent executable),
`BROWSER_PORT`, `BROWSER_TIMEOUT`, and the coordinate profile variables listed
above. It must not be pointed at a production origin; it starts and tears down
its own loopback service.

## Observed-state assertions

A passing run records evidence for all of these assertions:

1. `/` is non-empty HTML and reaches `complete`; `index.html` references
   content-hashed `.wasm` and `.js` assets, and every asset response is 200 with
   the expected MIME type.
2. The browser has a visible, non-zero Gio canvas. The console, page-error
   stream, and request log contain no errors; any request at HTTP 400 or above
   fails the run.
3. Fetches observe API `v1`, health `status: ok`, SQLite marker, and build/schema
   metadata from the same origin.
4. A real Chromium pointer press/release on **start game** causes authoritative
   `POST /api/v1/games` and resume. The created `ui-*` game is checked in the
   SQLite-backed API.
5. A real Gio direction key commits a move/tick, `Escape` commits pause, a
   reload resumes the same game, and a second `Escape` commits resume. The
   script retains API state before/after each transition and screenshots.
6. The rendered brain inspector is opened by ID and requests `/brains/{id}/inspect`;
   the inspector snapshot/screenshot and network evidence are retained.
7. Critical controls and the canvas remain visible at both 1280x720 and 320x480.

## Accessibility and resize review

For each supported viewport, reviewers record:

- keyboard focus is visible and `Space`, `Escape`, `1`–`9`, and all six direction
  keys operate the setup/play/pause flow;
- no decision control or score is clipped or overlaps the board/HUD;
- text and ownership cues remain readable without color alone (shape, labels, or
  contrast cues are present);
- `prefers-reduced-motion` and the in-game reduced-motion toggle remove animation
  without removing capture information; and
- resizing the browser preserves the current screen, cursor, and authoritative
  game state.

Attach the screenshots, snapshot output, browser errors, and API request log to the
release record. A failed assertion blocks that browser/viewport combination; do
not replace it with a DOM-only or unit-test result.
