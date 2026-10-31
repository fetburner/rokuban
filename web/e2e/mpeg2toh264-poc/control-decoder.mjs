// Plain <video> control: which decoder does this Chrome pick for an ordinary H.264 file?
//   node control-decoder.mjs <dir> <file-in-dir> <outJson> [--play 8]
// Serves <dir> at /media/ and plays the file through the same CDP Media.* capture as measure-run.mjs.
import { writeFile, mkdir, rm } from "node:fs/promises";
import { join, resolve } from "node:path";
import { startServer } from "./serve.mjs";
const { chromium } = await import("playwright");
const [dir, name, outJson] = process.argv.slice(2).map((s, i) => (i < 2 ? s : resolve(s)));
const play = Number(process.argv.includes("--play") ? process.argv[process.argv.indexOf("--play") + 1] : 8);
const profile = join(resolve(dir), "..", "profile-control");
await rm(profile, { recursive: true, force: true });
await mkdir(profile, { recursive: true });
const srv = await startServer({ port: 0, media: resolve(dir) });
const context = await chromium.launchPersistentContext(profile, { channel: "chrome", headless: false, args: ["--autoplay-policy=no-user-gesture-required"] });
const media = [];
const page = context.pages()[0] ?? (await context.newPage());
const cdp = await context.newCDPSession(page);
await cdp.send("Media.enable");
for (const ev of ["Media.playersCreated", "Media.playerPropertiesChanged", "Media.playerEventsAdded", "Media.playerMessagesLogged", "Media.playerErrorsRaised"]) cdp.on(ev, (p) => media.push({ ev, p }));
await page.goto(`${srv.url}/media/control.html?f=${encodeURIComponent(name)}`);
await new Promise((r) => setTimeout(r, play * 1000));
const info = await page.evaluate(() => { const v = document.querySelector("video"); const q = v.getVideoPlaybackQuality(); return { currentTime: v.currentTime, readyState: v.readyState, w: v.videoWidth, h: v.videoHeight, total: q.totalVideoFrames, dropped: q.droppedVideoFrames }; });
await writeFile(outJson, JSON.stringify({ name, info, chromeVersion: context.browser()?.version(), media }));
console.log(JSON.stringify({ name, info, chromeVersion: context.browser()?.version() }));
await context.close();
await srv.close();
