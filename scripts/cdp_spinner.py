"""Measure the sidebar spinner on a running motita gateway, in a real browser.

The properties this asserts are the ones the naive version got wrong, and each
one is MEASURED rather than read off the markup:

  * PLACEMENT: the row's title must not move by a pixel when the spinner comes
    and goes. The previous in-flow <svg> pushed the title, the timestamp and the
    facts sideways for the whole duration of a run.
  * ANIMATION: the ring must actually turn. `animate-spin` emits no rule in this
    build (Tailwind's `animation` plugin is off), so a class check passes over a
    static icon; the computed `transform` is read twice and must differ.
  * TRANSITION: it must not jump-cut in either direction. Sampled from the page
    at 40 ms, at least one sample must land strictly between the two states.
  * LIFECYCLE: up while the run is in flight, gone once the answer lands, and
    gone for good - not blanked for a single frame by a later stale read.
  * QUIET AT REST: an idle row must have no animation running behind its
    opacity of 0.

Usage: GATEWAY_URL=... GATEWAY_STATE=<token file> ./cdp_spinner.py
"""
import asyncio, base64, json, os, subprocess, sys, time, urllib.request

PORT = int(os.environ.get("CDP_PORT", "9355"))
CHROME = os.environ.get("CHROME", os.path.expanduser(
    "~/.hermes/cache/chrome/chrome-headless-shell-linux64/chrome-headless-shell"))
SHOTS = os.environ.get("SHOTS_DIR", "/tmp/motita-spinner")


class CDP:
    """A CDP client with exactly ONE coroutine reading the socket.

    Two readers raise `cannot call recv while another coroutine is already
    running recv`, and a collector task alongside the request loop is the usual
    way to write that bug.
    """

    def __init__(self, ws):
        self.ws = ws
        self.i = 0

    async def call(self, method, **params):
        self.i += 1
        mid = self.i
        await self.ws.send(json.dumps({"id": mid, "method": method, "params": params}))
        while True:
            msg = json.loads(await self.ws.recv())
            if msg.get("id") == mid:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    async def js(self, expr):
        r = await self.call("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if r.get("exceptionDetails"):
            raise RuntimeError(str(r["exceptionDetails"]))
        return r.get("result", {}).get("value")

    async def shot(self, path):
        r = await self.call("Page.captureScreenshot", format="png")
        with open(path, "wb") as f:
            f.write(base64.b64decode(r["data"]))
        return path


# Every row, with the geometry of its title and the computed style of its
# spinner. `hasSpinner` is asserted because the element must ALWAYS be mounted:
# an element that is conditionally rendered cannot be transitioned out, which is
# why the old version could only ever cut.
SPIN = r"""
(() => {
  const out = [];
  for (const row of [...document.querySelectorAll('.session-row')]) {
    const sp = row.querySelector('.session-spinner');
    const title = row.querySelector(':scope > div.flex-1 > div');
    const t = title ? title.getBoundingClientRect() : null;
    const rr = row.getBoundingClientRect();
    const cs = sp ? getComputedStyle(sp) : null;
    const sr = sp ? sp.getBoundingClientRect() : null;
    out.push({
      title: title ? title.textContent.trim() : '',
      titleLeft: t ? Math.round(t.left * 10) / 10 : null,
      titleTop: t ? Math.round(t.top * 10) / 10 : null,
      hasSpinner: !!sp,
      opacity: cs ? Math.round(parseFloat(cs.opacity) * 1000) / 1000 : null,
      transform: cs ? cs.transform : null,
      animationName: cs ? cs.animationName : null,
      transitionProperty: cs ? cs.transitionProperty : null,
      // The ring must live INSIDE the row it belongs to. Without a positioned
      // row, `position: absolute` resolves against the sidebar instead and the
      // ring floats at the sidebar's own middle - detached from every row,
      // while the row it belongs to reads as idle. `is-running` alone cannot
      // catch that: the class and the animation are both correct.
      sideCenter: sr ? Math.round((sr.top + sr.bottom) / 2) : null,
      rowCenter: Math.round((rr.top + rr.bottom) / 2),
      insideRow: sr ? (sr.top >= rr.top - 1 && sr.bottom <= rr.bottom + 1
                       && sr.left >= rr.left - 1 && sr.right <= rr.right + 1) : null,
      rowTop: Math.round(rr.top),
    });
  }
  return out;
})()
"""


def fail(msg):
    print(f"  FAIL {msg}")
    return 1


def gateway_says_running(base, token):
    """The gateway's own answer is the source of truth for \"is a run in flight\"."""
    raw = subprocess.run(["curl", "-s", "-H", f"Authorization: Bearer {token}",
                          f"{base}/v1/sessions"], capture_output=True, text=True).stdout
    try:
        return any(s.get("running") for s in json.loads(raw).get("sessions", []))
    except json.JSONDecodeError:
        return False


async def main(ws_url):
    os.makedirs(SHOTS, exist_ok=True)
    base = os.environ["GATEWAY_URL"]
    token = open(os.environ["GATEWAY_STATE"]).read().strip()
    failures = 0

    import websockets
    async with websockets.connect(ws_url, max_size=50 * 1024 * 1024) as ws:
        c = CDP(ws)
        await c.call("Page.enable")
        await c.call("Runtime.enable")
        await c.call("Emulation.setDeviceMetricsOverride", width=390, height=844,
                     deviceScaleFactor=1, mobile=False)
        # about:blank FIRST: a navigation that differs only in the FRAGMENT does
        # not reload, so going straight to the token URL twice leaves the first
        # page (and its cookie) in place.
        await c.call("Page.navigate", url="about:blank")
        await c.call("Page.navigate", url=f"{base}/#t={token}")
        await asyncio.sleep(2.5)
        await c.js("""(async () => {
            for (const r of await navigator.serviceWorker.getRegistrations()) await r.unregister();
            for (const k of await caches.keys()) await caches.delete(k);
        })()""")
        await c.call("Page.navigate", url="about:blank")
        await c.call("Page.navigate", url=f"{base}/#t={token}")
        await asyncio.sleep(3)

        rows = await c.js(SPIN)
        print(f"  rows at rest: {len(rows)}")
        if not rows:
            failures += fail("no session row to measure: the sidebar is empty")
        for r in rows:
            if not r["hasSpinner"]:
                failures += fail(f"row {r['title']!r} has no .session-spinner: it can never be faded out")
            elif r["opacity"] > 0.02:
                failures += fail(f"row {r['title']!r} shows a spinner while nothing is running")
            # Containment is asserted AT REST too: a ring pinned to the sidebar
            # rather than to its row is misplaced in both states, and the bug it
            # came from was invisible to a check that only read opacity.
            if r["insideRow"] is False:
                failures += fail(
                    f"row {r['title']!r} does not contain its own spinner: the ring is centred at "
                    f"y={r['sideCenter']} while its row is centred at y={r['rowCenter']} - it is "
                    f"resolving against the sidebar, not against the row")
        at_rest = {r["title"]: r["titleLeft"] for r in rows if r["titleLeft"] is not None}
        await c.shot(f"{SHOTS}/spinner-idle.png")

        # The sampling runs IN THE PAGE. Sampling from Python would miss both the
        # fade-in and the instant the spinner goes out - the two things under
        # test - because every round trip is tens of milliseconds.
        await c.js("""(() => {
            window.__samples = [];
            const t0 = performance.now();
            window.__sampler = setInterval(() => {
                const rec = {t: Math.round(performance.now() - t0), rows: []};
                for (const row of [...document.querySelectorAll('.session-row')]) {
                    const sp = row.querySelector('.session-spinner');
                    const title = row.querySelector(':scope > div.flex-1 > div');
                    const cs = sp ? getComputedStyle(sp) : null;
                    rec.rows.push({
                        title: title ? title.textContent.trim() : '',
                        left: title ? Math.round(title.getBoundingClientRect().left * 10) / 10 : null,
                        op: cs ? Math.round(parseFloat(cs.opacity) * 1000) / 1000 : null,
                        anim: cs ? cs.animationName : null,
                    });
                }
                window.__samples.push(rec);
            }, 40);
        })()""")

        # Send the turn the way the composer does: type, then press the button.
        await c.js("""(() => {
            const ta = document.querySelector('#task');
            ta.value = 'spinner probe turn';
            ta.dispatchEvent(new Event('input', {bubbles: true}));
        })()""")
        await asyncio.sleep(0.3)
        await c.js("document.querySelector('form button[type=submit]').click()")
        print("  turn sent")

        # A run really is in flight: the gateway says so, rather than a sleep
        # guessing that it is.
        started = False
        for _ in range(80):
            if gateway_says_running(base, token):
                started = True
                break
            await asyncio.sleep(0.1)
        if not started:
            failures += fail("the gateway never reported the run in flight, so nothing was measured")
        else:
            print("  the gateway reports the run in flight")

        await asyncio.sleep(0.6)
        mid = await c.js(SPIN)
        running_rows = [r for r in mid if r["opacity"] > 0.02]
        print(f"  visible spinners mid-run: {len(running_rows)}")
        if not running_rows:
            failures += fail("no spinner is visible while a run is in flight")
        else:
            r = running_rows[0]
            # The ring belongs to the ROW that is running, and to no other.
            if not r["insideRow"]:
                failures += fail(
                    f"the spinner is not inside its own row: ring centre y={r['sideCenter']}, "
                    f"row centre y={r['rowCenter']} (the absolutely positioned child is resolving "
                    f"against an ancestor other than .session-row)")
            for other in mid:
                if other["opacity"] > 0.02 and other["title"] != r["title"]:
                    failures += fail(f"more than one row shows a spinner: {other['title']!r}")
            waiting = [x for x in mid if x["opacity"] <= 0.02 and x["rowTop"] != r["rowTop"]]
            if not waiting:
                print("  (only one session row exists, so cross-row contamination is not exercised)")
            if not r["animationName"] or r["animationName"] == "none":
                failures += fail(f"the spinner has no running animation ({r['animationName']}): a static icon")
            a = r["transform"]
            await asyncio.sleep(0.25)
            mid2 = await c.js(SPIN)
            b = next((x["transform"] for x in mid2 if x["opacity"] > 0.02), None)
            print(f"  rotation: {a} -> {b}")
            if a == b:
                failures += fail("the computed transform never changes: the spinner is not rotating")
            moved = [(r["title"], at_rest.get(r["title"]), r["titleLeft"]) for r in mid
                     if r["title"] in at_rest and abs(at_rest[r["title"]] - r["titleLeft"]) > 0.5]
            print(f"  titles shifted while running: {moved}")
            if moved:
                failures += fail(f"the spinner pushes the row's content: {moved}")
        await c.shot(f"{SHOTS}/spinner-running.png")

        # Let it finish, then confirm the spinner is GONE and STAYS gone. The
        # second part is the whole bug: the gateway finishes the run before its
        # deferred releaseRunSlot, so the first list read after the answer can
        # still say running:true - and that read used to be the last one.
        for _ in range(200):
            if not gateway_says_running(base, token):
                break
            await asyncio.sleep(0.25)
        print("  the gateway reports the run finished")
        await asyncio.sleep(1.5)

        after = await c.js(SPIN)
        lingering = [r["title"] for r in after if r["opacity"] > 0.02]
        print(f"  visible after the answer: {len(lingering)} {lingering}")
        if lingering:
            failures += fail(f"a spinner is still showing after the run ended: {lingering}")
        await c.shot(f"{SHOTS}/spinner-done.png")

        samples = await c.js("clearInterval(window.__sampler); window.__samples")
        with open(f"{SHOTS}/samples.json", "w") as f:
            json.dump(samples, f)

        # The transition, from the page's own samples: each direction must pass
        # through a value strictly between 0 and 1.
        ups, downs, mid_fade, prev = [], [], 0, 0.0
        for rec in samples:
            ops = [r["op"] for r in rec["rows"] if r["op"] is not None]
            if not ops:
                continue
            cur = max(ops)
            if prev <= 0.02 and cur > 0.02:
                ups.append(rec["t"])
            if prev >= 0.98 and cur < 0.98:
                downs.append(rec["t"])
            prev = cur
            for o in ops:
                if 0.02 < o < 0.98:
                    mid_fade += 1
        print(f"  fade-in at {ups} ms, fade-out at {downs} ms")
        print(f"  samples between the two states (a transition, not a jump cut): {mid_fade}")
        if not ups:
            failures += fail("the spinner never faded in: it appeared with no transition")
        if not downs:
            failures += fail("the spinner never faded out: it disappeared with no transition")
        if mid_fade == 0:
            failures += fail("the spinner jumps straight on or off: nothing landed between the two states")

        # Per ROW, not across the whole sidebar. A single row's title must not
        # move; two DIFFERENT rows legitimately sit at different offsets (a row
        # inside a project is indented), so pooling every row's left into one set
        # fails on a correct sidebar. Each row is tracked by its POSITION in the
        # list, and a row that keeps one left for the whole turn is a row that
        # never moved.
        tracks = {}
        for rec in samples:
            for i, row in enumerate(rec["rows"]):
                if row["left"] is not None:
                    tracks.setdefault(i, []).append((rec["t"], row["left"], row["title"]))
        moved_rows = []
        for i, vals in sorted(tracks.items()):
            lefts = sorted({v[1] for v in vals})
            if len(lefts) > 1:
                moved_rows.append((vals[0][2][:40], lefts))
        print(f"  rows whose title moved during the turn: {moved_rows}")
        if moved_rows:
            failures += fail(f"a row's title moves during the turn: {moved_rows}")

        idle_anim = await c.js("""[...document.querySelectorAll('.session-spinner:not(.is-running)')]
            .map(e => getComputedStyle(e).animationName).filter(n => n && n !== 'none').length""")
        print(f"  idle rings with an animation running: {idle_anim}")
        if idle_anim:
            failures += fail(f"{idle_anim} idle row(s) spin behind an opacity of 0")

    print()
    print("VERDICT: " + ("the spinner appears, turns, fades and clears correctly"
                         if failures == 0 else f"FAILED ({failures})"))
    return 1 if failures else 0


def start_browser():
    if subprocess.run(["curl", "-sf", "-o", "/dev/null", f"http://127.0.0.1:{PORT}/json/version"],
                      capture_output=True).returncode == 0:
        return
    os.makedirs(SHOTS, exist_ok=True)
    subprocess.Popen(
        [CHROME, "--headless", f"--remote-debugging-port={PORT}",
         f"--user-data-dir={SHOTS}/cdp-profile", "--no-sandbox", "--disable-gpu", "about:blank"],
        stdout=open(f"{SHOTS}/chrome.log", "w"), stderr=subprocess.STDOUT, start_new_session=True)
    for _ in range(60):
        if subprocess.run(["curl", "-sf", "-o", "/dev/null",
                           f"http://127.0.0.1:{PORT}/json/version"], capture_output=True).returncode == 0:
            return
        time.sleep(0.25)


if __name__ == "__main__":
    start_browser()
    ws_url = None
    for _ in range(60):
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/list", timeout=1) as r:
                pages = [t for t in json.load(r) if t.get("type") == "page"]
            if pages:
                ws_url = pages[0]["webSocketDebuggerUrl"]
                break
        except Exception:
            pass
        time.sleep(0.25)
    if not ws_url:
        print("  FAIL could not reach a page target on the DevTools endpoint")
        sys.exit(1)
    sys.exit(asyncio.run(main(ws_url)))
