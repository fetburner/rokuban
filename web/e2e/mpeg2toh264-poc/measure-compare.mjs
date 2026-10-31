// Frame identification for chapter-boundary captures (measurement, not product code).
//
//   node measure-compare.mjs <run.json> <captureDir> <refsDir> <recLabel>
//
// Reference frames: refs/ref-<rec>-<b>ms-<pts>.png = ffmpeg frame at PTS <pts>, 320x180,
// frames n-2..n+2 around the boundary (n = the middle one). Each capture is compared with
// the five refs by luma MAE (Rec.709 luma of RGB, the same weights poc.capture uses), and by
// Pearson correlation of the luma planes (insensitive to a global gain/offset).
// Calibration: MAE between refs n and n+-1 / n+-2 shows how far apart neighbouring frames are.
import { readFile, readdir } from "node:fs/promises";
import { execFile } from "node:child_process";
import { join, resolve } from "node:path";

const [runPath, capDir, refsDir, rec = "292"] = process.argv.slice(2).map((s, i) => (i < 3 ? resolve(s) : s));

const W = 320;
const H = 180;

function rgb(path) {
  return new Promise((ok, fail) =>
    execFile(
      "ffmpeg",
      ["-v", "error", "-i", path, "-vf", `scale=${W}:${H}:flags=bicubic`, "-f", "rawvideo", "-pix_fmt", "rgb24", "-"],
      { encoding: "buffer", maxBuffer: 1 << 26 },
      (err, out) => (err ? fail(err) : ok(out)),
    ),
  );
}
function luma(buf) {
  const y = new Float64Array(W * H);
  for (let i = 0, p = 0; i < y.length; i++, p += 3) y[i] = 0.2126 * buf[p] + 0.7152 * buf[p + 1] + 0.0722 * buf[p + 2];
  return y;
}
function mae(a, b) {
  let s = 0;
  for (let i = 0; i < a.length; i++) s += Math.abs(a[i] - b[i]);
  return s / a.length;
}
function ncc(a, b) {
  const n = a.length;
  let ma = 0, mb = 0;
  for (let i = 0; i < n; i++) (ma += a[i]), (mb += b[i]);
  ma /= n;
  mb /= n;
  let num = 0, da = 0, db = 0;
  for (let i = 0; i < n; i++) {
    const x = a[i] - ma, y = b[i] - mb;
    num += x * y;
    da += x * x;
    db += y * y;
  }
  return da && db ? num / Math.sqrt(da * db) : null;
}
const r2 = (x) => (x == null ? null : Math.round(x * 1000) / 1000);

const run = JSON.parse(await readFile(runPath, "utf8"));
const refFiles = (await readdir(refsDir)).filter((f) => f.startsWith(`ref-${rec}-`) && f.endsWith(".png"));
const byBoundary = new Map();
for (const f of refFiles) {
  const m = /^ref-\d+-(\d+)ms-(\d+)\.png$/.exec(f);
  if (!m) continue;
  const b = Number(m[1]);
  if (!byBoundary.has(b)) byBoundary.set(b, []);
  byBoundary.get(b).push({ pts: Number(m[2]), path: join(refsDir, f) });
}
for (const list of byBoundary.values()) list.sort((a, b) => a.pts - b.pts);

const refLuma = new Map();
const getRef = async (path) => {
  if (!refLuma.has(path)) refLuma.set(path, luma(await rgb(path)));
  return refLuma.get(path);
};

const rows = [];
const calibration = [];
for (const [b, list] of [...byBoundary.entries()].sort((x, y) => x[0] - y[0])) {
  if (list.length < 5) {
    calibration.push({ boundary: b, note: `only ${list.length} refs` });
    continue;
  }
  const ys = await Promise.all(list.map((r) => getRef(r.path)));
  const spacing = {};
  for (const k of [-2, -1, 1, 2]) spacing[`n${k >= 0 ? "+" : ""}${k}`] = r2(mae(ys[2], ys[2 + k]));
  calibration.push({ boundary: b, refMAE_vs_n: spacing, refPts: list.map((r) => r.pts) });
}

for (const s of run.seeks) {
  const bl = byBoundary.get(s.boundary);
  if (!bl || bl.length < 5) continue;
  const cap = luma(await rgb(join(capDir, s.png)));
  const ys = await Promise.all(bl.map((r) => getRef(r.path)));
  const maes = ys.map((y) => mae(cap, y));
  const nccs = ys.map((y) => ncc(cap, y));
  let best = 0;
  for (let i = 1; i < 5; i++) if (maes[i] < maes[best]) best = i;
  const offset = best - 2;
  const others = maes.filter((_, i) => i !== best);
  const margin = Math.min(...others) - maes[best];
  const expectedPts = bl[2].pts;
  rows.push({
    boundary: s.boundary,
    variant: s.variant,
    target: r2(s.target),
    currentTimeBeforePlay: r2(s.currentTimeBeforePlay),
    mediaTime: s.mediaTime == null ? null : r2(s.mediaTime),
    mediaTimeFrameOffset: s.mediaTime == null ? null : r2((s.mediaTime * 90000 + run.originTicks - expectedPts) / 3003),
    bestOffset: offset,
    maeByOffset: maes.map(r2),
    nccByOffset: nccs.map((x) => r2(x)),
    marginMAE: r2(margin),
    pass: Math.abs(offset) <= 1,
    png: s.png,
  });
}
const pass = rows.filter((r) => r.pass).length;
console.log(JSON.stringify({ rec, refsPerBoundary: 5, origin: run.originTicks, F: run.F, passCount: `${pass}/${rows.length}`, rows, calibration }, null, 1));
