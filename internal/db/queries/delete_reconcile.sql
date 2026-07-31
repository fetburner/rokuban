-- 削除 reconcile（M3-8、docs/storage.md §7）。物理 unlink に至る 3 ソース
-- （ごみ箱 / until_encoded / 孤児）を 1 本のループに統一するためのクエリ群。
--
-- 削除プロトコルは冪等: active → deleting → deleted。deleting のまま
-- プロセスが落ちても ListMediaAssetsPendingDelete が次パスで拾い直す。

-- 前パスで deleting にマークしたまま unlink できずに終わった行を拾い直す。
-- 「既に決めた削除」の再実行であり、新規の判断ではないのでブレーカーの対象外。
-- name: ListMediaAssetsPendingDelete :many
SELECT id, recording_id, rel_path, size_bytes, kind
FROM media_assets
WHERE state = 'deleting'
ORDER BY id
LIMIT sqlc.arg('row_limit');

-- ごみ箱の猶予超過、または「今すぐ完全削除」（purge_after）の対象。
-- name: ListTrashMediaAssetsToDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.state = 'active'
  AND r.deleted_at IS NOT NULL
  AND (
    (r.purge_after IS NOT NULL AND r.purge_after <= now())
    OR r.deleted_at <= sqlc.arg('grace_cutoff')::timestamptz
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- keep_original='until_encoded' で、desired な派生物（全 encode_profiles +
-- thumbnail）がすべて active でコミット済みの原本。ごみ箱経由の録画は
-- ListTrashMediaAssetsToDelete 側で扱うのでここでは除外する。
--
-- cardinality(r.encode_profiles) > 0 は安全弁（issue #103 の「罠」、issue #104 で
-- より広い削除側ガードを別途検討中）。encode_profiles が空だと
-- NOT EXISTS (unnest(...)) が空集合に対して自明に真になり、「全プロファイル
-- 完備」が常に成立してしまう —— 一度も encode していない原本を thumbnail 完備
-- だけで消してしまう経路がここにあった。until_encoded はプロファイル指定が
-- 前提の機能で、API 側もそれを検証している（docs/storage.md §6「エンコード
-- プロファイル未指定のルールでは until_encoded を選択不可」）が、ここでも
-- 前提が崩れたときに削除しない側に倒す。
-- name: ListUntilEncodedOriginalsToDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.state = 'active'
  AND a.kind = 'original'
  AND r.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(r.encode_profiles) > 0
  AND NOT EXISTS (
    SELECT 1 FROM unnest(r.encode_profiles) AS want(profile)
    WHERE NOT EXISTS (
      SELECT 1 FROM media_assets e
      WHERE e.recording_id = r.id
        AND e.kind = 'encoded'
        AND e.state = 'active'
        AND e.profile = want.profile
    )
  )
  AND EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- recording.deleted（M3-11）の発火判定。「録画そのものが消えた」と言えるのは、
-- ごみ箱に入った（deleted_at IS NOT NULL）録画で、物理削除が終わっていない
-- media_assets が 1 行も残っていないときだけである。判定をアセットの kind に
-- 取ると until_encoded の原本削除（録画は生きている）で誤発火し、逆に原本を
-- 先に消した録画のごみ箱削除では発火しない。
-- name: GetRecordingPurgeState :one
SELECT
  r.site,
  r.title,
  -- 明示キャストが無いと sqlc が型を推論できず interface{} になる。
  (r.deleted_at IS NOT NULL)::boolean AS trashed,
  EXISTS (
    SELECT 1 FROM media_assets a
    WHERE a.recording_id = r.id AND a.state <> 'deleted'
  ) AS assets_remaining
FROM recordings r
WHERE r.id = $1;

-- name: MarkMediaAssetDeleting :execrows
UPDATE media_assets SET state = 'deleting', updated_at = now()
WHERE id = $1 AND state = 'active';

-- name: MarkMediaAssetDeleted :execrows
UPDATE media_assets SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE id = $1 AND state = 'deleting';

-- 孤児判定用。テーブル全体を読んで Go 側でファイル一覧と突き合わせる
-- （家庭用サーバー規模の行数を前提。件数が増えたら bloom filter 等の検討対象）。
-- name: ListAllMediaAssetRelPaths :many
SELECT rel_path FROM media_assets;

-- 孤児候補を記録する。既存行があれば first_seen を保持する
-- （DO NOTHING。エイジングの起点は「最初に孤児だと気づいた時刻」）。
-- name: UpsertOrphanFile :exec
INSERT INTO orphan_files (rel_path) VALUES ($1)
ON CONFLICT (rel_path) DO NOTHING;

-- name: DeleteOrphanFile :exec
DELETE FROM orphan_files WHERE rel_path = $1;

-- name: ListAllOrphanFiles :many
SELECT rel_path, first_seen FROM orphan_files;

-- エイジング済みの孤児（first_seen が age_cutoff 以前）。
-- name: ListAgedOrphanFiles :many
SELECT rel_path FROM orphan_files WHERE first_seen <= sqlc.arg('age_cutoff')::timestamptz;
