# Issue #1029 browser evidence

Captured from the production build with `web/e2e/reservation-detail-layout.mjs`.
Desktop is 1280×800; mobile is 360×800. Both light and dark color schemes are included.

- `*-normal`: reservation title, program-table link, and program details.
- `*-menu`: overflow menu with the cancel action.
- `*-404`: EPG 404 hides program details while the reservation remains visible.
- `*-5xx`: EPG 5xx shows the required failure message.
