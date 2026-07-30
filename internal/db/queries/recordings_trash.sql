-- ごみ箱（論理削除 / 復元 / 即時 purge 印）。M3-7 / issue #69。
-- 物理 unlink はしない（M3-8）。api ロールは DB だけ触る。

-- 論理削除。既に deleted_at が立っていても COALESCE で据え置き（冪等）。
-- 行が無ければ 0 行（:one なので pgx.ErrNoRows → API が 404）。
-- name: SoftDeleteRecording :one
UPDATE recordings
SET deleted_at = COALESCE(deleted_at, now()),
    updated_at = now()
WHERE id = $1
RETURNING id, deleted_at;

-- 復元。ごみ箱に入っている行だけを対象にする。
-- deleted_at と purge_after の両方を消す（即時 purge 印も取り消す）。
-- 同一イベントに生きている録画がある場合は unique partial index で 23505。
-- name: RestoreRecording :one
UPDATE recordings
SET deleted_at  = NULL,
    purge_after = NULL,
    updated_at  = now()
WHERE id = $1 AND deleted_at IS NOT NULL
RETURNING id;

-- 即時物理削除の要求。ファイルは消さない。
-- purge は soft-delete も兼ねる（まだごみ箱に入っていなければ deleted_at を立てる）。
-- 既に purge_after が立っていても now() で上書き（冪等に再要求できる）。
-- name: MarkRecordingPurgeAfter :one
UPDATE recordings
SET deleted_at  = COALESCE(deleted_at, now()),
    purge_after = now(),
    updated_at  = now()
WHERE id = $1
RETURNING id, deleted_at, purge_after;

-- ごみ箱一覧。通常 ListRecordings と同じ射影（原本サイズ + drop 合計）で、
-- deleted_at IS NOT NULL のものだけ。deleted_at 降順（最近捨てたものが上）。
-- name: ListTrashRecordings :many
SELECT
    r.*,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true
WHERE r.site = $1 AND r.deleted_at IS NOT NULL
ORDER BY r.deleted_at DESC, r.id DESC;
