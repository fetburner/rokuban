// The Session off the main thread, optionally with a pool of picture workers
// (the same split as packages/player at 7b008c6: pushDeferred / complete in
// here, PictureEncoder.encode in the pool). poc.js talks to it through
// WorkerSession.
//
// Requests (from poc.js)
//   { type: "start", pool }                       compile the module, start `pool` picture workers
//   { type: "new", seq, originTicks, audioPid }    replace the session; older seqs are dropped
//   { type: "push", seq, bytes } | { type: "finish", seq }
// Replies
//   { type: "ready", pool }                        pool = workers actually running
//   { type: "out", seq, fragments, originTicks, counters, convertMs } | { type: "failed", seq, message }
import init, { Session } from "./dist/mpeg2toh264_wasm.js";

const WASM_URL = new URL("./dist/mpeg2toh264_wasm_bg.wasm", import.meta.url);

let workers = [];
let idle = [];
let pending = null; // { run, outputs, queued, remaining, resolve, reject }
let runs = 0;
let session = null;
let seq = 0;
// Requests run one at a time: with a pool a push awaits the pictures, and the
// next push must not reach the session before this one has completed.
let chain = Promise.resolve();

function fill() {
  if (!pending) return;
  for (;;) {
    const worker = idle.pop();
    if (!worker) return;
    const next = pending.queued.shift();
    if (!next) {
      idle.push(worker);
      return;
    }
    worker.postMessage({ type: "encode", run: pending.run, ...next }, [next.job.buffer]);
  }
}

function took(worker, message) {
  if (message.type === "ready") return;
  idle.push(worker);
  if (!pending) return;
  if (message.run !== pending.run) return fill();
  if (message.type === "failed") {
    const p = pending;
    pending = null;
    return p.reject(new Error(message.message));
  }
  pending.outputs[message.index] = message.output;
  if (--pending.remaining > 0) return fill();
  const p = pending;
  pending = null;
  p.resolve(p.outputs);
}

function runPool(jobs) {
  if (jobs.length === 0) return Promise.resolve([]);
  return new Promise((resolve, reject) => {
    pending = { run: ++runs, outputs: jobs.map(() => null), queued: jobs.map((job, index) => ({ index, job })), remaining: jobs.length, resolve, reject };
    fill();
  });
}

function cancelPool() {
  if (!pending) return;
  const p = pending;
  pending = null;
  p.reject(new Error("abandoned"));
}

async function startPool(module, size) {
  for (let i = 0; i < size; i++) workers.push(new Worker(new URL("./picture-worker.js", import.meta.url), { type: "module" }));
  await Promise.all(
    workers.map(
      (worker) =>
        new Promise((resolve, reject) => {
          worker.onerror = (e) => reject(new Error(e.message || "picture worker failed to load"));
          worker.onmessage = ({ data }) => data.type === "ready" && resolve();
          worker.postMessage({ type: "start", module });
        }),
    ),
  );
  idle = [...workers];
  for (const worker of workers) {
    worker.onmessage = ({ data }) => took(worker, data);
    worker.onerror = () => cancelPool();
  }
}

async function step(first) {
  const fragments = [];
  let progress = first;
  for (;;) {
    fragments.push(...progress.fragments);
    if (progress.jobs.length === 0) return fragments;
    progress = session.complete(await runPool(progress.jobs));
  }
}

async function handle(data) {
  if (data.type === "new") {
    session?.free();
    session = new Session(undefined, data.originTicks ?? undefined);
    if (data.audioPid != null) session.selectAudio(data.audioPid);
    return;
  }
  if (data.seq !== seq) return self.postMessage({ type: "out", seq: data.seq, fragments: [], stale: true });
  const began = performance.now();
  let fragments;
  try {
    if (workers.length === 0) fragments = data.type === "push" ? session.push(data.bytes) : session.finish();
    else fragments = await step(data.type === "push" ? session.pushDeferred(data.bytes) : session.finishDeferred());
  } catch (error) {
    if (data.seq !== seq) return self.postMessage({ type: "out", seq: data.seq, fragments: [], stale: true });
    return self.postMessage({ type: "failed", seq: data.seq, message: String(error?.message ?? error) });
  }
  const convertMs = performance.now() - began;
  const transfer = fragments.filter((f) => f.data).map((f) => f.data.buffer);
  self.postMessage(
    {
      type: "out",
      seq: data.seq,
      fragments,
      originTicks: session.originTicks ?? null,
      counters: { dropped: session.dropped, scrambled: session.scrambled, errors: session.errors },
      convertMs,
    },
    transfer,
  );
}

self.onmessage = ({ data }) => {
  if (data.type === "start") {
    chain = chain.then(async () => {
      const module = await WebAssembly.compileStreaming(fetch(WASM_URL));
      await init({ module_or_path: module });
      if (data.pool > 0) await startPool(module, data.pool);
      self.postMessage({ type: "ready", pool: workers.length });
    });
    return;
  }
  if (data.type === "new") {
    // Out of the chain: the push in hand belongs to the old session, and
    // cancelling its pictures is what lets the new one start sooner.
    seq = data.seq;
    cancelPool();
  }
  chain = chain.then(() => handle(data));
};
