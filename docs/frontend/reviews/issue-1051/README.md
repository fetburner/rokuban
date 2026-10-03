# Issue #1051 visual review

Screenshots were captured by `web/e2e/design.mjs` in light mode on 2026-10-03:

- `live-1180x900.png`: 1180×900 viewport, compared with the approved #1022 rough.
- `live-1920x1080.png`: 1920×1080 viewport.
- `live-2560x1440.png`: 2560×1440 viewport with the long channel-list fixture.
- `live-360x844.png`: 360×844 mobile viewport.

The screenshots show the selection preview so the capture does not depend on a real
stream. The #1022 rough shows active playback, its settings menu, and auto-downgrade
notice; those media-state details differ, while the player frame and surrounding
layout are what this comparison measures. The local fixture has four sample channels,
so the 1180px screenshot shows fewer rows than the rough.
