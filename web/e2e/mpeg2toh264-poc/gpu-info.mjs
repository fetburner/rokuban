// What this Chrome says about its GPU and video decode acceleration.
//   node gpu-info.mjs <outJson>
// Reads chrome://gpu in headed Chrome (channel chrome) and keeps the two
// sections that decide whether a hardware video decoder can exist at all.
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const { chromium } = await import("playwright");
const outJson = resolve(process.argv[2] ?? "gpu-info.json");
const profile = await mkdtemp(join(tmpdir(), "gpu-info-"));
const context = await chromium.launchPersistentContext(profile, { channel: "chrome", headless: false });
try {
  const page = context.pages()[0] ?? (await context.newPage());
  await page.goto("chrome://gpu");
  await page.waitForTimeout(3000);
  // chrome://gpu renders into a shadow root; innerText of the host flattens it.
  const text = await page.evaluate(() => {
    const host = document.querySelector("info-view");
    return (host?.shadowRoot ?? document).querySelector("*")?.parentNode?.textContent ?? document.body.innerText;
  });
  const section = (title) => {
    const at = text.indexOf(title);
    return at < 0 ? null : text.slice(at, at + 2500);
  };
  const result = {
    chromeVersion: context.browser()?.version(),
    featureStatus: section("Graphics Feature Status"),
    videoAcceleration: section("Video Acceleration Information"),
    gpu0: section("GPU0"),
  };
  await writeFile(outJson, JSON.stringify(result, null, 2));
  console.log(result.featureStatus?.slice(0, 1200) ?? "(no feature status)");
  console.log(result.videoAcceleration?.slice(0, 1200) ?? "(no video acceleration section)");
} finally {
  await context.close();
  await rm(profile, { recursive: true, force: true });
}
