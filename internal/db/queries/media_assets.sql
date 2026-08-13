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

-- ingest の宛先事前チェック用（issue #197）。os.Create で宛先ファイルを開く前に、
-- 別の active な media_asset が同じ rel_path を既に使っていないかを確認する。
-- **正しさの根拠ではない**（先読みと実際の INSERT の間に別ジョブが commit
-- しうる TOCTOU の窓がある）。正しさの根拠は
-- CREATE UNIQUE INDEX ON media_assets (rel_path) WHERE state <> 'deleted'
-- （00002_schema_v1.sql）であり、ここが競合を見逃してもそちらが 23505 で守る。
-- ここでの WHERE state <> 'deleted' はその一意索引の述語と同じにする ---
-- 削除済みの行が使っていた rel_path は正当に再利用できるので、削除済み行と
-- 衝突させてはいけない。該当行が無ければ pgx.ErrNoRows を返す。
-- name: GetActiveMediaAssetByRelPath :one
SELECT id, recording_id FROM media_assets
WHERE rel_path = $1 AND state <> 'deleted';

-- encode / thumbnail が読む原本(パスとサイズ)。active のみ。tombstone や未 commit は対象外。
-- name: GetActiveOriginalMediaAsset :one
SELECT id, rel_path, size_bytes
FROM media_assets
WHERE recording_id = $1 AND kind = 'original' AND state = 'active';

-- encode の冪等性チェック。active な encoded が既にあれば ffmpeg を走らせない。
-- name: GetActiveEncodedMediaAssetID :one
SELECT id FROM media_assets
WHERE recording_id = $1
  AND kind = 'encoded'
  AND profile = $2
  AND state = 'active';

-- encode コミット。UNIQUE (recording_id, kind, profile) で冪等。
-- tombstone（state='deleted'）がある場合は active に戻してパスとサイズを更新する。
-- 既に active な行がある場合の上書きは worker 側の事前チェックで避ける。
-- name: UpsertEncodedMediaAsset :one
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
VALUES ($1, 'encoded', $2, $3, $4)
ON CONFLICT (recording_id, kind, profile) DO UPDATE SET
    rel_path   = EXCLUDED.rel_path,
    size_bytes = EXCLUDED.size_bytes,
    state      = 'active',
    deleted_at = NULL,
    updated_at = now()
RETURNING id;

-- thumbnail コミット。UNIQUE (recording_id, kind, profile) で冪等。
-- tombstone（state='deleted'、過去の完全削除の残骸）がある場合は active に戻して
-- パスとサイズを更新する（UpsertEncodedMediaAsset と同じ形。issue #108）。
-- ON CONFLICT DO NOTHING（id を返さず pgx.ErrNoRows で競合を伝える形）のままだと、
-- tombstone との競合も新規コミット後の競合も同じ ErrNoRows で返ってきて区別できず、
-- 呼び出し側が両方を「既にコミット済みで成功」に丸めてしまう。tombstone は
-- ファイルがメディア上に書かれ続ける一方で GetActiveThumbnailMediaAssetID が
-- 空を返し続け、レベルトリガーが同じジョブを積み直す孤児を生む。
-- rel_path は thumbnails/{recording_id}.jpg で recording_id から決定的に導出され、
-- ON CONFLICT のキー (recording_id, kind, profile) と 1 対 1 対応する
-- （他の recording_id の行が同じ rel_path を持つことはない）。そのため
-- tombstone を active に戻しても CREATE UNIQUE INDEX ON media_assets (rel_path)
-- WHERE state <> 'deleted' に別の生きた行が衝突すること（23505）はない。
-- 既に active な行がある場合の上書きは worker 側の事前チェック
-- （GetActiveThumbnailMediaAssetID）で避ける。
-- name: UpsertThumbnailMediaAsset :one
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'thumbnail', $2, $3)
ON CONFLICT (recording_id, kind, profile) DO UPDATE SET
    rel_path   = EXCLUDED.rel_path,
    size_bytes = EXCLUDED.size_bytes,
    state      = 'active',
    deleted_at = NULL,
    updated_at = now()
RETURNING id;

-- thumbnail の冪等性チェック用。active な thumbnail 行があれば id を返す。
-- name: GetActiveThumbnailMediaAssetID :one
SELECT id FROM media_assets
WHERE recording_id = $1
  AND kind = 'thumbnail'
  AND state = 'active';

-- レベルトリガー投入: original があり active thumbnail が無い recording_id。
-- thumbnail ジョブの desired − observed ギャップを埋める（issue #66）。
-- ごみ箱（recordings.deleted_at IS NOT NULL）は除外する（issue #109）:
-- 生成しても配信側（GetThumbnailMediaAssetForServing）が r.deleted_at IS NULL を
-- 要求するので誰にも配られず、猶予明けの削除 reconcile が消すだけの ffmpeg 無駄打ちになる。
-- name: ListRecordingIDsMissingThumbnail :many
SELECT o.recording_id
FROM media_assets o
JOIN recordings r ON r.id = o.recording_id
WHERE o.kind = 'original'
  AND o.state = 'active'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = o.recording_id
      AND t.kind = 'thumbnail'
      AND t.state = 'active'
  );

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

-- 配信対象のサムネイルを引く。ごみ箱・削除済みは配らない（原本と同じ契約）。
-- name: GetThumbnailMediaAssetForServing :one
SELECT a.id, a.rel_path, a.size_bytes, a.updated_at, r.title
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'thumbnail'
  AND a.state = 'active'
  AND r.deleted_at IS NULL;

-- 配信対象の encoded 派生物を引く（?profile= 付き）。原本と同じ配信規律。
-- name: GetEncodedMediaAssetForServing :one
SELECT a.id, a.rel_path, a.size_bytes, a.updated_at, r.title, a.profile
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'encoded'
  AND a.profile = $2
  AND a.state = 'active'
  AND r.deleted_at IS NULL;
