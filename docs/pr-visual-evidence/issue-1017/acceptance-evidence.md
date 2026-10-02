# Issue #1017 acceptance evidence

## Browser acceptance

The final branch passed the five browser checks in the issue:

| Acceptance check | Browser run | Result |
|---|---|---|
| One player while recording; no old headings, close button, or chase start button | `pnpm --dir web e2e:chase` in Chromium and WebKit | Pass |
| Finishing the recording keeps the same playing `<video>` and chase session | `pnpm --dir web e2e:chase` in Chromium and WebKit | Pass; same video, `paused=false`, and no extra chase playlist request after the status update |
| Seeking outside the chase range selects original-HLS offset and carries the selected position | `pnpm --dir web e2e:chase` in Chromium and WebKit | Pass; target 11s requested offset 11 and the timeline position remained 11s |
| Encoding completion leaves original HLS active; the next range exit selects encoded media | `pnpm --dir web e2e:recording-original-vod` in Chromium | Pass; no encoded request at completion, then encoded requests on range exit |
| Opening original-HLS recording does not request a playlist before play | `pnpm --dir web e2e:recording-original-vod` in Chromium | Pass; playlist count was zero before play and increased after clicking play |
| WebKit chase-to-original-HLS transition | `pnpm --dir web e2e:chase` in WebKit | Pass |

The chase fixture now derives `startedAt` from its fixed `startAt` value. This avoids a millisecond boundary changing the extension label from `70:10` to `70:11`.

The original-HLS E2E reports a missing start button as an assertion failure. It no longer aborts on a long, unhandled wait. This let the browser check detect the autoplay mutation.

## RED check on the pre-#1017 baseline

The baseline was the #1015 head (`2dfeac295da3f0c826d1761fe47af965b69be541`). It predates #1017's playback-source implementation.

Its build was blocked by an unresolved `selectedChaseOffsetRef` reference in the disposable checkout. I removed only that dead reference from the temporary copy. The build then succeeded, and the current E2E scripts failed against the old UI:

- `e2e:chase` timed out waiting for the new `recording-playback-poster` state.
- `e2e:recording-original-vod` failed because the reloaded page lacked `recording-playback-start`. The old UI had already started original HLS automatically.

These runs show the new-state checks are red on the pre-feature UI. Each run stopped at its first missing-state assertion. Later browser assertions were not independently exercised on the baseline.

## Mutation checks

Each mutation below was applied only in a disposable working copy, then reverted. The named current acceptance test failed because of that mutation.

| Mutation | Detection |
|---|---|
| Restore the old visible `追っかけ再生` heading | Chromium `e2e:chase` failed the one-player/old-UI assertion: `videos=1, headings=1, close=0, chaseStart=0`. |
| Add `recording.status` to the actual `LivePlayer` remount key | Chromium `e2e:chase` failed after recording completion: `sameVideo=false`, `paused=true`, and chase playlist requests increased. |
| Drop carried position for `source-range-exit` | Chromium `e2e:chase` failed the range-exit check: no offset request, selected position 11s, axis position 0. |
| Add `encodedAssets.length` to the original-HLS player's actual remount key | Chromium `e2e:recording-original-vod` failed because encoding completion changed the player/source. |
| Initialize original-HLS as already started | Chromium `e2e:recording-original-vod` failed the pre-play playlist assertion and the autoplay checks. |
| Force the source selector to ignore encoded availability | Source-selection unit table failed 8 of 20 cases, including encoded priority and carried-position cases. |
| Treat explicit position `0` as absent | The explicit-zero source-transition unit case failed. |
| Omit carried position for each of `source-range-exit`, `ended`, `source-error`, and `reopen` | The corresponding transition-table rows failed; the error mutation also failed the explicit-zero row. |

The status-only mutation changed the component's real `LivePlayer` key. It did not change only the pure helper.

This confirms the browser E2E detects a player recreation when recording status changes.

## Other checks

- `pnpm --dir web test` — 86 files and 1,673 tests passed.
- `pnpm --dir web lint` — passed with two existing Fast Refresh warnings.
- `pnpm docs:lint` — passed.
- `pnpm --dir web build` — passed on the same production source. It emitted existing Zod annotation and chunk-size warnings.

The estimated chase byte-offset drift remains unmeasured and is marked as unverified in `docs/frontend/recordings.md`.
