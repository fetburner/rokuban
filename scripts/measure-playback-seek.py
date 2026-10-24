#!/usr/bin/env python3
"""Measure HLS segment seek drift with synthetic MPEG-TS fixtures."""

import json
import subprocess
import tempfile
from fractions import Fraction
from pathlib import Path

FPS = 25
SEGMENT_SECONDS = 2
GOP_FRAMES = FPS * SEGMENT_SECONDS
OFFSET_SECONDS = 10.36


def run(args):
    return subprocess.run(args, check=True, text=True, capture_output=True)


def probe(path):
    out = run([
        'ffprobe', '-v', 'error', '-show_entries',
        'format=start_time,duration:stream=index,codec_type,start_time,time_base',
        '-of', 'json', str(path),
    ])
    return json.loads(out.stdout)


def video_packet_pts(path):
    out = run([
        'ffprobe', '-v', 'error', '-select_streams', 'v:0', '-show_packets',
        '-show_entries', 'packet=pts_time', '-of', 'csv=p=0', str(path),
    ])
    return [float(line.split(',')[0]) for line in out.stdout.splitlines() if line]


def frame_hashes(path):
    out = run([
        'ffmpeg', '-hide_banner', '-loglevel', 'error', '-i', str(path),
        '-map', '0:v:0', '-f', 'framehash', '-hash', 'md5', '-',
    ]).stdout
    timebase = None
    hashes = []
    for line in out.splitlines():
        if line.startswith('#tb '):
            _, _, tb = line.split()
            timebase = Fraction(tb)
        elif line and not line.startswith('#'):
            fields = [field.strip() for field in line.split(',')]
            hashes.append((int(fields[2]), fields[5]))
    if timebase is None or not hashes:
        raise RuntimeError(f'no framehash data for {path}')
    return timebase, hashes


def make_fixture(path, wrap=False):
    if wrap:
        cmd = [
            'ffmpeg', '-hide_banner', '-loglevel', 'error', '-y',
            '-f', 'lavfi', '-i', f'testsrc2=size=160x90:rate={FPS}:duration=20',
            '-map', '0:v:0', '-c:v', 'mpeg2video', '-q:v', '3', '-g', str(GOP_FRAMES),
            '-bf', '0', '-sc_threshold', '0', '-output_ts_offset', '95430',
            '-mpegts_copyts', '1', '-muxdelay', '0', '-muxpreload', '0',
            '-f', 'mpegts', str(path),
        ]
    else:
        cmd = [
            'ffmpeg', '-hide_banner', '-loglevel', 'error', '-y',
            '-itsoffset', '5400', '-f', 'lavfi', '-i',
            f'testsrc2=size=160x90:rate={FPS}:duration=30',
            '-itsoffset', '5400', '-f', 'lavfi', '-i',
            'sine=frequency=440:sample_rate=48000:duration=30',
            '-f', 'lavfi', '-i', 'sine=frequency=880:sample_rate=48000:duration=1',
            '-map', '0:v:0', '-map', '1:a:0', '-map', '2:a:0',
            '-c:v', 'mpeg2video', '-q:v', '3', '-g', str(GOP_FRAMES), '-bf', '0',
            '-sc_threshold', '0', '-c:a', 'mp2', '-b:a', '128k',
            '-mpegts_copyts', '1', '-muxdelay', '0', '-muxpreload', '0',
            '-f', 'mpegts', str(path),
        ]
    run(cmd)


def make_hls(src, outdir, seek_kind, copyts, origin_pts, offset):
    outdir.mkdir()
    target = origin_pts + offset
    cmd = ['ffmpeg', '-hide_banner', '-loglevel', 'error', '-y']
    if copyts:
        cmd.append('-copyts')
    if seek_kind == 'relative':
        cmd += ['-ss', str(offset)]
    elif seek_kind == 'absolute':
        cmd += ['-seek_timestamp', '1', '-ss', f'{target:.9f}']
    elif seek_kind == 'preroll-trim':
        cmd += ['-seek_timestamp', '1', '-ss', f'{target - 2:.9f}']
    cmd += ['-i', str(src), '-map', '0:v:0', '-an', '-fps_mode', 'passthrough']
    if seek_kind == 'preroll-trim':
        cmd += ['-vf', f'trim=start={target:.9f},setpts=PTS-STARTPTS']
    cmd += [
        '-c:v', 'libx264', '-preset', 'ultrafast', '-qp', '0',
        '-pix_fmt', 'yuv420p', '-g', str(GOP_FRAMES), '-keyint_min', str(GOP_FRAMES),
        '-sc_threshold', '0', '-bf', '0',
        '-force_key_frames', f'expr:gte(t,n_forced*{SEGMENT_SECONDS})',
        '-f', 'hls', '-hls_time', str(SEGMENT_SECONDS), '-hls_list_size', '0',
        '-hls_segment_filename', str(outdir / 'seg%03d.ts'),
        str(outdir / 'index.m3u8'),
    ]
    run(cmd)


def main():
    print(run(['ffmpeg', '-version']).stdout.splitlines()[0])
    with tempfile.TemporaryDirectory(prefix='issue1599-') as tmp:
        root = Path(tmp)
        for wrap in (False, True):
            name = 'wrap' if wrap else 'old-pes'
            src = root / f'{name}.ts'
            make_fixture(src, wrap)
            info = probe(src)
            stream = next(s for s in info['streams'] if s['codec_type'] == 'video')
            origin_pts = float(stream['start_time'])
            packets = video_packet_pts(src)
            print(
                f'{name}: format.start_time={info["format"]["start_time"]}, '
                f'video.start_time={origin_pts}, video_packets={len(packets)}, '
                f'packet_range={packets[0]:.6f}..{packets[-1]:.6f}, '
                f'crosses_zero={min(packets) < 0 < max(packets)}'
            )
            source_tb, src_hashes = frame_hashes(src)
            by_hash = {}
            for idx, (_, h) in enumerate(src_hashes):
                by_hash.setdefault(h, []).append(idx)
            print(f'  source frames={len(src_hashes)} unique_hashes={len(by_hash)} duplicates={len(src_hashes)-len(by_hash)} tb={source_tb}')
            variants = [('relative', False), ('relative', True), ('absolute', False), ('absolute', True), ('preroll-trim', True)]
            print('  segment,variant,copyts,observed_source_pts,expected_source_pts,error_frames,mux_first_video_packet_pts')
            for kind, copyts in variants:
                tag = f'{kind}-copyts-{int(copyts)}'
                outdir = root / f'{name}-{tag}'
                make_hls(src, outdir, kind, copyts, origin_pts, OFFSET_SECONDS)
                expected0 = round(OFFSET_SECONDS * FPS)
                for n, seg in enumerate(sorted(outdir.glob('seg*.ts'))):
                    _, seg_frames = frame_hashes(seg)
                    h = seg_frames[0][1]
                    matches = by_hash.get(h, [])
                    if len(matches) != 1:
                        raise RuntimeError(f'{tag} {seg.name}: hash matches {matches}')
                    actual = matches[0]
                    expected = expected0 + n * SEGMENT_SECONDS * FPS
                    observed_pts = origin_pts + actual / FPS
                    expected_pts = origin_pts + OFFSET_SECONDS + n * SEGMENT_SECONDS
                    muxpts = video_packet_pts(seg)[0]
                    print(
                        f'  {n},{kind},{int(copyts)},{observed_pts:.6f},'
                        f'{expected_pts:.6f},{actual-expected},{muxpts:.6f}'
                    )

if __name__ == '__main__':
    main()
