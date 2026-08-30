-- schedule_sync は reservation_id 列（observed schedule がどの reservations
-- 行に対応するかの便宜的なポインタ）を持たない --- 読む本番コードが 1 つも
-- 無かった（この列を含む唯一の SELECT だった ListScheduleSyncsBySite も
-- 呼び出し元ゼロだったため、この issue で併せて落とした）。reconciler の
-- 「自分が作った schedule か」の判定は常に tags = mirakc.IsOurs で行う。
--
-- issue #99 は reservation_id の FK（ON DELETE SET NULL）だけを外す案を
-- 挙げたが、PR #147 のレビューで取り下げられた --- 外すとこの列は「削除済み
-- 予約を指す古い id」を持ちうるようになり、NULL より紛らわしくなる
-- （インシデント対応で直接 SELECT する人を誤らせる）。reservations.id は
-- ruler の導出削除・再実体化で変わる不安定な値（#53/#98/#99）であり、
-- 読み手のいない列にそれを保存し続ける理由が無いため、issue #148 で
-- 列自体を落とした（CLAUDE.md 不変条件 10「意味を持たない行を作らない」/
-- 11「これを書く / 使うコードは今あるか」）。
-- name: UpsertScheduleSync :exec
INSERT INTO schedule_sync (
    site, program_id, state,
    options, tags, failed_reason, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (site, program_id) DO UPDATE SET
    state          = EXCLUDED.state,
    options        = EXCLUDED.options,
    tags           = EXCLUDED.tags,
    failed_reason  = EXCLUDED.failed_reason,
    observed_at    = now();

-- name: DeleteStaleScheduleSyncs :exec
DELETE FROM schedule_sync
WHERE site = $1 AND observed_at < $2;
