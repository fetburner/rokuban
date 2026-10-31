// Node-side continuous (a) and seek (b) sessions for the audio-seam check (measurement only).
//   node measure-av-node.mjs <file.m2ts> <outDir> <label> <untilSec> <p1,p2,...>
// Writes <outDir>/<label>-cont.json and <label>-seek-<P>.json (fragments only), and prints the origin.
import { mkdir, writeFile } from "node:fs/promises";
import { basename, join, resolve } from "node:path";
import { loadLib, runSession, byteForPts } from "./measure-node.mjs";
import { stat } from "node:fs/promises";

const [file, outDir, label, untilArg, pointsArg] = process.argv.slice(2);
const until = Number(untilArg);
const points = pointsArg.split(",").map(Number);
await mkdir(outDir, { recursive: true });
const lib = await loadLib();
const size = (await stat(resolve(file))).size;
const slim = (frags) => frags.map((f) => ({ kind: f.kind, start: f.start, tracks: f.tracks?.map((t) => ({ handler: t.handler, pres: t.pres })) }));
const began = Date.now();
const cont = await runSession(lib, { file: resolve(file), untilSec: until, size });
const origin = cont.originTicks;
await writeFile(join(outDir, `${label}-cont.json`), JSON.stringify({ origin, fragments: slim(cont.fragments) }));
console.log(JSON.stringify({ label, origin, contSec: (Date.now() - began) / 1000, fragments: cont.fragments.length }));
for (const p of points) {
  const t = Date.now();
  const { byte, rel } = await byteForPts(lib, resolve(file), size, origin, p);
  const seek = await runSession(lib, { file: resolve(file), startByte: byte, origin, untilSec: p + 12, size });
  await writeFile(join(outDir, `${label}-seek-${p}.json`), JSON.stringify({ point: p, byte, seekRel: rel, origin: seek.originTicks, fragments: slim(seek.fragments) }));
  console.log(JSON.stringify({ label, point: p, byte, seekRel: rel, sec: (Date.now() - t) / 1000, seekOrigin: seek.originTicks }));
}
