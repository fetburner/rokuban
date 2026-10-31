// Chrome run for the audio-seam check (measurement, not product code).
//
//   node measure-av-chrome.mjs --file <m2ts> --out <dir> --label <name>
//       --mode continuous --play <sec>                     play from the start, no seek
//   node measure-av-chrome.mjs --file <m2ts> --out <dir> --label <name>
//       --mode seeks --points 15,60,120 --watch 12         open, then seek to each point (session seconds)
//
// Writes <out>/<label>.json with poc.fragLog (per fragment: session seq, start, per-track
// presentation times in seconds), state samples every 500 ms and the event log.
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
const outDir = resolve(opt("--out"));
const label = opt("--label", "run");
const mode = opt("--mode", "continuous");
const play = Number(opt("--play", "60"));
const points = opt("--points", "").split(",").filter(Boolean).map(Number);
const watch = Number(opt("--watch", "12"));
const audioPid = opt("--audio-pid", null);

await mkdir(outDir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const srv = await startServer({ port: 0, media: dirname(file) });
const context = await chromium.launchPersistentContext(join(outDir, `profile-${label}`), {
  channel: "chrome",
  headless: false,
  args: ["--autoplay-policy=no-user-gesture-required"],
});
const out = { file: basename(file), label, mode, points, watch, play, samples: [], seeks: [], errors: [] };
let exitCode = 0;
try {
  const page = context.pages()[0] ?? (await context.newPage());
  page.on("pageerror", (e) => out.errors.push(String(e.message)));
  await page.goto(`${srv.url}/index.html`);
  await page.waitForFunction(() => Boolean(window.poc), null, { timeout: 30_000 });
  await page.evaluate(() => window.poc.ready);
  const url = `${srv.url}/media/${encodeURIComponent(basename(file))}`;
  const opened = await page.evaluate(([u, o]) => window.poc.open(u, o), [url, { audioPid: audioPid === null ? undefined : Number(audioPid), muted: true }]);
  out.open = opened;
  const sampler = setInterval(async () => {
    try {
      const st = await page.evaluate(() => window.poc.state());
      out.samples.push({ wall: Date.now(), currentTime: st.currentTime, readyState: st.readyState, error: st.error, seeking: st.seeking, w: st.videoWidth, h: st.videoHeight, dropped: st.playbackQuality?.droppedVideoFrames ?? null, total: st.playbackQuality?.totalVideoFrames ?? null, buffered: st.buffered });
    } catch {
      // page busy; skip this sample
    }
  }, 500);

  if (mode === "continuous") {
    await page.evaluate(() => window.poc.play());
    await sleep(play * 1000);
  } else {
    for (const p of points) {
      await page.evaluate(() => document.getElementById("v").pause());
      const before = await page.evaluate(() => window.poc.fragLog.length);
      const began = Date.now();
      const rec = await page.evaluate((s) => window.poc.seek(s), p);
      // Wait for the playhead to reach the point, then play for `watch` seconds of media.
      const ready = await page.evaluate(async (target) => {
        const v = document.getElementById("v");
        const t0 = performance.now();
        while (performance.now() - t0 < 90_000) {
          if (!v.seeking && v.readyState >= 3 && Math.abs(v.currentTime - target) < 0.1) return true;
          await new Promise((r) => setTimeout(r, 100));
        }
        return false;
      }, p);
      await page.evaluate(() => window.poc.play());
      const endAt = p + watch;
      await page.evaluate(async (end) => {
        const v = document.getElementById("v");
        const t0 = performance.now();
        while (performance.now() - t0 < 120_000 && v.currentTime < end) await new Promise((r) => setTimeout(r, 200));
      }, endAt);
      await page.evaluate(() => document.getElementById("v").pause());
      out.seeks.push({ point: p, ready, byte: rec?.byte ?? null, achieved: rec?.achieved ?? null, fragIndexFrom: before, wallMs: Date.now() - began });
    }
  }
  clearInterval(sampler);
  out.events = (await page.evaluate(() => window.poc.events)).filter((e) => e.type !== "timeupdate");
  out.origin = (await page.evaluate(() => window.poc.state())).originTicks;
  out.fragLog = await page.evaluate(() => window.poc.fragLog);
  out.chromeVersion = context.browser()?.version();
  await writeFile(join(outDir, `${label}.json`), JSON.stringify(out));
  console.log(JSON.stringify({ label, fragments: out.fragLog.length, seeks: out.seeks.length, origin: out.origin, errors: out.errors.length }));
} catch (error) {
  exitCode = 1;
  out.fatal = String(error?.stack ?? error);
  await writeFile(join(outDir, `${label}.json`), JSON.stringify(out)).catch(() => {});
  console.error("measure-av-chrome failed:", error);
} finally {
  await context.close().catch(() => {});
  await srv.close();
}
process.exitCode = exitCode;
