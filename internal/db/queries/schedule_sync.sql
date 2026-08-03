-- reservation_id は「この observed schedule がどの reservations 行に対応するか」の
-- 便宜的なポインタとして reconciler.observeSchedules が書く。
--
-- **この列を読む本番コードは 1 つも無い**（この列を含む唯一の SELECT である
-- ListScheduleSyncsBySite はどこからも呼ばれていない。reconciler の「自分が作った
-- schedule か」の判定は常に tags = mirakc.IsOurs で行う）。issue #99 が挙げた
-- 「予約の再実体化で observed 行が外部産と同じ見た目になる」という症状は、
-- 読み手が無いので実害としては存在しない ---- FK（ON DELETE SET NULL）を外す
-- 対処も検討したが、外すとこの列は「削除済み予約を指す古い id」を持ちうるように
-- なり、NULL より紛らわしくなる（インシデント対応で直接 SELECT する人を誤らせる）。
-- 書き手はいるが読み手がいない列なので、**正しい始末は列自体を落とすこと**
-- （CLAUDE.md 不変条件 10 / 11「これを書く / 使うコードは今あるか」）。
-- reconciler を触る必要があるため #101 の後の別タスクに切り出した。
-- name: UpsertScheduleSync :exec
INSERT INTO schedule_sync (
    site, program_id, reservation_id, state,
    options, tags, failed_reason, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (site, program_id) DO UPDATE SET
    reservation_id = EXCLUDED.reservation_id,
    state          = EXCLUDED.state,
    options        = EXCLUDED.options,
    tags           = EXCLUDED.tags,
    failed_reason  = EXCLUDED.failed_reason,
    observed_at    = now();

-- name: ListScheduleSyncsBySite :many
SELECT * FROM schedule_sync WHERE site = $1;

-- name: DeleteStaleScheduleSyncs :exec
DELETE FROM schedule_sync
WHERE site = $1 AND observed_at < $2;
