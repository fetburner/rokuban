// Which video decoder each run ended on, from the CDP Media.* events that
// measure-run.mjs and control-decoder.mjs record.
//   node decoders.mjs <file.json>...   -> one Markdown table row per file
import { readFile } from "node:fs/promises";
import { basename } from "node:path";

console.log("| run | decoder | platform | fallback | frames (dropped / total) |");
console.log("|---|---|---|---|---|");
for (const path of process.argv.slice(2)) {
  const run = JSON.parse(await readFile(path, "utf8"));
  const props = new Map();
  const messages = [];
  for (const { ev, p } of run.media ?? []) {
    if (ev === "Media.playerPropertiesChanged") for (const { name, value } of p.properties) props.set(name, value);
    if (ev === "Media.playerMessagesLogged") for (const m of p.messages) messages.push(m.message);
  }
  const fallback = messages.some((m) => /decoder fallback/.test(m));
  const q = run.extra?.final?.playbackQuality ?? (run.info ? { droppedVideoFrames: run.info.dropped, totalVideoFrames: run.info.total } : null);
  const frames = q ? `${q.droppedVideoFrames} / ${q.totalVideoFrames}` : "-";
  console.log(`| ${run.label ?? basename(path)} | ${props.get("kVideoDecoderName") ?? "-"} | ${props.get("kIsPlatformVideoDecoder") ?? "-"} | ${fallback ? "yes" : "no"} | ${frames} |`);
}
