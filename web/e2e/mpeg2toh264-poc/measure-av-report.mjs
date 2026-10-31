// Report for the audio-seam check (measurement, not product code).
//
//   node measure-av-report.mjs --src <src.json> --cont <cont.json> --seek <seek1.json>,<seek2.json>,...
//
//   src.json   { origin, video: [ticks], audio: [ticks] }   ffprobe packet PTS, sorted
//   cont.json  { fragments: [...] }   continuous session from the start of the file
//   seek.json  { point: P, fragments: [...] }   a new Session started near P (session seconds)
//
// Fragments: { start, tracks: [{ handler, pres: [sec] }] } (poc.fragLog or measure-node output).
// For each point P (session seconds) it prints:
//   sameSamples      audio samples in [P, P+10] present in both sessions (matched by source PTS)
//   maxAudioShift    max |out_seek - out_cont| over those samples (seconds)
//   avOffsetAt0      mean(resAudio) - mean(resVideo) over the seek session's first 1 s of output
//   avOffsetAt10     same over [P+10, P+11]
//   contAvOffsetAt0  the same for the continuous session over [P, P+1]
//   seams            consecutive-sample anomalies in the seek session's audio
import { readFile } from "node:fs/promises";
import { matchResiduals, samplesOf, audioSeams } from "./measure-av.mjs";

const argv = process.argv.slice(2);
const opt = (name) => {
  const i = argv.indexOf(name);
  return i >= 0 ? argv[i + 1] : null;
};
const src = JSON.parse(await readFile(opt("--src"), "utf8"));
const cont = JSON.parse(await readFile(opt("--cont"), "utf8"));
const seeks = await Promise.all(
  opt("--seek")
    .split(",")
    .map(async (p) => JSON.parse(await readFile(p, "utf8"))),
);
const origin = src.origin;
const r = (x) => (x == null ? null : Math.round(x * 1e6) / 1e6);
const mean = (xs) => (xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null);

const contA = matchResiduals(samplesOf(cont.fragments, "soun"), src.audio, origin);
const contV = matchResiduals(samplesOf(cont.fragments, "vide"), src.video, origin);

function windowMean(list, from, to) {
  return mean(list.filter((s) => s.t >= from && s.t < to).map((s) => s.res));
}

const rows = [];
for (const s of seeks) {
  const P = s.point;
  const seekA = matchResiduals(samplesOf(s.fragments, "soun"), src.audio, origin);
  const seekV = matchResiduals(samplesOf(s.fragments, "vide"), src.video, origin);
  const t0 = seekA.length ? Math.min(...seekA.map((x) => x.t)) : null;
  // Same source audio frames in both sessions: compare output times of identical src PTS.
  const contBySrc = new Map(contA.map((x) => [x.src, x.t]));
  let same = 0;
  let maxShift = 0;
  for (const x of seekA) {
    if (x.t < P || x.t >= P + 10) continue;
    const c = contBySrc.get(x.src);
    if (c == null) continue;
    same++;
    maxShift = Math.max(maxShift, Math.abs(c - x.t));
  }
  const offAt = (aList, vList, from, to) => {
    const a = windowMean(aList, from, to);
    const v = windowMean(vList, from, to);
    return a == null || v == null ? null : r(a - v);
  };
  rows.push({
    point: P,
    seekFirstAudioOut: r(t0),
    seekFirstVideoOut: seekV.length ? r(Math.min(...seekV.map((x) => x.t))) : null,
    sameSamples: same,
    maxAudioShift: r(maxShift),
    avOffsetAt0: t0 == null ? null : offAt(seekA, seekV, t0, t0 + 1),
    avOffsetAt10: offAt(seekA, seekV, P + 10, P + 11),
    contAvOffsetAt0: offAt(contA, contV, P, P + 1),
    seams: audioSeams(seekA.map((x) => ({ t: x.t }))),
    srcErrTicksMax: Math.max(0, ...seekA.map((x) => Math.abs(x.srcErrTicks))),
  });
}
console.log(JSON.stringify(rows, null, 1));
