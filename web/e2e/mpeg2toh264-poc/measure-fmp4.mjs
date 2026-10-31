// Measurement helper (not product code): reads the timing of an fMP4 fragment
// that mpeg2toh264 emitted. Used by poc.js (browser) and the node scripts.
//
//   parseInit(bytes)     -> Map<trackId, { timescale, handler }>   from the moov
//   parseFragment(bytes) -> [{ trackId, baseDecodeTime, samples: [{ dur, cto }] }]
//   presentation(track)  -> [ticks]  presentation time of each sample, in the track timescale
//                           (baseDecodeTime + running decode durations + composition offset)

const CONTAINERS = new Set(["moov", "trak", "mdia", "minf", "stbl", "moof", "traf", "mvex"]);

function boxes(bytes, start, end) {
  const out = [];
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let pos = start;
  while (pos + 8 <= end) {
    let size = view.getUint32(pos);
    const type = String.fromCharCode(...bytes.subarray(pos + 4, pos + 8));
    let header = 8;
    if (size === 1) {
      size = Number(view.getBigUint64(pos + 8));
      header = 16;
    } else if (size === 0) {
      size = end - pos;
    }
    if (size < header || pos + size > end) break;
    out.push({ type, body: pos + header, end: pos + size });
    pos += size;
  }
  return out;
}

function u32(view, p) {
  return view.getUint32(p);
}

export function parseInit(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const tracks = new Map();
  const walk = (start, end, ctx) => {
    for (const b of boxes(bytes, start, end)) {
      if (b.type === "tkhd") {
        const v = bytes[b.body];
        const id = u32(view, b.body + (v === 1 ? 20 : 12));
        ctx.id = id;
      } else if (b.type === "mdhd") {
        const v = bytes[b.body];
        ctx.timescale = u32(view, b.body + (v === 1 ? 20 : 12));
      } else if (b.type === "hdlr") {
        ctx.handler = String.fromCharCode(...bytes.subarray(b.body + 8, b.body + 12));
      } else if (CONTAINERS.has(b.type)) {
        if (b.type === "trak") {
          const t = {};
          walk(b.body, b.end, t);
          if (t.id != null) tracks.set(t.id, { timescale: t.timescale, handler: t.handler });
        } else {
          walk(b.body, b.end, ctx);
        }
      }
    }
  };
  walk(0, bytes.length, {});
  return tracks;
}

/** Parse every moof in one media fragment. */
export function parseFragment(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const out = [];
  for (const moof of boxes(bytes, 0, bytes.length).filter((b) => b.type === "moof")) {
    for (const traf of boxes(bytes, moof.body, moof.end).filter((b) => b.type === "traf")) {
      const track = { trackId: null, baseDecodeTime: 0, defaultDur: 0, defaultSize: 0, samples: [] };
      for (const b of boxes(bytes, traf.body, traf.end)) {
        if (b.type === "tfhd") {
          const flags = u32(view, b.body) & 0xffffff;
          let p = b.body + 4;
          track.trackId = u32(view, p);
          p += 4;
          if (flags & 0x1) p += 8;
          if (flags & 0x2) p += 4;
          if (flags & 0x8) track.defaultDur = u32(view, p), (p += 4);
          if (flags & 0x10) track.defaultSize = u32(view, p), (p += 4);
        } else if (b.type === "tfdt") {
          const v = bytes[b.body];
          track.baseDecodeTime = v === 1 ? Number(view.getBigUint64(b.body + 4)) : u32(view, b.body + 4);
        } else if (b.type === "trun") {
          const v = bytes[b.body];
          const flags = u32(view, b.body) & 0xffffff;
          const count = u32(view, b.body + 4);
          let p = b.body + 8;
          if (flags & 0x1) p += 4;
          if (flags & 0x4) p += 4;
          for (let i = 0; i < count; i++) {
            const s = { dur: track.defaultDur, cto: 0 };
            if (flags & 0x100) s.dur = u32(view, p), (p += 4);
            if (flags & 0x200) p += 4;
            if (flags & 0x400) p += 4;
            if (flags & 0x800) {
              s.cto = v === 1 ? view.getInt32(p) : u32(view, p);
              p += 4;
            }
            track.samples.push(s);
          }
        }
      }
      out.push(track);
    }
  }
  return out;
}

/** Presentation ticks (track timescale) of each sample of one parsed track run. */
export function presentation(track) {
  const ticks = [];
  let t = track.baseDecodeTime;
  for (const s of track.samples) {
    ticks.push(t + s.cto);
    t += s.dur;
  }
  return ticks;
}
