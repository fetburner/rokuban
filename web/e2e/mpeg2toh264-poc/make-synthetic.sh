#!/usr/bin/env bash
# Generate the synthetic 60 s interlaced MPEG-2 transport stream used by the
# smoke test. Synthetic only: no broadcast or recorded material.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$here/out"
ffmpeg -y -hide_banner -loglevel error \
  -f lavfi -i testsrc2=size=1920x1080:rate=30000/1001 \
  -f lavfi -i sine=frequency=1000 \
  -t 60 -c:v mpeg2video -flags +ilme+ildct -field_order tt -b:v 15M \
  -c:a aac -f mpegts "$here/out/synthetic.ts"
ls -l "$here/out/synthetic.ts"
