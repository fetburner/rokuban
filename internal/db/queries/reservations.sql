-- name: GetReservation :one
SELECT id, rule_id FROM reservations
WHERE id = $1;

-- name: GetReservationBySiteAndProgramID :one
SELECT id, rule_id FROM reservations
WHERE site = $1 AND program_id = $2;

-- name: CreateManualReservation :one
-- テスト用の直接 INSERT ヘルパー。reservations の実運用上の唯一の書き手は
-- ruler の一括 INSERT（internal/ruler/sql.go）で、api はこの表に一切書かない
-- （M3-1、issue #29「導出器が作るキーを宛先にしない」の帰結）。
--
-- 番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）は
-- program_snapshots に抽出された（#27）。FK (site, program_id) REFERENCES
-- program_snapshots があるので、呼び出し側（テストの fixture）はこの
-- INSERT より先に program_snapshots の行を upsert しておくこと。
--
-- reservations.source は持たない（issue #26 で削除）。この予約が「手動」で
-- あることは、program_intents.action='record' の行がそのまま表す。
INSERT INTO reservations (site, program_id)
VALUES ($1, $2)
RETURNING *;

-- 予約とユーザー意図・上書き・番組スナップショットを 1 行に合わせて返す。
-- action は program_intents、overrides は program_overrides（M2-4 / 00010 で分離）
-- にあり、予約が存在しても意図・上書きのどちらかしかない（あるいはどちらも
-- 無い）ことがあるので両方 LEFT JOIN する。番組スナップショットは FK が
-- あるので必ず存在する（INNER JOIN）。
-- name: GetReservationFull :one
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.id = $1;

-- name: ListReservationsBySite :many
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
ORDER BY s.start_at;

-- 同期対象の「候補」を返すクエリ（issue #54）。ここで絞っているのは
-- orphaned_at IS NULL だけで、それ以上のことはしていない（#28/#30 で
-- state <> 'orphaned' から置き換え）。orphaned だけを除外してよい理由は
-- docs/schema.md §3「state を『mirakc への同期対象か』のフィルタに使っては
-- ならない」、docs/recording.md §4.3: 番組が終了しているので schedule を
-- 作る意味がない。active/detached はどちらも「実質 manual として動く」ことが
-- あるため候補に含める。旧名 ListActiveReservationsBySite は state='active' で
-- しか絞っておらず detached の予約に schedule が作られないバグの原因だった
-- （M2-4 で修正）。
--
-- **「同期対象か」を最終的に決めるのは effective.skip（base + overrides +
-- program_intents.action の合成）であり、この行だけでは絞り切れていない。**
-- 絞り込みは呼び出し元が db.EvaluateSyncCandidates（internal/db/sync.go）に
-- 通して行う。旧名 ListSyncableReservationsBySite は「もう絞ってある」と
-- 約束してしまっていた。その約束を信じて shadow-diff（cmd/rokuban/shadowdiff.go）
-- の書き手は effective.skip の絞り込みを移植し忘れ、M2-6 の重複排除が
-- base.skip=true を立てた予約を「EPGStation と一致（Both）」と誤報告する
-- 見逃しが M2 の出口基準の測定器に入り込んだ（issue #54）。同じ間違いを
-- 繰り返さないため、クエリ名には「候補」であることだけを約束させる。
--
-- 番組の開始時刻・尺（reconciler の開始遅延検出・orphaned 化判定に使う）は
-- program_snapshots に移設された（#27）ので JOIN する。FK があるので必ず存在する。
-- name: ListReservationsForSyncEvaluation :many
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1 AND r.orphaned_at IS NULL
ORDER BY s.start_at;

-- 番組終了後に schedule が観測されなかった予約を orphaned にする
-- （reconciler.markOrphaned から呼ばれる）。かつては state = 'active' でしか
-- 絞らず detached の予約が対象から漏れるバグがあった（#30 症状 2）。
-- orphaned_at は不可逆な観測で、書き手はここ（reconciler）だけ --- ruler は
-- 一切上書きしない（CLAUDE.md 不変条件 9）。除外してよいのは既に orphaned な
-- 行への再更新だけ（updated_at を無駄に進めない）なので active/detached の
-- 両方（= orphaned_at IS NULL の行すべて）を対象にする。
-- :execrows にしてあるのは、呼び出し側（reconciler.markOrphaned）が「実際に
-- 更新できたか」をログ出力の可否に使うため（0 行のときに「marked orphaned」と
-- ログに出ると実態と食い違う）。
-- name: MarkReservationOrphaned :execrows
UPDATE reservations
SET orphaned_at = now(), updated_at = now()
WHERE id = $1 AND orphaned_at IS NULL;

-- 番組終了後の GC は DeleteEndedProgramSnapshots（internal/db/queries/program_snapshots.sql）
-- 1 本に集約された（#27）。reservations は program_snapshots への FK が
-- ON DELETE CASCADE なので、program_snapshots 側の行が消えれば一緒に落ちる
-- （orphaned かどうかを問わない。recordings.reservation_id は ON DELETE SET NULL
-- なので、録画履歴（recordings/media_assets）はこの削除の影響を受けない）。
-- 個別の DeleteEndedReservations は撤去した。
