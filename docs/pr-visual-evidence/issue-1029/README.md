# Issue #1029 visual evidence

The issue has no static screen rough. I compared the page against its written acceptance criteria.
I also checked `docs/frontend/reservations.md`, `docs/frontend/programs.md`, and `docs/frontend/design.md`.

| Viewport | Browser capture |
| --- | --- |
| Desktop, 1280 × 800 viewport | [desktop.png](desktop.png) |
| Phone, 360 px viewport | [smartphone.png](smartphone.png) |

Both captures show program details beneath the reservation title.
They also show the metadata link and the open header overflow menu.
The menu stays inside the viewport at both widths.
The cancel item measures 160.5 × 27.4 CSS px on desktop.
It measures 157.6 × 26.9 CSS px on the phone viewport.

The browser acceptance script is `web/e2e/reservation-detail-layout.mjs`.
Its pre-change run reported missing program details, metadata link, and header menu at both widths.
The post-change run passed all checks.

## Mutation checks

A temporary 404 mutation disabled hiding the program details.
The 404 test failed because the program error message became visible.
A temporary `at + 1ms` mutation changed the metadata destination.
The link test failed on the epoch mismatch.
Both mutations were reverted before the commit.
