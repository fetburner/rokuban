// Measurement helper (not product code): runs mpeg2toh264 Session in node
// (same WASM as the page) over a file, so timestamps can be compared without a
// browser. Node runs are timing-only; they do not replace the Chrome checks.
//
//   loadLib()                      -> { Session, firstTimestamp, lastTimestamp }
//   runSession(lib, opts)          -> { originTicks, fragments: [...] }
//     opts: { file, size?, startByte?, origin?, audioPid?, passthrough?, untilSec?, chunk? }
//     fragment: { kind, start?, mimeCodec?, tracks?: [{ trackId, timescale, pres: [sec...] }] }
//     pres = presentation time (seconds, session timeline) of each sample.
//   byteForPts(lib, file, size, origin, sec) -> byte offset whose first PES timestamp is at or before sec
import { readFile, open, stat } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { parseInit, parseFragment, presentation } from "./measure-fmp4.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const TICKS = 90000;
const MOD = 2 ** 33;

export async function loadLib() {
  const mod = await import(join(HERE, "dist/mpeg2toh264_wasm.js"));
  const bytes = await readFile(join(HERE, "dist/mpeg2toh264_wasm_bg.wasm"));
  mod.initSync({ module: bytes });
  return mod;
}

async function readAt(fh, start, end) {
  const buf = Buffer.alloc(end - start);
  const { bytesRead } = await fh.read(buf, 0, buf.length, start);
  return new Uint8Array(buf.buffer, buf.byteOffset, bytesRead);
}

const modPts = (t) => ((t % MOD) + MOD) % MOD;

/** Bisect for the byte whose first PES timestamp (relative to origin) is at or before sec. */
export async function byteForPts(lib, file, size, origin, sec, probeBytes = 512 * 1024) {
  const fh = await open(file, "r");
  try {
    const relAt = async (byte) => {
      const pts = lib.firstTimestamp(await readAt(fh, byte, Math.min(size, byte + probeBytes)));
      return pts == null ? null : modPts(pts - origin) / TICKS;
    };
    let lo = { byte: 0, rel: (await relAt(0)) ?? 0 };
    let hi = { byte: size, rel: lo.rel + 1e9 };
    // Find hi.rel by probing the tail once.
    const tail = lib.lastTimestamp(await readAt(fh, Math.max(0, size - probeBytes), size));
    if (tail != null) hi.rel = modPts(tail - origin) / TICKS;
    for (let i = 0; i < 40; i++) {
      if (lo.rel <= sec && sec - lo.rel <= 2) break;
      if (hi.byte - lo.byte <= 1 || hi.rel <= lo.rel) break;
      const f = (sec - 1 - lo.rel) / (hi.rel - lo.rel);
      const next = Math.min(hi.byte - 1, Math.max(lo.byte + 1, Math.round(lo.byte + f * (hi.byte - lo.byte))));
      const rel = await relAt(next);
      if (rel == null) break;
      if (rel <= sec - 1) lo = { byte: next, rel };
      else hi = { byte: next, rel };
    }
    // Align to a packet boundary.
    const probe = await readAt(fh, lo.byte, Math.min(size, lo.byte + 188 * 4));
    for (let i = 0; i < 188 && i + 376 < probe.length; i++) {
      if (probe[i] === 0x47 && probe[i + 188] === 0x47 && probe[i + 376] === 0x47) return { byte: lo.byte + i, rel: lo.rel };
    }
    return { byte: lo.byte, rel: lo.rel };
  } finally {
    await fh.close();
  }
}

/**
 * Run one Session over file from startByte. Stops once a media fragment starts
 * past untilSec (session timeline) when untilSec is given.
 */
export async function runSession(lib, opts) {
  const { file, startByte = 0, audioPid, passthrough = false, untilSec = Infinity, chunk = 3 * 1024 * 1024 } = opts;
  const size = opts.size ?? (await stat(file)).size;
  const sess = new lib.Session(undefined, opts.origin ?? undefined, undefined, undefined, undefined, passthrough);
  if (audioPid != null) sess.selectAudio(audioPid);
  const fragments = [];
  const fh = await open(file, "r");
  let timescales = new Map();
  let stop = false;
  const handle = (list) => {
    for (const f of list) {
      if (f.kind === "init") {
        timescales = parseInit(f.data);
        fragments.push({ kind: "init", mimeCodec: f.mimeCodec, width: f.width, height: f.height });
      } else if (f.kind === "media") {
        const tracks = parseFragment(f.data).map((t) => ({
          trackId: t.trackId,
          timescale: timescales.get(t.trackId)?.timescale ?? null,
          handler: timescales.get(t.trackId)?.handler ?? null,
          pres: presentation(t).map((x) => x / (timescales.get(t.trackId)?.timescale ?? TICKS)),
          durs: t.samples.map((s) => s.dur),
        }));
        fragments.push({ kind: "media", start: f.start, videoSamples: f.videoSamples, audioSamples: f.audioSamples, tracks });
        if (f.start > untilSec) stop = true;
      }
    }
  };
  try {
    for (let off = startByte; off < size && !stop; off += chunk) {
      const bytes = await readAt(fh, off, Math.min(size, off + chunk));
      handle(sess.push(bytes));
      if (stop) break;
    }
    if (!stop) handle(sess.finish());
  } finally {
    await fh.close();
  }
  return { originTicks: sess.originTicks ?? null, dropped: sess.dropped, scrambled: sess.scrambled, errors: sess.errors, fragments };
}
