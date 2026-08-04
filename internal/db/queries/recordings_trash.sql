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
-- purged_at が立っている行（完全削除が完了した tombstone、issue #135）は
-- 対象外 —— WHERE に条件を足して 0 行にし、既存の 404 経路に落とす。
-- ファイルは二度と戻らないので、それをライブラリに戻すと「再生できない
-- 録画」が並んでしまう。
-- name: RestoreRecording :one
UPDATE recordings
SET deleted_at  = NULL,
    purge_after = NULL,
    updated_at  = now()
WHERE id = $1 AND deleted_at IS NOT NULL AND purged_at IS NULL
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

-- ごみ箱一覧。ListRecordings と同じく原本サイズ + drop 合計は載せるが、
-- available_encoded_profiles（再生可能な encoded プロファイル名）は意図的に
-- 射影しない。ごみ箱の録画は配信 3 クエリ（GetOriginalMediaAssetForServing /
-- GetThumbnailMediaAssetForServing / GetEncodedMediaAssetForServing）が
-- deleted_at IS NOT NULL を理由に必ず 404 にするため、フロントはごみ箱では
-- サムネイル・RecordingPlayer・原本リンクを一切出さない（M3-18）。出さない
-- 値をここで揃えても使われないので揃えていない。
-- deleted_at IS NOT NULL のものだけ。deleted_at 降順（最近捨てたものが上）。
--
-- purged_at IS NULL を条件に足す（issue #135）。完全削除が完了した録画
-- （delete_reconcile の MarkPurgedRecordings が purged_at を立てた録画）は
-- tombstone として recordings に残り続けるが、ユーザーに見せるものではない
-- （docs/storage.md §7・§8）。除外の根拠は「アセットが無い」ではなく「purge が
-- 完了した」であることに注意 —— 除外条件を「残っているアセットがある録画
-- だけ」にすると、status='failed' でアセットが 0 行の録画が purge 前から
-- ごみ箱に出なくなってしまう。
-- encode_profiles は issue #159 で recording_encode_policy 衛星表に切り出された
-- ため r.* には含まれない（internal/db/queries/recordings.sql の ListRecordings
-- コメント参照。表示上「未凍結」と「空として凍結」は区別しない）。
-- name: ListTrashRecordings :many
SELECT
    r.*,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
    COALESCE(p.encode_profiles, '{}')::text[] AS encode_profiles
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true
WHERE r.site = $1 AND r.deleted_at IS NOT NULL AND r.purged_at IS NULL
ORDER BY r.deleted_at DESC, r.id DESC;
