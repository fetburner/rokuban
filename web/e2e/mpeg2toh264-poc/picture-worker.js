// One picture at a time: the PoC's copy of packages/player/src/picture-worker.ts
// at 7b008c6, as a plain module worker. Started by session-worker.js.
import { initSync, PictureEncoder } from "./dist/mpeg2toh264_wasm.js";

let encoder = null;

self.onmessage = ({ data }) => {
  if (data.type === "start") {
    initSync({ module: data.module });
    encoder = new PictureEncoder();
    self.postMessage({ type: "ready" });
    return;
  }
  const { run, index, job } = data;
  try {
    const output = encoder.encode(job);
    self.postMessage({ type: "encoded", run, index, output }, [output.buffer]);
  } catch (error) {
    self.postMessage({ type: "failed", run, index, message: String(error?.message ?? error) });
  }
};
