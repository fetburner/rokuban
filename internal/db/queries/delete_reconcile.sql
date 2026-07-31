-- 削除 reconcile（M3-8、docs/storage.md §7）。物理 unlink に至る 3 ソース
-- （ごみ箱 / until_encoded / 孤児）を 1 本のループに統一するためのクエリ群。
--
-- 削除プロトコルは冪等: active → deleting → deleted。deleting のまま
-- プロセスが落ちても ListMediaAssetsPendingDelete が次パスで拾い直す。

-- 前パスで deleting にマークしたまま unlink できずに終わった行を拾い直す。
--
-- WHERE は ListTrashMediaAssetsToDelete / ListUntilEncodedOriginalsToDelete と
-- 同じ判定条件をそのまま再掲する（issue #105）。deleting は「再計算できる
-- 決定」であって不可逆な事実ではないため、pending 経路は「既に決めた削除の
-- 再実行だから無条件に信じてよい」とはできない —— ごみ箱からの復元は
-- recordings.deleted_at だけを消す（RestoreRecording）ので、deleting の
-- 間に復元されると media_assets 側は取り残されたまま unlink される
-- （不変条件 9「適用の瞬間」。ruler が toDelete を tx 外で計算していたのと
-- 同型の距離）。ここで判定条件を再評価し、該当しなくなった行は
-- RevertUnqualifiedDeletingAssets が active に戻す。
--
-- ブレーカーとの関係: この再評価は「新しく削除対象を増やす」判断ではない
-- （前パスで一度 active → deleting に遷移させた行の集合を超えて広げることは
-- ない。集合を絞る側にしか働かない）。したがって従来どおりサーキット
-- ブレーカーの対象外のままでよい。
--
-- 罠: until_encoded の原本も pending に乗る。ここを「recordings.deleted_at
-- IS NOT NULL」だけで判定すると、生きている録画の until_encoded 原本が
-- 永久に deleting のまま止まる（ごみ箱条件にも until_encoded 条件にも
-- 該当しない扱いになってしまうため）。両条件を OR で残すこと。
-- name: ListMediaAssetsPendingDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.state = 'deleting'
  AND (
    -- ごみ箱の猶予超過、または「今すぐ完全削除」（ListTrashMediaAssetsToDelete と同条件）
    (
      r.deleted_at IS NOT NULL
      AND (
        (r.purge_after IS NOT NULL AND r.purge_after <= now())
        OR r.deleted_at <= sqlc.arg('grace_cutoff')::timestamptz
      )
    )
    OR
    -- until_encoded の派生物完備（ListUntilEncodedOriginalsToDelete と同条件）
    (
      a.kind = 'original'
      AND r.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
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
    )
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- deleting のまま止まっていたが、上の 2 条件のどちらにも該当しなくなった行を
-- active に戻す（issue #105）。判定条件は ListMediaAssetsPendingDelete の
-- WHERE をそのまま否定したもの —— 適用（この UPDATE 自体）の瞬間に再評価する
-- ことが目的なので、事前に計算した真偽値を受け渡さない。
--
-- 典型例: ごみ箱の猶予超過で deleting にした後、unlink 前に復元
-- （recordings.deleted_at が NULL に戻る）。until_encoded の原本を
-- encode_profiles 変更後に deleting にした後、旧プロファイルの派生物が
-- 揃わなくなるケースも同様に含む。
--
-- ここで active に戻すのは deleting → active の遷移のみで、進行中の unlink
-- とは競合しない: このワーカー自身が media_assets.state の唯一の書き手
-- （InsertOpts の UniqueOpts により同時に 1 パスしか走らない）であり、
-- deleting のまま次パスに持ち越された行は前パスのプロセスが既に終了して
-- いるので、いま unlink が進行中の行と衝突しようがない。復元 API から
-- deleting → active へ即座に戻す案を採らなかったのはこの前提が無い
-- （進行中の unlink と非同期に競合しうる）ため。
-- name: RevertUnqualifiedDeletingAssets :many
UPDATE media_assets a
SET state = 'active', updated_at = now()
FROM recordings r
WHERE a.recording_id = r.id
  AND a.state = 'deleting'
  AND NOT (
    (
      r.deleted_at IS NOT NULL
      AND (
        (r.purge_after IS NOT NULL AND r.purge_after <= now())
        OR r.deleted_at <= sqlc.arg('grace_cutoff')::timestamptz
      )
    )
    OR
    (
      a.kind = 'original'
      AND r.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
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
    )
  )
RETURNING a.id, a.recording_id, a.rel_path;

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
-- name: ListUntilEncodedOriginalsToDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.state = 'active'
  AND a.kind = 'original'
  AND r.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
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
