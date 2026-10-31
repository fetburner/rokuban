// Headed-Chrome driver for the PoC.
//
//   node driver.mjs <file.ts> <outDir> [seek,seek,...] [--play 15] [--settle 4]
//                   [--audio-pid N] [--yadif]
//
// Starts serve.mjs (mounting the file's directory at /media/), opens the page
// in real Chrome (channel 'chrome', headed), plays for --play seconds, then seeks
// to each listed second and captures frames. Prints one JSON object per step and
// writes <outDir>/run.json with everything.
import { mkdir, writeFile } from "node:fs/promises";
import { basename, dirname, join, resolve } from "node:path";
import { startServer } from "./serve.mjs";

const { chromium } = await import("playwright");

const argv = process.argv.slice(2);
const flag = (name, fallback) => {
  const i = argv.indexOf(name);
  return i >= 0 ? argv[i + 1] : fallback;
};
const positional = [];
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    if (argv[i] !== "--yadif") i++;
    continue;
  }
  positional.push(argv[i]);
}
if (positional.length < 2) {
  console.error("usage: node driver.mjs <file.ts> <outDir> [seek,seek,...] [--play N] [--settle N] [--audio-pid N] [--yadif]");
  process.exit(2);
}
const file = resolve(positional[0]);
const outDir = resolve(positional[1]);
const seeks = (positional[2] && positional[2] !== "-" ? positional[2].split(",") : []).map(Number);
const playSeconds = Number(flag("--play", 15));
const settleSeconds = Number(flag("--settle", 4));
const audioPid = flag("--audio-pid", null);
const yadif = argv.includes("--yadif");

await mkdir(outDir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const report = { file, startedAt: new Date().toISOString(), options: { playSeconds, seeks, settleSeconds, audioPid, yadif }, steps: [] };
const say = (step, data) => {
  report.steps.push({ step, ...data });
  console.log(JSON.stringify({ step, ...data }));
};

async function save(capture, name) {
  const png = Buffer.from(capture.dataUrl.split(",")[1], "base64");
  await writeFile(join(outDir, name), png);
  const { dataUrl, ...rest } = capture;
  return { file: join(outDir, name), ...rest };
}

const srv = await startServer({ port: 0, media: dirname(file) });
const browser = await chromium.launch({
  channel: "chrome",
  headless: false,
  args: ["--autoplay-policy=no-user-gesture-required"],
});
let exitCode = 0;
try {
  const page = await browser.newPage();
  page.on("pageerror", (e) => say("pageerror", { message: e.message }));
  page.on("console", (m) => {
    if (m.type() === "error" || m.type() === "warning") say("console", { type: m.type(), text: m.text() });
  });
  await page.goto(`${srv.url}/index.html`);
  await page.waitForFunction(() => Boolean(window.poc), null, { timeout: 30_000 });
  await page.evaluate(() => window.poc.ready);

  const url = `${srv.url}/media/${encodeURIComponent(basename(file))}`;
  const began = Date.now();
  const opened = await page.evaluate(([u, o]) => window.poc.open(u, o), [url, { audioPid: audioPid === null ? undefined : Number(audioPid), yadif }]);
  say("open", { ...opened, wallMs: Date.now() - began });

  await sleep(playSeconds * 1000);
  say("state-after-play", await page.evaluate(() => window.poc.state()));
  say("capture-play", await save(await page.evaluate(() => window.poc.capture()), "play.png"));

  for (const sec of seeks) {
    const seekBegan = Date.now();
    const record = await page.evaluate((s) => window.poc.seek(s), sec);
    say("seek-returned", { target: sec, record, wallMs: Date.now() - seekBegan });
    // Wait until the playhead is past the target, or give up after 45 s.
    const deadline = Date.now() + 45_000;
    let st;
    while (Date.now() < deadline) {
      st = await page.evaluate(() => window.poc.state());
      if (st.currentTime >= sec + settleSeconds || st.lastError || st.error) break;
      await sleep(500);
    }
    say("state-after-seek", { target: sec, ...(await page.evaluate(() => window.poc.state())) });
    say("capture-seek", await save(await page.evaluate(() => window.poc.capture()), `seek-${sec}.png`));
  }

  report.events = await page.evaluate(() => window.poc.events);
  report.finishedAt = new Date().toISOString();
  await writeFile(join(outDir, "run.json"), JSON.stringify(report, null, 2));
  console.log(`events: ${report.events.length}; wrote ${join(outDir, "run.json")}`);
} catch (error) {
  exitCode = 1;
  console.error("driver failed:", error);
} finally {
  await browser.close();
  await srv.close();
}
process.exitCode = exitCode;
