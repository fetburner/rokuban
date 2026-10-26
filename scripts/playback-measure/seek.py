#!/usr/bin/env python3
"""Compare TS input seeks inside a Pod, exporting timestamps and hashes only."""
import argparse
import json
import subprocess
import uuid
from fractions import Fraction
from pathlib import Path


def frames(text):
    tb = None
    result = []
    for line in text.splitlines():
        if line.startswith("#tb 0:"):
            tb = Fraction(line.split(":", 1)[1].strip())
        elif line.startswith("0,"):
            fields = line.split(",")
            if tb is None:
                raise ValueError("missing framehash time base")
            result.append((int(fields[2]) * tb, fields[-1].strip()))
    return result


def errors(reference, output, target, interval):
    """Compare each segment's source frame to the first frame on/after its grid."""
    rows = []
    for n, (_, digest) in enumerate(output):
        matches = [i for i, (_, value) in enumerate(reference) if value == digest]
        if len(matches) != 1:
            raise ValueError(f"segment {n}: {len(matches)} matching source frames")
        expected = next((i for i, (pts, _) in enumerate(reference)
                         if pts >= target + n * interval), None)
        if expected is None:
            raise ValueError(f"segment {n}: target outside reference")
        rows.append({"segment": n, "source_frame": matches[0],
                     "expected_frame": expected, "error_frames": matches[0] - expected})
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", default="rokuban")
    parser.add_argument("--pod", help="run inside this Pod; without it, run the local ffmpeg/ffprobe")
    parser.add_argument("--scratch-root", default="/tmp", help="scratch directory root (local mode)")
    parser.add_argument("--input", required=True, help="read-only path inside the Pod (or a local path)")
    parser.add_argument("--video-origin-90k", type=int, required=True)
    parser.add_argument("--format-start-90k", type=int, required=True)
    parser.add_argument("--offset", type=Fraction, required=True)
    parser.add_argument("--reference-frames", type=int, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--gop", type=int, default=60, help="-g / -keyint_min of the measured output (frames)")
    parser.add_argument("--force-key-frames", default="expr:gte(t,n_forced*2)", help="-force_key_frames of the measured output")
    parser.add_argument("--hls-time", default="2", help="-hls_time of the measured output")
    parser.add_argument("--enc-time-base", help="-enc_time_base:v for the measured output, for example 1/90000")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    local = args.pod is None
    base = [] if local else ["kubectl", "-n", args.namespace, "exec", args.pod, "--"]

    def remote(*command, timeout=90):
        if local and command[0] == "timeout":
            command = command[2:]  # GNU timeout is absent on macOS; subprocess's timeout applies
        return subprocess.run(base + list(command), capture_output=True, text=True,
                              timeout=timeout)

    def checked(*command, timeout=90):
        r = remote(*command, timeout=timeout)
        if r.returncode:
            raise RuntimeError(r.stderr)
        return r.stdout

    scratch = (args.scratch_root if local else "/tmp") + "/rp3-seek-" + uuid.uuid4().hex
    checked("mkdir", scratch)
    # The input MPEG-TS video time base must be 1/90000 for the select expression.
    metadata = json.loads(checked("ffprobe", "-v", "quiet", "-select_streams", "v:0",
                                 "-show_entries", "stream=time_base", "-of", "json", args.input))
    if metadata["streams"][0]["time_base"] != "1/90000":
        raise ValueError("this runner requires the MPEG-TS 90 kHz time base")
    reference_path = scratch + "/reference.hash"
    checked("timeout", "300", "ffmpeg", "-hide_banner", "-loglevel", "error",
            "-threads", "1", "-copyts", "-i", args.input, "-map", "0:v:0", "-an",
            "-frames:v", str(args.reference_frames), "-vf", "scale=320:180,format=yuv420p",
            "-fps_mode", "passthrough", "-enc_time_base:v", "1/90000", "-f", "framehash", "-hash", "md5", reference_path, timeout=360)
    reference_text = checked("cat", reference_path)
    (args.output / "reference.hash").write_text(reference_text)
    reference = frames(reference_text)
    target = Fraction(args.video_origin_90k, 90000) + args.offset
    target90 = target * 90000
    if target90.denominator != 1:
        raise ValueError("target must be an integer 90 kHz tick")
    modes = [("relative", ["-ss", str(float(args.offset))]),
             ("absolute", ["-seek_timestamp", "1", "-ss", f"{float(target):.6f}"])]
    cases = [(name, copy, seek) for name, seek in modes for copy in (False, True)]
    corrected_seek = ["-ss", f"{float(target - 2 - Fraction(args.format_start_90k, 90000)):.6f}"]
    cases.append(("corrected", True, corrected_seek))
    # Same seek as "corrected", with trim=start=T instead of select (frame containing T is kept).
    cases.append(("corrected-trim", True, corrected_seek))
    # Absolute seek with -noaccurate_seek, the product's seek option.
    cases.append(("absolute-noaccurate", True,
                  ["-seek_timestamp", "1", "-noaccurate_seek", "-ss", f"{float(target):.6f}"]))
    result = []
    for name, copy, seek in cases:
        key = name + ("-copy" if copy else "-reset")
        directory = scratch + "/" + key
        checked("mkdir", directory)
        vf = "scale=320:180,format=yuv420p"
        if name == "corrected":
            vf = f"select=gte(pts\\,{int(target90)}),setpts=PTS-{int(target90)}," + vf
        elif name == "corrected-trim":
            vf = f"trim=start={float(target):.9f},setpts=PTS-{int(target90)}," + vf
        r = remote("timeout", "60", "ffmpeg", "-hide_banner", "-loglevel", "error",
                   "-threads", "1", *(["-copyts"] if copy else []), *seek,
                   "-i", args.input, "-map", "0:v:0", "-an", "-frames:v", "420",
                   "-vf", vf, "-fps_mode", "passthrough", "-c:v", "libx264",
                   "-threads", "1", "-preset", "ultrafast", "-qp", "0", "-g", str(args.gop),
                   "-keyint_min", str(args.gop), "-sc_threshold", "0", "-bf", "0",
                   "-force_key_frames", args.force_key_frames, *(["-enc_time_base:v", args.enc_time_base] if args.enc_time_base else []), "-f", "hls",
                   "-hls_time", args.hls_time, "-hls_list_size", "0", "-hls_flags", "temp_file",
                   "-hls_segment_filename", directory + "/seg%03d.ts", directory + "/index.m3u8")
        (args.output / (key + ".stderr")).write_text(r.stderr)
        # Empty TS files (for example after timeout) are not successful frames.
        script = r'''for s in "$DIR"/seg*.ts; do
[ -s "$s" ] || continue
echo SEGMENT=$(basename "$s")
ffprobe -v quiet -select_streams v:0 -show_packets -show_entries packet=pts -of csv=p=0 "$s" | head -1
ffmpeg -v error -threads 1 -i "$s" -map 0:v:0 -an -frames:v 1 -f framehash -hash md5 -
done'''
        hashes = checked("env", "DIR=" + directory, "sh", "-c", script)
        (args.output / (key + ".hashes")).write_text(hashes)
        row = {"case": key, "exit": r.returncode, "segments": hashes.count("SEGMENT="),
               "frames": errors(reference, frames(hashes), target, Fraction(2))}
        result.append(row)
        print(json.dumps(row), flush=True)
    (args.output / "summary.json").write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
