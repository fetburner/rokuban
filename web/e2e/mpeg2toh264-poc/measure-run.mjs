// One measured run in headed Chrome (one browser per run; run sequentially).
//
//   node measure-run.mjs --out <dir> --label <name> --mode idle|file|grow
//       [--file <ts>] [--start 0|mid] [--mid <sec>] [--play 120] [--yadif]
//       [--grow-rate B/s] [--grow-lag s] [--grow-step s] [--grow-head s] [--live-back s]
//
// Writes <dir>/<label>.json with:
//   samples      state snapshot every 500 ms (currentTime, buffered, quality, poll counters)
//   procs        `ps -A -o pid,ppid,%cpu,rss,command` filtered to this run's Chrome
//                (by --user-data-dir), every 1 s, with role from --type=
//   live         grow mode: liveEdge() every 5 s (visible size, last media second)
//   media        raw CDP Media.* events (decoder names, codec, errors)
//   events       window.poc.events (waiting / playing / seek / grew / ...)
//   window       { startMs, endMs } on the Node clock; events and samples carry the same clock
import { mkdir, rm, writeFile } from "node:fs/promises";
import { basename, join, resolve } from "node:path";
import { execFile } from "node:child_process";
import { startServer } from "./serve.mjs";

const { chromium } = await import("playwright");

const argv = process.argv.slice(2);
const opt = (name, fallback) => {
  const i = argv.indexOf(name);
  return i >= 0 ? argv[i + 1] : fallback;
};
const has = (name) => argv.includes(name);

const outDir = resolve(opt("--out", "./out-measure"));
const label = opt("--label", "run");
const mode = opt("--mode", "idle");
const file = opt("--file") ? resolve(opt("--file")) : null;
const start = opt("--start", "0");
const mid = Number(opt("--mid", "0"));
const playSeconds = Number(opt("--play", "120"));
const yadif = has("--yadif");
const growRate = Number(opt("--grow-rate", "0"));
const growLag = Number(opt("--grow-lag", "0"));
const growStep = Number(opt("--grow-step", "0"));
const growHead = Number(opt("--grow-head", "0"));
const liveBack = Number(opt("--live-back", "10"));

await mkdir(outDir, { recursive: true });
const profileDir = join(outDir, `profile-${label}`);
await rm(profileDir, { recursive: true, force: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const T0 = Date.now();
const now = () => Date.now() - T0;

const srv = await startServer({
  port: 0,
  media: mode === "file" ? (file ? resolve(file, "..") : null) : null,
  grow:
    mode === "grow"
      ? { file, rate: growRate, lag: growLag, step: growStep, head: growHead }
      : null,
});

const samples = [];
const procs = [];
const live = [];
const media = [];
const psLine = /^\s*(\d+)\s+(\d+)\s+([\d.]+)\s+(\d+)\s+(.*)$/;

function samplePs() {
  return new Promise((done) =>
    execFile("/bin/ps", ["-A", "-o", "pid,ppid,%cpu,rss,command"], { maxBuffer: 1 << 26 }, (err, out) => {
      const rows = [];
      for (const line of (out ?? "").split("\n")) {
        const m = psLine.exec(line);
        if (!m || !m[5].includes(profileDir)) continue;
        const type = /--type=([a-z-]+)/.exec(m[5]);
        const role = type ? type[1] : m[5].includes("Google Chrome.app/Contents/MacOS/Google Chrome") ? "browser" : "other";
        rows.push({ pid: Number(m[1]), ppid: Number(m[2]), cpu: Number(m[3]), rssKB: Number(m[4]), role });
      }
      done({ t: now(), rows });
    }),
  );
}

const context = await chromium.launchPersistentContext(profileDir, {
  channel: "chrome",
  headless: false,
  args: ["--autoplay-policy=no-user-gesture-required"],
});
const chromeVersion = context.browser()?.version() ?? "unknown";
let exitCode = 0;
let page;
let timers = [];
try {
  page = context.pages()[0] ?? (await context.newPage());
  for (const extra of context.pages().slice(1)) await extra.close();
  page.on("pageerror", (e) => media.push({ t: now(), ev: "pageerror", p: { message: e.message } }));
  page.on("console", (m) => {
    if (m.type() === "error") media.push({ t: now(), ev: "console-error", p: { text: m.text() } });
  });
  await page.goto(`${srv.url}/index.html`);
  await page.waitForFunction(() => Boolean(window.poc), null, { timeout: 30_000 });
  await page.evaluate(() => window.poc.ready);

  const cdp = await context.newCDPSession(page);
  await cdp.send("Media.enable");
  for (const ev of ["Media.playersCreated", "Media.playerPropertiesChanged", "Media.playerEventsAdded", "Media.playerMessagesLogged", "Media.playerErrorsRaised"]) {
    cdp.on(ev, (p) => media.push({ t: now(), ev, p }));
  }

  // Sampling: ps every 1 s, page state every 500 ms, liveEdge every 5 s (grow only).
  let psBusy = false;
  let stBusy = false;
  let liveBusy = false;
  timers.push(
    setInterval(async () => {
      if (psBusy) return;
      psBusy = true;
      procs.push(await samplePs());
      psBusy = false;
    }, 1000),
  );
  timers.push(
    setInterval(async () => {
      if (stBusy) return;
      stBusy = true;
      try {
        const s = await page.evaluate(() => window.poc.state());
        samples.push({ t: Date.now() - T0, ...s, seeks: undefined, sessionCounters: undefined });
      } catch (e) {
        samples.push({ t: now(), sampleError: String(e.message ?? e) });
      }
      stBusy = false;
    }, 500),
  );
  if (mode === "grow") {
    timers.push(
      setInterval(async () => {
        if (liveBusy) return;
        liveBusy = true;
        try {
          live.push({ t: now(), ...(await page.evaluate(() => window.poc.liveEdge())) });
        } catch (e) {
          live.push({ t: now(), error: String(e.message ?? e) });
        }
        liveBusy = false;
      }, 5000),
    );
  }

  const idleStart = now();
  let windowStart = null;
  let windowEnd = null;
  const extra = {};

  if (mode === "idle") {
    windowStart = now();
    await sleep(playSeconds * 1000);
  } else {
    const url = `${srv.url}/media/${encodeURIComponent(basename(file))}`;
    const growUrl = `${srv.url}/grow/${encodeURIComponent(basename(file))}`;
    const openUrl = mode === "grow" ? growUrl : url;
    const openOpts = { yadif, growing: mode === "grow" };
    const openBegan = now();
    extra.open = await page.evaluate(([u, o]) => window.poc.open(u, o), [openUrl, openOpts]);
    extra.openWallMs = now() - openBegan;

    if (mode === "grow") {
      extra.liveAtOpen = await page.evaluate(() => window.poc.liveEdge());
      const target = Math.max(0, extra.liveAtOpen.seconds - liveBack);
      extra.seekTarget = target;
      extra.seekRecord = await page.evaluate((s) => window.poc.seek(s), target);
    } else if (start === "mid") {
      extra.seekRecord = await page.evaluate((s) => window.poc.seek(s), mid);
    }
    // Window opens once the playhead is past the seek target (or immediately from the start).
    const want = start === "mid" || mode === "grow" ? (extra.seekTarget ?? mid) : null;
    if (want != null) {
      const deadline = Date.now() + 45_000;
      while (Date.now() < deadline) {
        const st = await page.evaluate(() => window.poc.state());
        if (st.currentTime >= want + 1 || st.error) break;
        await sleep(250);
      }
    }
    windowStart = now();
    await page.evaluate(() => window.poc.events.push({ type: "window-start", t: Math.round(performance.now() - window.poc.clock0) }));
    await sleep(playSeconds * 1000);
  }
  windowEnd = now();
  await page.evaluate(() => window.poc.events.push({ type: "window-end", t: Math.round(performance.now() - window.poc.clock0) }));
  for (const t of timers) clearInterval(t);
  timers = [];
  await sleep(600); // let in-flight samples land
  extra.final = await page.evaluate(() => window.poc.state());
  extra.clock0 = await page.evaluate(() => window.poc.clock0);
  const events = await page.evaluate(() => window.poc.events);
  const pageNow = await page.evaluate(() => performance.now());
  const result = {
    label,
    mode,
    start,
    yadif,
    file: file ? basename(file) : null,
    options: { playSeconds, mid, growRate, growLag, growStep, growHead, liveBack },
    chromeVersion,
    idleStartMs: idleStart,
    window: { startMs: windowStart, endMs: windowEnd },
    pageNowAtEnd: pageNow,
    extra,
    samples,
    procs,
    live,
    media,
    events,
  };
  await writeFile(join(outDir, `${label}.json`), JSON.stringify(result));
  console.log(
    JSON.stringify({
      label,
      windowSec: (windowEnd - windowStart) / 1000,
      samples: samples.length,
      procSamples: procs.length,
      events: events.length,
      polls: extra.final?.fragments?.polls ?? null,
      grows: extra.final?.fragments?.grows ?? null,
      error: extra.final?.error ?? null,
      lastError: extra.final?.lastError ?? null,
      chromeVersion,
    }),
  );
} catch (error) {
  exitCode = 1;
  console.error("measure-run failed:", error);
} finally {
  for (const t of timers) clearInterval(t);
  await context.close().catch(() => {});
  await srv.close();
}
process.exitCode = exitCode;
