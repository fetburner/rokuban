# Issue #1017 visual evidence

These screenshots were captured from the final #1017 branch with the Chromium Playwright fixture. The accepted roughs are in the [#1017 issue comment](https://github.com/fetburner/rokuban/issues/1017#issuecomment-5934020232).

The chase fixture uses a test title and synthetic times. It has color bars, no poster, and no series history. The pre-play capture is black and shows the browser's missing-image glyph; the rough uses artwork. The capture has no right-hand episode list because this fixture has no series history. The rough includes that list. The rough desktop images are 2,364 pixels wide; the Playwright capture is 1,280 pixels wide. We compare the player layout and timeline, plus the planned-end marker, extension label, and settings placement. The app shell follows merged #1018.

- [Chase before play](chase-before-play-desktop.png)
- [Chase desktop with settings](chase-desktop-settings.png)
- [Extended chase timeline](chase-extended-desktop.png)
- [Chase phone with settings](chase-phone-settings-390px.png)
- [Original HLS desktop with settings](original-hls-desktop-settings.png)
- [Original HLS phone with settings](original-hls-mobile-settings.png)

Browser acceptance and mutation results are recorded in [acceptance-evidence.md](acceptance-evidence.md).
