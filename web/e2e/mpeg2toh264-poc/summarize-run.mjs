// Summarise run JSON files written by measure-run.mjs.
//   node summarize-run.mjs <runs-dir>  -> prints one JSON object per run to stdout
// Window stats use the window-start / window-end events the run itself appended to
// window.poc.events, so the page clock and the sample clock are not mixed.
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";

const dir = process.argv[2];
const pct = (xs, p) => {
  if (!xs.length) return null;
  const s = [...xs].sort((a, b) => a - b);
  return round(s[Math.min(s.length - 1, Math.ceil((p / 100) * s.length) - 1)]);
};
const mean = (xs) => (xs.length ? round(xs.reduce((a, b) => a + b, 0) / xs.length) : null);
const round = (n) => Math.round(n * 100) / 100;

function leadAt(state) {
  if (!state || !state.buffered) return null;
  for (const [s, e] of state.buffered) if (s - 0.05 <= state.currentTime && state.currentTime <= e) return e - state.currentTime;
  return 0;
}

function summarise(run) {
  const { label, samples, procs, events, media, window: w } = run;
  const ws = events.find((e) => e.type === "window-start");
  const we = events.find((e) => e.type === "window-end");
  const inWin = (e) => (ws && we ? e.t >= ws.t && e.t <= we.t : false);
  const out = { label, mode: run.mode, start: run.start, yadif: run.yadif, file: run.file, chromeVersion: run.chromeVersion };

  // --- CPU: ps samples whose node time is inside the window -------------------
  const wStart = w.startMs;
  const wEnd = w.endMs;
  const pw = procs.filter((p) => p.t >= wStart && p.t <= wEnd);
  const totals = pw.map((p) => p.rows.reduce((a, r) => a + r.cpu, 0));
  const rend = pw.map((p) => p.rows.filter((r) => r.role === "renderer").reduce((a, r) => a + r.cpu, 0));
  const gpu = pw.map((p) => p.rows.filter((r) => r.role === "gpu-process").reduce((a, r) => a + r.cpu, 0));
  const browser = pw.map((p) => p.rows.filter((r) => r.role === "browser").reduce((a, r) => a + r.cpu, 0));
  const util = pw.map((p) => p.rows.filter((r) => r.role === "utility").reduce((a, r) => a + r.cpu, 0));
  out.cpu = {
    samples: pw.length,
    totalMean: mean(totals),
    totalP95: pct(totals, 95),
    rendererMean: mean(rend),
    rendererP95: pct(rend, 95),
    gpuMean: mean(gpu),
    browserMean: mean(browser),
    utilityMean: mean(util),
    procRolesSeen: [...new Set(procs.flatMap((p) => p.rows.map((r) => r.role)))],
  };

  // --- frames: getVideoPlaybackQuality delta between the samples nearest the window ends
  const q = (s) => s?.playbackQuality ?? null;
  const near = (t) => samples.reduce((best, s) => (best == null || Math.abs(s.t - t) < Math.abs(best.t - t) ? s : best), null);
  const qs = q(near(wStart));
  const qe = q(near(wEnd));
  out.frames = qs && qe ? { dropped: qe.droppedVideoFrames - qs.droppedVideoFrames, total: qe.totalVideoFrames - qs.totalVideoFrames } : null;
  out.framesTotalSinceOpen = qe ? { dropped: qe.droppedVideoFrames, total: qe.totalVideoFrames } : null;

  // --- stalls -----------------------------------------------------------------
  const waits = events.filter((e) => e.type === "waiting" && inWin(e));
  const stalledEv = events.filter((e) => e.type === "stalled" && inWin(e));
  const playings = events.filter((e) => e.type === "playing");
  const stallDur = waits.map((wv) => {
    const next = playings.find((p) => p.t > wv.t);
    return next ? { at: round(wv.currentTime), ms: next.t - wv.t, resumedAt: round(next.currentTime) } : { at: round(wv.currentTime), ms: null, resumedAt: null };
  });
  out.stalls = {
    waiting: waits.length,
    stalledEvents: stalledEv.length,
    unresolved: stallDur.filter((s) => s.ms == null).length,
    totalMs: stallDur.reduce((a, s) => a + (s.ms ?? 0), 0),
    maxMs: stallDur.reduce((a, s) => Math.max(a, s.ms ?? 0), 0),
    list: stallDur.slice(0, 40),
  };

  // --- buffer lead inside the window --------------------------------------------
  const leads = samples.filter((s) => s.t >= wStart && s.t <= wEnd).map((s) => leadAt(s)).filter((x) => x != null);
  out.minLeadSec = leads.length ? round(Math.min(...leads)) : null;
  const leadsAfter10 = samples.filter((s) => s.t >= wStart + 10000 && s.t <= wEnd).map((s) => leadAt(s)).filter((x) => x != null);
  out.minLeadAfter10sSec = leadsAfter10.length ? round(Math.min(...leadsAfter10)) : null;
  out.leadBelow1sFraction = leads.length ? round(leads.filter((x) => x < 1).length / leads.length) : null;

  // --- transcode speed: buffered-end growth before backpressure -------------------
  // Start at the first sample after the (first or seek) Session; stop at the first sample whose lead >= 19 s.
  const seekIdx = samples.findIndex((s) => s.fragments && s.fragments.sessions >= 2);
  const target = run.extra?.seekRecord?.target ?? 0;
  const from = Math.max(0, seekIdx);
  // first sample where the range that starts at the target exists (post-seek buffer)
  const i0 = samples.findIndex((s, i) => i >= from && s.buffered?.length && Math.abs(s.buffered[0][0] - target) <= 5);
  let i1 = -1;
  for (let i = i0 + 1; i < samples.length && i0 >= 0; i++) {
    const l = leadAt(samples[i]);
    if ((l != null && l >= 19) || samples[i].t - samples[i0].t >= 60000) {
      i1 = i;
      break;
    }
  }
  if (i0 >= 0 && i1 > i0) {
    const endOf = (s) => s.buffered[0][1];
    const a = samples[i0];
    const b = samples[i1];
    const dMedia = endOf(b) - endOf(a);
    const dWall = (b.t - a.t) / 1000;
    out.transcode = { mediaSecPerWallSec: dWall > 0 ? round(dMedia / dWall) : null, wallSec: round(dWall), mediaSec: round(dMedia), note: "buffered end growth from the post-seek range start until lead reaches 19 s or 60 s wall, whichever first" };
  } else {
    out.transcode = null;
  }

  // --- decoder evidence (CDP Media.playerPropertiesChanged) ------------------------
  const props = new Map();
  for (const m of media) {
    if (m.ev !== "Media.playerPropertiesChanged") continue;
    for (const { name, value } of m.p.properties) {
      if (/Decoder|Platform|kResolution|kVideoTracks|kAudioTracks/.test(name)) props.set(name, value);
    }
  }
  out.decoder = Object.fromEntries([...props].filter(([k]) => /Decoder|Platform/.test(k)));
  const msgs = media.filter((m) => m.ev === "Media.playerMessagesLogged").flatMap((m) => m.p.messages.map((x) => x.message));
  out.decoderMessages = msgs.filter((x) => /Decoder|decoder/.test(x)).slice(0, 8);

  // --- errors -------------------------------------------------------------------------
  const fin = run.extra?.final;
  out.errors = { error: fin?.error ?? null, lastError: fin?.lastError ?? null, pollErrors: fin?.fragments?.pollErrors ?? null, ended: events.some((e) => e.type === "ended") };
  out.final = fin ? { currentTime: fin.currentTime, polls: fin.fragments?.polls, grows: fin.fragments?.grows, media: fin.fragments?.media, dropped: fin.sessionCounters?.dropped } : null;

  // --- live-edge distance (grow mode) ----------------------------------------------------
  if (run.live?.length) {
    const d = [];
    for (const lv of run.live) {
      if (lv.seconds == null) continue;
      const s = near(lv.t);
      if (s?.currentTime != null) d.push({ t: lv.t, behind: round(lv.seconds - s.currentTime), liveSec: lv.seconds, size: lv.size });
    }
    out.live = {
      samples: d.length,
      maxBehindLiveSec: d.length ? round(Math.max(...d.map((x) => x.behind))) : null,
      minBehindLiveSec: d.length ? round(Math.min(...d.map((x) => x.behind))) : null,
      visibleSizeFirst: run.live[0]?.size ?? null,
      visibleSizeLast: run.live.at(-1)?.size ?? null,
    };
  }
  out.seekRecordAchieved = run.extra?.seekRecord?.achieved ?? null;
  out.open = run.extra?.open ?? null;
  return out;
}

const files = (await readdir(dir)).filter((f) => f.endsWith(".json") && !f.startsWith("profile"));
for (const f of files.sort()) {
  const run = JSON.parse(await readFile(join(dir, f), "utf8"));
  if (!run.procs) continue;
  console.log(JSON.stringify(summarise(run)));
}
