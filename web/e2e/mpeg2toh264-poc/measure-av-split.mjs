// Splits a measure-av-chrome.mjs JSON into the inputs measure-av-report.mjs reads (measurement only).
//   node measure-av-split.mjs <chrome.json> <outDir>
// Writes <outDir>/cont.json (continuous session, when mode=continuous) or
// <outDir>/seek-<P>.json per seek point (the session started by that seek).
import { readFile, writeFile, mkdir } from "node:fs/promises";
import { join } from "node:path";

const [inPath, outDir] = process.argv.slice(2);
const run = JSON.parse(await readFile(inPath, "utf8"));
await mkdir(outDir, { recursive: true });
const log = run.fragLog;
const slim = (frags) => frags.map((f) => ({ seq: f.seq, start: f.start, tracks: f.tracks.map((t) => ({ handler: t.handler, pres: t.pres })) }));
const modalSeq = (frags) => {
  const count = new Map();
  for (const f of frags) count.set(f.seq, (count.get(f.seq) ?? 0) + 1);
  return [...count.entries()].sort((a, b) => b[1] - a[1])[0]?.[0];
};
if (run.mode === "continuous") {
  await writeFile(join(outDir, "cont.json"), JSON.stringify({ origin: run.origin, fragments: slim(log) }));
  console.log(`cont: ${log.length} fragments, origin ${run.origin}`);
} else {
  for (let i = 0; i < run.seeks.length; i++) {
    const s = run.seeks[i];
    const to = run.seeks[i + 1]?.fragIndexFrom ?? log.length;
    const part = log.slice(s.fragIndexFrom, to);
    const seq = modalSeq(part);
    const mine = part.filter((f) => f.seq === seq);
    await writeFile(join(outDir, `seek-${s.point}.json`), JSON.stringify({ point: s.point, origin: run.origin, seq, ready: s.ready, fragments: slim(mine) }));
    console.log(`seek ${s.point}: ${mine.length} fragments, session ${seq}, ready ${s.ready}`);
  }
}
