// Static server for the PoC. Node stdlib only.
//
//   node serve.mjs [--port 8787] [--media <dir>] [--grow <file> --rate <bytes/s> --lag <seconds>
//                  [--step <seconds>] [--head <seconds>]]
// --step quantizes the lagged point to multiples of step (0 = smooth).
// --head pretends the server started that many seconds earlier (data visible at t=0).
//
// Serves this directory at /, the --media directory at /media/<name>, and in
// grow mode serves the --grow file at /grow/<name> with a visible size of
//   min(actual size, rate * max(0, t - lag))
// where t is seconds since the server started. That simulates a recording
// still being written, seen through a share with visibility lag: HEAD and
// Range responses report only the visible part.
import { createServer } from "node:http";
import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import { basename, extname, join, normalize, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const HERE = fileURLToPath(new URL(".", import.meta.url));

const TYPES = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".wasm": "application/wasm",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".png": "image/png",
  ".ts": "video/mp2t",
  ".m2ts": "video/mp2t",
  ".m2v": "video/mpeg",
  ".mp4": "video/mp4",
};

/** Parse a single `bytes=` range against a known size, or null when none. */
export function parseRange(header, size) {
  if (!header || !header.startsWith("bytes=")) return null;
  const first = header.slice(6).split(",")[0].trim();
  const [a, b] = first.split("-");
  let start;
  let end;
  if (a === "") {
    const suffix = Number(b);
    if (!Number.isFinite(suffix) || suffix <= 0) return "unsatisfiable";
    start = Math.max(0, size - suffix);
    end = size - 1;
  } else {
    start = Number(a);
    end = b === "" || b === undefined ? size - 1 : Math.min(Number(b), size - 1);
    if (!Number.isInteger(start) || start >= size || end < start) return "unsatisfiable";
  }
  return { start, end };
}

/** Serve one file, honouring Range and reporting `visibleSize` as its size. */
function sendFile(req, res, path, visibleSize, contentType) {
  const headers = {
    "Content-Type": contentType,
    "Accept-Ranges": "bytes",
    "Cache-Control": "no-store",
  };
  const range = parseRange(req.headers.range, visibleSize);
  if (range === "unsatisfiable") {
    res.writeHead(416, { ...headers, "Content-Range": `bytes */${visibleSize}` });
    return res.end();
  }
  if (range) {
    const length = range.end - range.start + 1;
    res.writeHead(206, {
      ...headers,
      "Content-Range": `bytes ${range.start}-${range.end}/${visibleSize}`,
      "Content-Length": length,
    });
    if (req.method === "HEAD") return res.end();
    return createReadStream(path, { start: range.start, end: range.end }).pipe(res);
  }
  res.writeHead(200, { ...headers, "Content-Length": visibleSize });
  if (req.method === "HEAD") return res.end();
  return createReadStream(path, { start: 0, end: visibleSize - 1 }).pipe(res);
}

/**
 * Start the server. Returns { url, close, server }.
 * opts: { port, media, grow: { file, rate, lag } }
 */
export function startServer(opts = {}) {
  const started = Date.now();
  const mediaDir = opts.media ? resolve(opts.media) : null;
  const grow = opts.grow ?? null;

  const server = createServer(async (req, res) => {
    try {
      const url = new URL(req.url, "http://localhost");
      const pathname = decodeURIComponent(url.pathname);
      if (req.method !== "GET" && req.method !== "HEAD") {
        res.writeHead(405).end();
        return;
      }

      if (grow && pathname === `/grow/${basename(grow.file)}`) {
        const actual = (await stat(grow.file)).size;
        const t = (Date.now() - started) / 1000 + (grow.head ?? 0);
        const lagged = Math.max(0, t - grow.lag);
        // step > 0: the visible point only moves in steps of `step` seconds (bursty NFS visibility).
        const quantized = grow.step > 0 ? Math.floor(lagged / grow.step) * grow.step : lagged;
        const visible = Math.min(actual, Math.floor(grow.rate * quantized));
        return sendFile(req, res, grow.file, visible, TYPES[extname(grow.file)] ?? "application/octet-stream");
      }

      if (mediaDir && pathname.startsWith("/media/")) {
        const path = join(mediaDir, normalize(pathname.slice("/media/".length)));
        if (!path.startsWith(mediaDir + "/")) return res.writeHead(403).end();
        const info = await stat(path).catch(() => null);
        if (!info?.isFile()) return res.writeHead(404).end();
        return sendFile(req, res, path, info.size, TYPES[extname(path)] ?? "application/octet-stream");
      }

      const rel = pathname === "/" ? "index.html" : normalize(pathname.slice(1));
      const path = join(HERE, rel);
      if (!path.startsWith(HERE)) return res.writeHead(403).end();
      const info = await stat(path).catch(() => null);
      if (!info?.isFile()) return res.writeHead(404).end();
      return sendFile(req, res, path, info.size, TYPES[extname(path)] ?? "application/octet-stream");
    } catch (error) {
      res.writeHead(500, { "Content-Type": "text/plain" }).end(String(error));
    }
  });

  return new Promise((resolveStart, reject) => {
    server.on("error", reject);
    server.listen(opts.port ?? 0, "127.0.0.1", () => {
      const { port } = server.address();
      resolveStart({
        url: `http://127.0.0.1:${port}`,
        server,
        close: () => new Promise((done) => server.close(done)),
      });
    });
  });
}

// CLI
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const args = process.argv.slice(2);
  const arg = (name) => {
    const i = args.indexOf(name);
    return i >= 0 ? args[i + 1] : undefined;
  };
  const growFile = arg("--grow");
  const srv = await startServer({
    port: Number(arg("--port") ?? 8787),
    media: arg("--media"),
    grow: growFile
      ? {
          file: resolve(growFile),
          rate: Number(arg("--rate") ?? NaN),
          lag: Number(arg("--lag") ?? 0),
          step: Number(arg("--step") ?? 0),
          head: Number(arg("--head") ?? 0),
        }
      : null,
  });
  if (growFile && !Number.isFinite(Number(arg("--rate")))) {
    console.error("--grow needs --rate <bytes/s>");
    process.exit(2);
  }
  console.log(`serving ${HERE} at ${srv.url}`);
  if (arg("--media")) console.log(`  /media/ -> ${resolve(arg("--media"))}`);
  if (growFile) console.log(`  /grow/${basename(growFile)} (rate ${arg("--rate")} B/s, lag ${arg("--lag") ?? 0} s)`);
}
