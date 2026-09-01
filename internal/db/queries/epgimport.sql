-- rokuban import epgstation（issue #72）専用のクエリ。
-- ルールの冪等キーは rules.metadata の jsonb containment（
-- {"epgstation":{"ruleId": N}}）。rules に EPGStation 固有の列は足さない
-- （不変条件 7: mirakc/EPGStation 固有の概念を永続テーブルの列に持ち込まない。
-- metadata は既存の汎用 jsonb 列を使うだけで新しい形を固定しない）。

-- name: FindRuleByMetadata :one
SELECT * FROM rules WHERE metadata @> sqlc.arg(metadata)::jsonb LIMIT 1;

-- history import（--history-json）専用のルックアップ。
--
-- recordings_unique_active_event（docs/schema/recordings.md）は
-- `WHERE deleted_at IS NULL AND superseded_at IS NULL` の部分索引なので、
-- 生まれた瞬間に tombstone にする history 行の ON CONFLICT ターゲットには
-- ならない（tombstone 済みの行は索引に含まれず、再実行で UpsertInPlaceRecording
-- の INSERT がそのまま新しい行を作ってしまう。二重登録は TestImportHistory_
-- IdempotentRerun で実測して見つけた）。deleted_at を問わず
-- (site, network_id, service_id, event_id) で引き、既存行があれば
-- ImportHistory 側で INSERT せず再利用することで冪等にする。
-- name: FindRecordingByEventAnyState :one
SELECT * FROM recordings
WHERE site = sqlc.arg(site)
  AND network_id = sqlc.arg(network_id)
  AND service_id = sqlc.arg(service_id)
  AND event_id = sqlc.arg(event_id)
  AND superseded_at IS NULL
ORDER BY id DESC
LIMIT 1;
