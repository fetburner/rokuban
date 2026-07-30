-- name: CreateMediaAsset :one
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- ingest の冪等性チェック用。worker/ingest.go の Work は転送を始める前にこれで
-- 「この recording_id の original はもうコミット済みか」を確認する
-- （不変条件 3「コミット = DB 行」。行が無ければまだコミットされていない）。
-- 該当行が無ければ pgx.ErrNoRows を返す。
-- name: GetOriginalMediaAssetID :one
SELECT id FROM media_assets
WHERE recording_id = $1 AND kind = 'original';

-- pid_type は分類できなかった PID では NULL（空文字を入れない）。
-- 値の権威は internal/tsstat（列に CHECK は無い）。
-- name: InsertDropStat :exec
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetRecordingByID :one
SELECT * FROM recordings WHERE id = $1;

-- 配信対象の原本を引く。ごみ箱に入った録画・削除済みアセットは配らない。
-- name: GetOriginalMediaAssetForServing :one
SELECT a.id, a.rel_path, a.size_bytes, a.updated_at, r.title
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'original'
  AND a.state = 'active'
  AND r.deleted_at IS NULL;
