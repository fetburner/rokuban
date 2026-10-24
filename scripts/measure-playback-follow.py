#!/usr/bin/env python3
"""Observe HLS segments produced while a synthetic MPEG-TS file is growing."""

import argparse
import signal
import subprocess
import tempfile
import time
from pathlib import Path

WRITER_SECONDS = 12
READER_START_SECONDS = 2
SEGMENT_SECONDS = 2
DEFAULT_RW_TIMEOUT_US = 2_000_000


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rw-timeout-us', type=int, default=DEFAULT_RW_TIMEOUT_US)
    sigint = parser.add_mutually_exclusive_group()
    sigint.add_argument('--sigint-after-writer', action='store_true')
    sigint.add_argument('--sigint-immediately-after-writer', action='store_true')
    parser.add_argument('--sigint-idle-seconds', type=float, default=1.0)
    args = parser.parse_args()

    version = subprocess.run(
        ['ffmpeg', '-version'], check=True, capture_output=True, text=True
    ).stdout.splitlines()[0]
    print(version)

    with tempfile.TemporaryDirectory(prefix='issue1599-follow-') as tmp:
        root = Path(tmp)
        source = root / 'growing.ts'
        output = root / 'hls'
        output.mkdir()

        writer_cmd = [
            'ffmpeg', '-hide_banner', '-loglevel', 'error', '-y', '-re',
            '-f', 'lavfi', '-i',
            f'testsrc2=size=160x90:rate=25:duration={WRITER_SECONDS}',
            '-an', '-c:v', 'mpeg2video', '-q:v', '3', '-g', '50', '-bf', '0',
            '-sc_threshold', '0', '-mpegts_copyts', '1', '-flush_packets', '1',
            '-f', 'mpegts', str(source),
        ]
        writer = subprocess.Popen(
            writer_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True
        )
        started = time.monotonic()
        reader = None
        try:
            time.sleep(READER_START_SECONDS)
            reader_cmd = [
                'ffmpeg', '-hide_banner', '-loglevel', 'error', '-y',
                '-follow', '1', '-rw_timeout', str(args.rw_timeout_us), '-ss', '1',
                '-i', str(source), '-map', '0:v:0', '-an', '-fps_mode', 'passthrough',
                '-c:v', 'libx264', '-preset', 'ultrafast', '-qp', '0',
                '-pix_fmt', 'yuv420p', '-g', '50', '-keyint_min', '50',
                '-sc_threshold', '0', '-bf', '0',
                '-force_key_frames', f'expr:gte(t,n_forced*{SEGMENT_SECONDS})',
                '-f', 'hls', '-hls_time', str(SEGMENT_SECONDS), '-hls_list_size', '0',
                '-hls_flags', 'temp_file',
                '-hls_segment_filename', str(output / 'seg%03d.ts'),
                str(output / 'index.m3u8'),
            ]
            reader = subprocess.Popen(
                reader_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True
            )
            seen = set()
            events = []
            writer_ended = None
            signal_sent = None
            last_fs_state = None
            last_fs_change = started
            while reader.poll() is None:
                now = time.monotonic()
                if writer_ended is None and writer.poll() is not None:
                    writer_ended = now
                fs_state = tuple(
                    (path.name, path.stat().st_size, path.stat().st_mtime_ns)
                    for path in sorted([source, *output.iterdir()])
                    if path.exists()
                )
                if fs_state != last_fs_state:
                    last_fs_state = fs_state
                    last_fs_change = now
                if (
                    (args.sigint_after_writer or args.sigint_immediately_after_writer)
                    and writer_ended is not None
                    and signal_sent is None
                    and (
                        args.sigint_immediately_after_writer
                        or now - last_fs_change >= args.sigint_idle_seconds
                    )
                ):
                    reader.send_signal(signal.SIGINT)
                    signal_sent = now
                for segment in sorted(output.glob('seg*.ts')):
                    if segment.name not in seen:
                        seen.add(segment.name)
                        events.append((
                            time.monotonic() - started,
                            segment.name,
                            writer.poll() is None,
                            segment.stat().st_size,
                        ))
                time.sleep(0.05)

            reader_ended = time.monotonic()
            writer.wait(timeout=3)
            if writer_ended is None:
                writer_ended = time.monotonic()
            for segment in sorted(output.glob('seg*.ts')):
                if segment.name not in seen:
                    seen.add(segment.name)
                    events.append((
                        reader_ended - started,
                        segment.name,
                        writer.poll() is None,
                        segment.stat().st_size,
                    ))

            manifest = (output / 'index.m3u8').read_text()
            expected_reader_exit = 255 if signal_sent is not None else 0
            if (
                reader.returncode != expected_reader_exit
                or writer.returncode != 0
                or 'ENDLIST' not in manifest
            ):
                raise RuntimeError(
                    f'writer exit={writer.returncode}, reader exit={reader.returncode}; '
                    f'writer stderr={writer.stderr.read()}; reader stderr={reader.stderr.read()}'
                )

            print('segment,elapsed_seconds,writer_alive,bytes')
            for elapsed, name, writer_alive, size in events:
                print(f'{name},{elapsed:.3f},{str(writer_alive).lower()},{size}')
            print(
                f'writer_duration_seconds={writer_ended - started:.3f} '
                f'reader_exit_after_writer_ms={round((reader_ended - writer_ended) * 1000)} '
                f'sigint_mode={"idle" if args.sigint_after_writer else "immediate" if args.sigint_immediately_after_writer else "none"} '
                f'sigint_idle_seconds={args.sigint_idle_seconds if args.sigint_after_writer else "n/a"} '
                f'sigint_sent={signal_sent is not None} '
                f'sigint_to_exit_ms={round((reader_ended - signal_sent) * 1000) if signal_sent else "n/a"} '
                f'reader_exit={reader.returncode} segments={len(seen)} endlist=true'
            )
        finally:
            if reader is not None and reader.poll() is None:
                reader.terminate()
                reader.wait(timeout=3)
            if writer.poll() is None:
                writer.terminate()
                writer.wait(timeout=3)


if __name__ == '__main__':
    main()
