// Audio seam / A-V offset analysis (measurement, not product code).
//
// Input: fragments in the shape measure-node.mjs and poc.js fragLog share:
//   { seq?, start, tracks: [{ id|trackId, handler, pres: [sec...] }] }
// and source PTS lists from ffprobe (see measure-src.sh):
//   { video: [ticks...], audio: [ticks...] }   (absolute 90 kHz PTS)
// origin: the originTicks the first Session reported (session second = (pts - origin) / 90000).
//
// Each output sample is matched to its nearest source PTS. Its residual is
//   res = outTime - (srcPts - origin) / 90000
// The A/V offset of a pair is resAudio - resVideo, which equals
//   (audio out - video out) - (audio src - video src) / 90000
// when the pair is matched by source PTS.
//
// Exports: samplesOf(fragments, handler) and matchResiduals(samples, srcTicks, origin).

const TICKS = 90000;

/** All output samples of one handler ("vide" or "soun") from a list of fragments, with their seq. */
export function samplesOf(fragments, handler) {
  const out = [];
  for (const f of fragments) {
    if (f.kind && f.kind !== "media") continue;
    for (const t of f.tracks ?? []) {
      if (t.handler !== handler) continue;
      for (const pres of t.pres) out.push({ t: pres, seq: f.seq ?? null, fragStart: f.start ?? null });
    }
  }
  return out;
}

/** Nearest source PTS (ticks) for each output sample; srcTicks must be sorted ascending. */
export function matchResiduals(samples, srcTicks, origin) {
  const res = [];
  let j = 0;
  // Video samples come in decode order; the two-pointer walk needs presentation order.
  for (const s of [...samples].sort((a, b) => a.t - b.t)) {
    const want = origin + s.t * TICKS;
    while (j + 1 < srcTicks.length && Math.abs(srcTicks[j + 1] - want) <= Math.abs(srcTicks[j] - want)) j++;
    const p = srcTicks[j];
    res.push({ ...s, src: p, res: s.t - (p - origin) / TICKS, srcErrTicks: Math.round(want - p) });
  }
  return res;
}

/** Gaps and overlaps between consecutive audio sample times (expected step = 1024 / rate). */
export function audioSeams(audio, rate = 48000) {
  const step = 1024 / rate;
  const bad = [];
  for (let i = 1; i < audio.length; i++) {
    const d = audio[i].t - audio[i - 1].t;
    if (Math.abs(d - step) > 1e-4) bad.push({ index: i, t: audio[i].t, delta: d });
  }
  return { checked: Math.max(0, audio.length - 1), anomalies: bad.length, first: bad.slice(0, 5) };
}
