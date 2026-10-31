// PoC page logic: transport stream -> mpeg2toh264 Session (WASM) -> MSE.
// Plain ES module. Driven from outside through window.poc (see README-less API
// comments below). Nothing here is product code.
//
// window.poc API
//   ready                       Promise<true> once the WASM module is up
//   open(url, { audioPid?, muted?, yadif? })
//                               resolves once the first media fragment is appended
//   play()                      video.play()
//   seek(sec)                   new Session anchored at the stored origin; resolves
//                               with { target, byte, probes, achieved, lookupMs }
//   state()                     plain object snapshot (see below)
//   capture(w=320, h=180)       PNG dataURL of the presented frame + mediaTime + meanLuma
//   events                      timestamped log array
//
// Timeline: fragment.start and the fMP4 timing are presentation seconds measured
// from the origin the FIRST Session reports (originTicks, 90 kHz PES units). A
// seek builds a Session anchored to that same origin, so MSE time == presentation
// time across seeks and nothing is ever appended with a timestampOffset.
import init, { Session, firstTimestamp, lastTimestamp } from "./dist/mpeg2toh264_wasm.js";
// Measurement only: per-fragment track timing (see measure-fmp4.mjs).
import { parseInit, parseFragment, presentation } from "./measure-fmp4.mjs";

const TICKS_PER_SECOND = 90000;
const PTS_MODULUS = 2 ** 33;
const CHUNK_BYTES = 3 * 1024 * 1024; // the task asked for 2-4 MB Range reads
const PROBE_BYTES = 512 * 1024;
const MAX_AHEAD_SECONDS = 20; // stop converting this far past the playhead
const KEEP_BEHIND_SECONDS = 10; // evict media this far behind the playhead
const SEEK_TOLERANCE_SECONDS = 2.5;
const FIRST_MEDIA_TIMEOUT_MS = 60_000;

const video = document.getElementById("v");
const canvas = document.createElement("canvas");
const ctx = canvas.getContext("2d", { willReadFrequently: true });

const events = [];
const t0 = performance.now();
function log(type, detail = {}) {
  if (events.length < 1000) events.push({ t: Math.round(performance.now() - t0), type, ...detail });
}
for (const name of ["waiting", "stalled", "error", "ended", "seeking", "seeked", "playing", "pause"]) {
  video.addEventListener(name, () =>
    log(name, {
      currentTime: round(video.currentTime),
      ...(video.error ? { code: video.error.code, message: video.error.message } : {}),
    }),
  );
}

const wasmReady = init().then(() => true);

// Page-lifetime state. origins: the originTicks the first Session of each URL
// reported; later sessions are anchored to it and never report their own.
const origins = new Map();
const durations = new Map();
let ms = null;
let sb = null;
let sbMime = null;
let objectUrl = null;
let gen = 0; // bumped whenever the feeding loop must stop
let current = null; // { url, size, opts }
let session = null;
let placing = false; // place the playhead at the first buffered media
let counters = freshCounters();
let lastError = null;
let seeks = [];
let rejectFirst = null;
let resolveFirst = null;
let deinterlacer = null;
let yadifTimeline = [];
// Measurement only: one entry per Fragment, with the session that produced it.
let fragLog = [];
let sessionSeq = 0;
let activeSeq = 0;
let initTimescales = new Map();

function freshCounters() {
  return { sessions: 0, init: 0, media: 0, privateStream: 0, bytesFed: 0, bytesAppended: 0, lastStart: null, polls: 0, grows: 0, pollErrors: 0 };
}
const round = (n) => Math.round(n * 1000) / 1000;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const modPts = (ticks) => ((ticks % PTS_MODULUS) + PTS_MODULUS) % PTS_MODULUS;
const relSeconds = (pts, origin) => modPts(pts - origin) / TICKS_PER_SECOND;

async function fetchRange(url, start, endExclusive) {
  const res = await fetch(url, { headers: { Range: `bytes=${start}-${endExclusive - 1}` } });
  if (res.status !== 206) throw new Error(`Range ${start}-${endExclusive - 1} answered ${res.status}`);
  return new Uint8Array(await res.arrayBuffer());
}

async function probeSize(url) {
  const res = await fetch(url, { headers: { Range: "bytes=0-0" } });
  const match = /\/(\d+)$/.exec(res.headers.get("Content-Range") ?? "");
  await res.arrayBuffer();
  if (!match) throw new Error("server gave no Content-Range; Range reads are required");
  return Number(match[1]);
}

/** Presentation seconds of the first PES timestamp in the PROBE_BYTES at byte. */
async function probeRel(url, byte, origin, size) {
  const pts = firstTimestamp(await fetchRange(url, byte, Math.min(size, byte + PROBE_BYTES)));
  return pts == null || origin == null ? null : relSeconds(pts, origin);
}

/**
 * Presentation seconds of the last PES timestamp in the file's tail.
 * fresh: re-read the tail and do not cache (a growing file's end moves).
 */
async function durationOf(url, size, origin, fresh = false) {
  if (!fresh && durations.has(url)) return durations.get(url);
  const last = lastTimestamp(await fetchRange(url, Math.max(0, size - PROBE_BYTES), size));
  const seconds = last == null ? null : relSeconds(last, origin);
  if (!fresh) durations.set(url, seconds);
  return seconds;
}

/** Bisect (interpolating) for the byte whose presentation time is just before sec. */
async function byteForTime(url, size, origin, sec, duration) {
  const probes = [];
  let lo = { byte: 0, rel: (await probeRel(url, 0, origin, size)) ?? 0 };
  let hi = { byte: size, rel: duration ?? lo.rel + 1 };
  probes.push({ byte: 0, rel: round(lo.rel) });
  const target = Math.max(0, sec - 1);
  for (let i = 0; i < 16; i++) {
    if (lo.rel <= sec && sec - lo.rel <= SEEK_TOLERANCE_SECONDS) break;
    if (hi.byte - lo.byte <= 1 || hi.rel <= lo.rel) break;
    const fraction = (target - lo.rel) / (hi.rel - lo.rel);
    const next = Math.min(hi.byte - 1, Math.max(lo.byte + 1, Math.round(lo.byte + fraction * (hi.byte - lo.byte))));
    const rel = await probeRel(url, next, origin, size);
    if (rel == null) break;
    probes.push({ byte: next, rel: round(rel) });
    if (rel <= target) lo = { byte: next, rel };
    else hi = { byte: next, rel };
  }
  return { byte: lo.byte, achieved: round(lo.rel), probes };
}

/** Find a 188-byte packet boundary at or after byte. */
async function alignToPacket(url, byte, size) {
  const bytes = await fetchRange(url, byte, Math.min(size, byte + 188 * 4));
  for (let i = 0; i < 188 && i + 376 < bytes.length; i++) {
    if (bytes[i] === 0x47 && bytes[i + 188] === 0x47 && bytes[i + 376] === 0x47) return byte + i;
  }
  throw new Error(`no packet sync near byte ${byte}`);
}

function makeSession(url, audioPid) {
  counters.sessions++;
  activeSeq = ++sessionSeq;
  // Only the first session of a URL reports an origin; later ones pass it in.
  const s = new Session(undefined, origins.get(url));
  if (audioPid != null) s.selectAudio(audioPid);
  return s;
}

function noteOrigin(url, sess) {
  if (!origins.has(url) && sess.originTicks != null) {
    origins.set(url, sess.originTicks);
    log("origin", { originTicks: sess.originTicks });
  }
}

async function waitForOrigin(url) {
  const deadline = performance.now() + FIRST_MEDIA_TIMEOUT_MS;
  while (!origins.has(url)) {
    if (performance.now() > deadline) throw new Error("no originTicks from the first session");
    await sleep(100);
  }
  return origins.get(url);
}

function fail(error, where) {
  const message = String(error?.message ?? error);
  lastError = { where, message };
  log(`${where}-error`, { message });
  if (rejectFirst) {
    rejectFirst(new Error(`${where}: ${message}`));
    rejectFirst = null;
  }
}

// --- SourceBuffer plumbing: every op is awaited, so nothing overlaps ----------

function settle() {
  if (!sb || !sb.updating) return Promise.resolve();
  return new Promise((resolve) => sb.addEventListener("updateend", resolve, { once: true }));
}

function appendOnce(data) {
  return new Promise((resolve, reject) => {
    const done = () => {
      sb.removeEventListener("updateend", done);
      sb.removeEventListener("error", failed);
      resolve();
    };
    const failed = () => {
      sb.removeEventListener("updateend", done);
      sb.removeEventListener("error", failed);
      reject(new Error("SourceBuffer error event"));
    };
    sb.addEventListener("updateend", done);
    sb.addEventListener("error", failed);
    try {
      sb.appendBuffer(data);
    } catch (error) {
      sb.removeEventListener("updateend", done);
      sb.removeEventListener("error", failed);
      reject(error);
    }
  });
}

async function removeRange(start, end) {
  await settle();
  if (!sb) return;
  sb.remove(start, end);
  await settle();
}

async function appendWithRoom(data) {
  for (let attempt = 0; ; attempt++) {
    await settle();
    try {
      await appendOnce(data);
      counters.bytesAppended += data.byteLength;
      return;
    } catch (error) {
      if (!(error instanceof DOMException && error.name === "QuotaExceededError") || attempt >= 20) throw error;
      log("quota", { attempt, currentTime: round(video.currentTime) });
      const cut = video.currentTime - 2;
      if (cut > 0 && sb.buffered.length && sb.buffered.start(0) < cut) await removeRange(0, cut);
      else await sleep(500);
    }
  }
}

function bufferedAhead() {
  if (!sb || sb.buffered.length === 0) return 0;
  return sb.buffered.end(sb.buffered.length - 1) - video.currentTime;
}

/** Backpressure and eviction. Returns false once this generation is stale. */
async function waitForRoom(g) {
  while (g === gen) {
    await settle();
    if (sb && sb.buffered.length && video.currentTime - sb.buffered.start(0) > KEEP_BEHIND_SECONDS + 2) {
      await removeRange(0, video.currentTime - KEEP_BEHIND_SECONDS);
    }
    if (bufferedAhead() <= MAX_AHEAD_SECONDS) return true;
    await sleep(250);
  }
  return false;
}

function placeAtMedia() {
  if (!placing || !sb || sb.buffered.length === 0) return;
  placing = false;
  const start = sb.buffered.start(0);
  if (video.currentTime < start) video.currentTime = start;
  log("placed", { bufferedStart: round(start), currentTime: round(video.currentTime) });
}

/** Measurement only: presentation times (session seconds) of every sample in a media fragment. */
function describeFragment(f, seq) {
  return {
    seq,
    start: f.start,
    videoSamples: f.videoSamples,
    audioSamples: f.audioSamples,
    tracks: parseFragment(f.data).map((t) => {
      const meta = initTimescales.get(t.trackId);
      const timescale = meta?.timescale ?? 90000;
      return { id: t.trackId, handler: meta?.handler ?? null, timescale, pres: presentation(t).map((x) => x / timescale) };
    }),
  };
}

async function appendAll(g, fragments) {
  for (const f of fragments) {
    if (g !== gen) return false;
    if (f.kind === "init") {
      initTimescales = parseInit(f.data);
      if (!sb) {
        if (!MediaSource.isTypeSupported(f.mimeCodec)) throw new Error(`unsupported codec ${f.mimeCodec}`);
        sb = ms.addSourceBuffer(f.mimeCodec);
        sb.mode = "segments";
        sb.addEventListener("error", () => fail(new Error("SourceBuffer error event"), "sourcebuffer"));
        sbMime = f.mimeCodec;
      } else if (f.mimeCodec !== sbMime) {
        await settle();
        sb.changeType(f.mimeCodec);
        log("changeType", { mime: f.mimeCodec });
        sbMime = f.mimeCodec;
      }
      await appendWithRoom(f.data);
      counters.init++;
      log("init", { mime: f.mimeCodec, width: f.width, height: f.height, bytes: f.data.byteLength });
    } else if (f.kind === "media") {
      fragLog.push(describeFragment(f, activeSeq));
      await appendWithRoom(f.data);
      counters.media++;
      counters.lastStart = round(f.start);
      placeAtMedia();
      if (deinterlacer) {
        yadifTimeline = [...yadifTimeline, ...f.scans.map((s) => ({ start: s.start, scan: { interlaced: s.interlaced, topFieldFirst: s.topFieldFirst } }))].slice(-64);
        deinterlacer.videoTimeline = yadifTimeline;
      }
      if (resolveFirst) {
        resolveFirst();
        resolveFirst = null;
        rejectFirst = null;
      }
    } else if (f.kind === "private-stream") {
      counters.privateStream++;
    }
  }
  return g === gen;
}

async function ensureDuration(url, size) {
  const origin = origins.get(url);
  if (origin == null || durations.has(url) || !sb || current?.opts.growing) return;
  const seconds = await durationOf(url, size, origin);
  await settle();
  if (seconds == null || !ms || ms.readyState !== "open") return;
  const end = sb.buffered.length ? sb.buffered.end(sb.buffered.length - 1) : 0;
  try {
    ms.duration = Math.max(seconds, end);
    log("duration", { seconds: round(seconds) });
  } catch (error) {
    log("duration-error", { message: String(error.message ?? error) });
  }
}

const GROW_POLL_MIN_MS = 250;
const GROW_POLL_MAX_MS = 2000;

/**
 * Growing file only: the feeder reached the visible end. Re-probe the size with
 * backoff until it passes offset. Returns the new size, or null when superseded.
 * A failed probe is counted and retried, never treated as end of input.
 */
async function waitForGrowth(g, url, offset) {
  let wait = GROW_POLL_MIN_MS;
  while (g === gen) {
    await sleep(wait);
    if (g !== gen) return null;
    counters.polls++;
    try {
      const size = await probeSize(url);
      if (size > offset) {
        counters.grows++;
        log("grew", { offset, size, polls: counters.polls });
        return size;
      }
    } catch (error) {
      counters.pollErrors++;
      log("poll-error", { message: String(error.message ?? error) });
    }
    wait = Math.min(GROW_POLL_MAX_MS, wait * 2);
  }
  return null;
}

/** The feeding loop: Range-read, push, append, until end of input or superseded. */
async function feed(g, url, size, sess, offset, growing = false) {
  try {
    while (true) {
      if (offset >= size) {
        if (!growing) break;
        const grown = await waitForGrowth(g, url, offset);
        if (grown == null) return;
        size = grown;
        if (current) current.size = grown;
        continue;
      }
      if (!(await waitForRoom(g))) return;
      const end = Math.min(size, offset + CHUNK_BYTES);
      const bytes = await fetchRange(url, offset, end);
      if (g !== gen) return;
      offset = end;
      counters.bytesFed += bytes.length;
      let fragments;
      try {
        fragments = sess.push(bytes);
      } catch (error) {
        fail(error, "session");
        return;
      }
      noteOrigin(url, sess);
      if (!(await appendAll(g, fragments))) return;
      await ensureDuration(url, size);
      await sleep(0);
    }
    if (g !== gen) return;
    let tail;
    try {
      tail = sess.finish();
    } catch (error) {
      fail(error, "session");
      return;
    }
    if (!(await appendAll(g, tail))) return;
    await settle();
    if (g === gen && ms.readyState === "open") {
      ms.endOfStream();
      log("ended-stream");
    }
  } catch (error) {
    if (g === gen) fail(error, "append");
  }
}

function teardown() {
  gen++;
  try {
    if (ms && ms.readyState === "open" && !(sb && sb.updating)) ms.endOfStream();
  } catch {
    // Being thrown away anyway.
  }
  if (objectUrl) URL.revokeObjectURL(objectUrl);
  objectUrl = null;
  video.removeAttribute("src");
  video.load();
  ms = null;
  sb = null;
  sbMime = null;
  session = null;
  placing = false;
  counters = freshCounters();
  lastError = null;
  seeks = [];
  rejectFirst = null;
  resolveFirst = null;
  yadifTimeline = [];
  fragLog = [];
  initTimescales = new Map();
}

async function initYadif() {
  const mod = await import("./dist/yadif/index.js");
  deinterlacer = new mod.Deinterlacer(video);
  deinterlacer.start();
  deinterlacer.enabled = true;
  log("yadif", { on: true });
}

async function open(url, opts = {}) {
  await wasmReady;
  teardown();
  const g = gen;
  const size = await probeSize(url);
  current = { url, size, opts };
  const growing = Boolean(opts.growing);
  video.muted = opts.muted ?? true;
  ms = new MediaSource();
  const opened = new Promise((resolve) => ms.addEventListener("sourceopen", resolve, { once: true }));
  objectUrl = URL.createObjectURL(ms);
  video.src = objectUrl;
  await opened;
  if (opts.yadif && !deinterlacer) await initYadif();
  session = makeSession(url, opts.audioPid);
  placing = true;
  const first = new Promise((resolve, reject) => {
    resolveFirst = resolve;
    rejectFirst = reject;
  });
  const started = performance.now();
  feed(g, url, size, session, 0, growing); // deliberately not awaited
  const timer = setTimeout(() => fail(new Error("no media fragment within timeout"), "timeout"), FIRST_MEDIA_TIMEOUT_MS);
  await first.finally(() => clearTimeout(timer));
  video.play().catch((error) => log("play-rejected", { message: String(error.message ?? error) }));
  return { size, firstMediaMs: Math.round(performance.now() - started), originTicks: origins.get(url) ?? null };
}

async function seek(sec) {
  await wasmReady;
  if (!current) throw new Error("open() first");
  const { url } = current;
  const size = current.size;
  const growing = Boolean(current.opts.growing);
  const origin = await waitForOrigin(url);
  const g = ++gen; // stop the running feed before anything else
  const began = performance.now();
  const duration = await durationOf(url, size, origin, growing);
  const found = await byteForTime(url, size, origin, sec, duration);
  const byte = await alignToPacket(url, found.byte, size);
  if (g !== gen) return { superseded: true };
  await settle();
  if (sb && sb.buffered.length) await removeRange(0, Infinity);
  if (g !== gen) return { superseded: true };
  video.currentTime = sec;
  session = makeSession(url, current.opts.audioPid);
  placing = true;
  const record = {
    target: sec,
    byte,
    originTicks: origin,
    achieved: found.achieved,
    probes: found.probes,
    lookupMs: Math.round(performance.now() - began),
  };
  seeks.push(record);
  log("seek", { target: sec, byte, probes: found.probes.length });
  feed(g, url, size, session, byte, growing); // not awaited
  return record;
}

/** Growing file: the visible size and the media time of its last timestamp, read now. */
async function liveEdge() {
  if (!current) throw new Error("open() first");
  const { url } = current;
  const origin = await waitForOrigin(url);
  const size = await probeSize(url);
  const seconds = await durationOf(url, size, origin, true);
  return { size, seconds: seconds == null ? null : round(seconds), currentTime: round(video.currentTime) };
}

function ranges() {
  const out = [];
  for (let i = 0; i < video.buffered.length; i++) out.push([round(video.buffered.start(i)), round(video.buffered.end(i))]);
  return out;
}

function state() {
  const q = video.getVideoPlaybackQuality?.();
  return {
    url: current?.url ?? null,
    currentTime: round(video.currentTime),
    duration: Number.isFinite(video.duration) ? round(video.duration) : null,
    buffered: ranges(),
    originTicks: current ? origins.get(current.url) ?? null : null,
    readyState: video.readyState,
    audioDecodedBytes: video.webkitAudioDecodedByteCount ?? null,
    videoDecodedBytes: video.webkitVideoDecodedByteCount ?? null,
    networkState: video.networkState,
    paused: video.paused,
    ended: video.ended,
    seeking: video.seeking,
    videoWidth: video.videoWidth,
    videoHeight: video.videoHeight,
    playbackQuality: q
      ? { totalVideoFrames: q.totalVideoFrames, droppedVideoFrames: q.droppedVideoFrames, corruptedVideoFrames: q.corruptedVideoFrames }
      : null,
    error: video.error ? { code: video.error.code, message: video.error.message } : null,
    lastError,
    sessionCounters: session ? { dropped: session.dropped, scrambled: session.scrambled, errors: session.errors } : null,
    fragments: { ...counters },
    seeks: seeks.slice(-5),
    yadif: deinterlacer ? { enabled: deinterlacer.enabled, running: deinterlacer.running } : null,
  };
}

/** PNG of the presented frame. mediaTime is the frame's own time, from rVFC. */
function capture(w = 320, h = 180) {
  return new Promise((resolve) => {
    let finished = false;
    const finish = (meta) => {
      if (finished) return;
      finished = true;
      canvas.width = w;
      canvas.height = h;
      ctx.drawImage(video, 0, 0, w, h);
      const { data } = ctx.getImageData(0, 0, w, h);
      let sum = 0;
      for (let i = 0; i < data.length; i += 4) sum += 0.2126 * data[i] + 0.7152 * data[i + 1] + 0.0722 * data[i + 2];
      resolve({
        dataUrl: canvas.toDataURL("image/png"),
        meanLuma: round(sum / (w * h)),
        mediaTime: meta?.mediaTime ?? null,
        presentedFrames: meta?.presentedFrames ?? null,
        currentTime: round(video.currentTime),
        videoWidth: video.videoWidth,
        videoHeight: video.videoHeight,
      });
    };
    if (typeof video.requestVideoFrameCallback === "function" && !video.paused) {
      video.requestVideoFrameCallback((_now, meta) => finish(meta));
      setTimeout(() => finish(null), 2000);
    } else finish(null);
  });
}

window.poc = {
  ready: wasmReady,
  open,
  seek,
  liveEdge,
  play: () => video.play().then(() => true, (error) => String(error.message ?? error)),
  state,
  capture,
  events,
  clock0: t0, // performance.now() origin of events[].t
  get fragLog() {
    return fragLog;
  },
};
