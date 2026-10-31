// Chapter-boundary seek check in one headed Chrome (measurement, not product code).
//
//   node measure-boundaries.mjs --file <m2ts> --F <first video PTS ticks> --boundaries 2302,276243,...
//       --out <dir> --label <name> [--audio-pid 273] [--settle 0.05]
//
// For each chapter boundary b (ms, frame 0 = first video frame F), target time in the
// Session timeline is  t = b/1000 + (F - originTicks)/90000, where originTicks is what the
// first Session reported. Two seeks per boundary:
//   exact   t
//   center  t + 0.5 / 29.97   (middle of the frame that contains t)
// After each seek the page waits until the playhead is at the target and buffered, then
// plays for one presented frame (requestVideoFrameCallback gives mediaTime), captures
// 320x180, and pauses. Writes <out>/<label>.json and PNGs next to it.
import { mkdir, writeFile } from "node:fs/promises";
import { basename, dirname, join, resolve } from "node:path";
import { startServer } from "./serve.mjs";

const { chromium } = await import("playwright");

const argv = process.argv.slice(2);
const opt = (name, fallback) => {
  const i = argv.indexOf(name);
  return i >= 0 ? argv[i + 1] : fallback;
};
const file = resolve(opt("--file"));
const F = Number(opt("--F"));
const boundaries = opt("--boundaries", "").split(",").filter(Boolean).map(Number);
const outDir = resolve(opt("--out"));
const label = opt("--label", "run");
const audioPid = opt("--audio-pid", null);
const FRAME = 1001 / 30000;

await mkdir(outDir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const srv = await startServer({ port: 0, media: dirname(file) });
const profile = join(outDir, `profile-${label}`);
const context = await chromium.launchPersistentContext(profile, {
  channel: "chrome",
  headless: false,
  args: ["--autoplay-policy=no-user-gesture-required"],
});
const result = { file: basename(file), F, label, chromeVersion: context.browser()?.version(), originTicks: null, seeks: [], events: [], errors: [] };
let exitCode = 0;
try {
  const page = context.pages()[0] ?? (await context.newPage());
  page.on("pageerror", (e) => result.errors.push({ where: "pageerror", message: e.message }));
  await page.goto(`${srv.url}/index.html`);
  await page.waitForFunction(() => Boolean(window.poc), null, { timeout: 30_000 });
  await page.evaluate(() => window.poc.ready);

  const url = `${srv.url}/media/${encodeURIComponent(basename(file))}`;
  const opened = await page.evaluate(([u, o]) => window.poc.open(u, o), [url, { audioPid: audioPid === null ? undefined : Number(audioPid), muted: true }]);
  result.open = opened;
  result.originTicks = opened.originTicks;
  const origin = opened.originTicks;
  if (origin == null) throw new Error("no originTicks reported");
  await page.evaluate(() => document.getElementById("v").pause());

  const targets = [];
  for (const b of boundaries) {
    const exact = b / 1000 + (F - origin) / 90000;
    targets.push({ boundary: b, variant: "exact", target: exact });
    targets.push({ boundary: b, variant: "center", target: exact + FRAME / 2 });
  }

  for (const t of targets) {
    const began = Date.now();
    const rec = await page.evaluate((s) => window.poc.seek(s), t.target);
    // Stage 1: wait until the target is buffered. The first seek can land early: Chrome
    // places the playhead at the start of the new Session's first buffered GOP, which is
    // up to a second before the target. Record where it landed.
    const stage1 = await page.evaluate(async (target) => {
      const v = document.getElementById("v");
      const start = performance.now();
      const covered = () =>
        Array.from({ length: v.buffered.length }, (_, i) => [v.buffered.start(i), v.buffered.end(i)]).some(
          ([s, e]) => s <= target && e >= target + 0.3,
        );
      while (performance.now() - start < 60_000 && !covered()) await new Promise((r) => setTimeout(r, 50));
      return { coveredMs: Math.round(performance.now() - start), landed: v.currentTime, covered: covered() };
    }, t.target);
    // Stage 2: re-seek to the exact target now that it is buffered, and wait for it.
    const waited = await page.evaluate(async (target) => {
      const v = document.getElementById("v");
      v.currentTime = target;
      const start = performance.now();
      while (performance.now() - start < 30_000) {
        if (!v.seeking && v.readyState >= 2 && Math.abs(v.currentTime - target) < 0.05) return Math.round(performance.now() - start);
        await new Promise((r) => setTimeout(r, 50));
      }
      return null;
    }, t.target);
    const beforePlay = await page.evaluate(() => {
      const v = document.getElementById("v");
      return { currentTime: v.currentTime, readyState: v.readyState, paused: v.paused };
    });
    // Capture while paused: the frame shown at currentTime. (Playing first advanced the
    // picture 5-9 frames before the grab, measured with mediaTime in the first run.)
    await page.evaluate(() => document.getElementById("v").pause());
    await sleep(400);
    const cap = await page.evaluate(() => window.poc.capture(320, 180));
    const png = Buffer.from(cap.dataUrl.split(",")[1], "base64");
    const pngName = `seek-b${t.boundary}-${t.variant}.png`;
    await writeFile(join(outDir, pngName), png);
    result.seeks.push({
      boundary: t.boundary,
      variant: t.variant,
      target: t.target,
      byte: rec?.byte ?? null,
      seekAchieved: rec?.achieved ?? null,
      coveredMs: stage1.coveredMs,
      landedBeforeReseek: stage1.landed,
      waitedMs: waited,
      wallMs: Date.now() - began,
      currentTimeBeforePlay: beforePlay.currentTime,
      readyState: beforePlay.readyState,
      mediaTime: cap.mediaTime,
      presentedFrames: cap.presentedFrames,
      videoWidth: cap.videoWidth,
      videoHeight: cap.videoHeight,
      png: pngName,
    });
    await sleep(150);
  }
  result.state = await page.evaluate(() => window.poc.state());
  result.events = (await page.evaluate(() => window.poc.events)).filter((e) => !["timeupdate"].includes(e.type));
  await writeFile(join(outDir, `${label}.json`), JSON.stringify(result, null, 1));
  console.log(JSON.stringify({ label, originTicks: origin, seeks: result.seeks.length, errors: result.errors.length, chrome: result.chromeVersion }));
} catch (error) {
  exitCode = 1;
  result.fatal = String(error?.stack ?? error);
  await writeFile(join(outDir, `${label}.json`), JSON.stringify(result, null, 1)).catch(() => {});
  console.error("measure-boundaries failed:", error);
} finally {
  await context.close().catch(() => {});
  await srv.close();
}
process.exitCode = exitCode;
