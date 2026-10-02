# Issue #1017 visual evidence

These screenshots were captured from the final #1017 branch with the Chromium Playwright fixture. The accepted roughs are in the [#1017 issue comment](https://github.com/fetburner/rokuban/issues/1017#issuecomment-5934020232).

The fixture uses color bars and has no poster. Its pre-play screen is black and shows the browser's missing-image glyph where the rough uses artwork. The playing image also differs from the rough. The rough desktop images are 2,364 pixels wide; the Playwright capture is 1,280 pixels wide. Fixture times differ from the rough's sample values. We compare the player layout and timeline, plus the planned-end marker, extension label, and settings placement. The app shell follows merged #1018.

- [Chase before play](chase-before-play-desktop.png)
- [Chase desktop with settings](chase-desktop-settings.png)
- [Extended chase timeline](chase-extended-desktop.png)
- [Chase phone with settings](chase-phone-settings-390px.png)
- [Original HLS desktop with settings](original-hls-desktop-settings.png)
- [Original HLS phone with settings](original-hls-mobile-settings.png)

Browser acceptance and mutation results are recorded in [acceptance-evidence.md](acceptance-evidence.md).
