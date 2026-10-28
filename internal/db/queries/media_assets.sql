-- name: CreateMediaAsset :one
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES ($1, $2, $3, $4, CASE WHEN $2 = 'original' THEN NULL ELSE 0 END)
RETURNING id;

-- 原本の世代は、原本の INSERT と進捗行の削除を同じ tx に置くため、
-- 転送中の recording_ingest_progress からコピーする。
-- name: CreateOriginalMediaAsset :one
INSERT INTO media_assets (recording_id, kind, size_bytes, ingest_generation)
SELECT p.recording_id, 'original', sqlc.arg('size_bytes'), p.temp_generation
FROM recording_ingest_progress p
WHERE p.recording_id = sqlc.arg('recording_id')
RETURNING id;

-- ingest の冪等性チェック用。worker/ingest.go の Work は転送を始める前にこれで
-- 「この recording_id の original はもうコミット済みか」を確認する
-- （不変条件 3「コミット = DB 行」。行が無ければまだコミットされていない）。
-- 該当行が無ければ pgx.ErrNoRows を返す。
-- name: GetOriginalMediaAssetID :one
SELECT media_assets.id FROM media_assets
WHERE recording_id = $1 AND kind = 'original';

-- ingest の転送前ヒント用（issue #197）。worker/ingest.go の Work が
-- record 固有の一時ファイルを開く前に、別のまだ削除されていない
-- （state <> 'deleted'。'active' に限らず、delete_reconcile の unlink 前後の
-- 中間状態である 'deleting' も含む）コピーが同じ置き場の rel_path を既に
-- 使っていないかを確認する。名前を "Active" ではなく "Live" にしているのは、
-- 他の Get*Active*MediaAsset* 系クエリ（state = 'active' を厳密に見る）と
-- 述語が違うことを名前からも分かるようにするため（PR #267 のレビュー指摘:
-- "active" という語だと 'deleting' 行にも発火する事実とずれる）。
--
-- **ingest 対 ingest の決着はこの SELECT ではない**（issue #731）。複数の
-- 試行が一時ファイルへ並行転送でき、commit 内の media_asset_copies INSERT と
-- 部分一意索引が採用を一つに決める。ここで拾うのは転送を始める価値が無い
-- 「別の recording が既にコミットした」という恒久的な衝突である。
-- 通常削除は ingest commit と同じ rel_path filesystem lock を取得するため、
-- 公開・削除の確定区間はこの SELECT と独立に直列化される。canonical orphan は
-- 自動で unlink しない。ただしこの関数自体は転送前の安価なヒントで、
-- ingest 同士の決着は commit 内の lock と media_asset_copies の一意索引に任せる。
-- ここを一意性の最終判定に使わない。WHERE state <> 'deleted' はその一意索引の
-- 述語と同じにする --- 削除済みコピーが使っていた rel_path は正当に再利用できるので、
-- 削除済み行と衝突させてはいけない。呼び出し側は recording_id しか使わないので
-- id は選択しない。該当行が無ければ pgx.ErrNoRows を返す。
-- name: GetLiveMediaAssetByRelPath :one
SELECT a.recording_id
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.rel_path = sqlc.arg('rel_path')
  AND c.state <> 'deleted'
  AND (sqlc.narg('store_name')::text IS NULL OR c.store = sqlc.narg('store_name'));

-- encode / thumbnail が読む原本（パスとサイズ）。active コピーのみ。tombstone や未 commit は対象外。
-- name: GetActiveOriginalMediaAsset :one
SELECT a.id, c.rel_path, a.size_bytes, c.store
FROM media_assets a
JOIN LATERAL (
    SELECT copy.rel_path, copy.store
    FROM preferred_active_media_asset_copy(a.id, sqlc.narg('store_name')::text) copy
) c ON true
WHERE a.recording_id = sqlc.arg('recording_id')
  AND a.kind = 'original';

-- encode の冪等性チェック。active な encoded が既にあれば ffmpeg を走らせない。
-- name: GetActiveEncodedMediaAssetID :one
SELECT media_assets.id FROM media_assets
WHERE recording_id = $1
  AND kind = 'encoded'
  AND profile = $2
  AND EXISTS (SELECT 1 FROM active_media_asset_copies(media_assets.id, sqlc.narg('store_name')::text));

-- publish が tombstone も含めて既存の行を見るため、active に限らない。置き場を限定しない読み手に
-- 備え、active を先に、同順位は store 順に 1 件だけ選ぶ。
-- name: GetEncodedMediaAssetForProfile :one
SELECT a.id, c.rel_path, c.state, c.store,
       COALESCE(a.derived_generation, 0)::bigint AS derived_generation
FROM media_assets a
JOIN LATERAL (
    SELECT copy.rel_path, copy.state, copy.store
    FROM media_asset_copies copy
    WHERE copy.media_asset_id = a.id
      AND (sqlc.narg('store_name')::text IS NULL OR copy.store = sqlc.narg('store_name'))
    ORDER BY (copy.state = 'active') DESC, copy.store
    LIMIT 1
) c ON true
WHERE a.recording_id = sqlc.arg('recording_id')
  AND a.kind = 'encoded'
  AND a.profile = sqlc.arg('profile');

-- GetDerivedMediaAssetGeneration returns the current counter, including tombstones.
-- Only kinds without a profile (thumbnail, seek_tiles) have a single row.
-- name: GetDerivedMediaAssetGeneration :one
SELECT COALESCE(derived_generation, 0)::bigint AS derived_generation
FROM media_assets
WHERE recording_id = sqlc.arg('recording_id')
  AND kind = sqlc.arg('kind')
  AND profile IS NULL;

-- 置き換えの同一 tx 内で論理資産のサイズと世代を更新する。物理パスはコピー表に
-- 書く。**行は消さない** --- 世代番号で必ず新しいパスになるので UPDATE で足りる。
-- name: UpdateEncodedMediaAssetPath :exec
UPDATE media_assets
SET size_bytes = sqlc.arg('size_bytes'),
    derived_generation = sqlc.arg('derived_generation'),
    updated_at = now()
WHERE id = sqlc.arg('id');

-- 凍結した区間の差し替え（DELETE → InsertMediaAssetCuts の順に同一 tx で呼ぶ）。
-- name: DeleteMediaAssetCuts :exec
DELETE FROM media_asset_cuts WHERE media_asset_id = sqlc.arg('media_asset_id');

-- keep_ranges は ms の半開区間の列。境界はフレーム境界へ量子化済みで、昇順・
-- 非交差・非隣接に正規化して渡す（値どうしの一致比較をするため）。`isempty` を
-- CHECK が拒否するので、空の multirange は渡さない（呼び出し側が先に落とす）。
-- name: InsertMediaAssetCuts :exec
INSERT INTO media_asset_cuts (media_asset_id, keep_ranges)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('keep_ranges')::int8multirange);

-- 凍結した区間（ms の半開区間）。行が無ければ空配列 --- COALESCE が要る
-- （集約は行が無いと NULL になる。GetRecordingCMRangesJSON と同じ形）。
-- name: GetMediaAssetKeepRangesJSON :one
SELECT COALESCE(
    jsonb_agg(
        jsonb_build_object('startMs', lower(k), 'endMs', upper(k))
        ORDER BY lower(k)
    ),
    '[]'::jsonb
)::jsonb AS ranges
FROM media_asset_cuts c
CROSS JOIN LATERAL unnest(c.keep_ranges) AS k
WHERE c.media_asset_id = sqlc.arg('media_asset_id');

-- encode コミット。UNIQUE (recording_id, kind, profile) で冪等。
-- 既存行がある場合はサイズと世代を更新する。コピーの状態とパスはコピー表へ書く。
-- 既に active な行がある場合の上書きは worker 側の事前チェックで避ける。
-- name: UpsertEncodedMediaAsset :one
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES ($1, 'encoded', $2, $3, $4)
ON CONFLICT (recording_id, kind, profile) DO UPDATE SET
    size_bytes = EXCLUDED.size_bytes,
    derived_generation = EXCLUDED.derived_generation,
    sha256     = NULL,
    updated_at = now()
RETURNING id;

-- thumbnail コミット。UNIQUE (recording_id, kind, profile) で冪等。
-- 過去の完全削除の残骸があればサイズと世代を更新する。コピーの状態とパスはコピー表へ書く。
-- ON CONFLICT DO NOTHING（id を返さず pgx.ErrNoRows で競合を伝える形）のままだと、
-- tombstone との競合も新規コミット後の競合も同じ ErrNoRows で返ってきて区別できず、
-- 呼び出し側が両方を「既にコミット済みで成功」に丸めてしまう。tombstone は
-- ファイルがメディア上に書かれ続ける一方で GetActiveThumbnailMediaAssetID が
-- 空を返し続け、レベルトリガーが同じジョブを積み直す孤児を生む。
-- 既に active な行がある場合の上書きは worker 側の事前チェック
-- （GetActiveThumbnailMediaAssetID）で避ける。
-- name: UpsertThumbnailMediaAsset :one
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'thumbnail', $2, $3)
ON CONFLICT (recording_id, kind, profile) DO UPDATE SET
    size_bytes = EXCLUDED.size_bytes,
    derived_generation = EXCLUDED.derived_generation,
    sha256     = NULL,
    updated_at = now()
RETURNING id;

-- name: UpsertSeekTilesMediaAsset :one
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'seek_tiles', $2, $3)
ON CONFLICT (recording_id, kind, profile) DO UPDATE SET
    size_bytes = EXCLUDED.size_bytes,
    derived_generation = EXCLUDED.derived_generation,
    sha256     = NULL,
    updated_at = now()
RETURNING id;

-- thumbnail の冪等性チェック用。active な thumbnail 行があれば id を返す。
-- name: GetActiveThumbnailMediaAssetID :one
SELECT media_assets.id FROM media_assets
WHERE recording_id = $1
  AND kind = 'thumbnail'
  AND EXISTS (SELECT 1 FROM active_media_asset_copies(media_assets.id, sqlc.narg('store_name')::text));

-- thumbnail 差し替えの commit tx 内で行を直列化し、世代付きパスへ UPDATE する。
-- seek_ms は衛星表から読む。行が無ければ旧サムネイルで位置不明。
-- name: LockActiveThumbnailMediaAsset :one
SELECT a.id, c.rel_path, s.seek_ms, COALESCE(a.derived_generation, 0)::bigint AS derived_generation
FROM media_assets a
JOIN media_asset_copies c
  ON c.media_asset_id = a.id
 AND c.store = sqlc.arg('store_name')
 AND c.state = 'active'
LEFT JOIN media_asset_thumbnail_seeks s ON s.media_asset_id = a.id
WHERE a.recording_id = sqlc.arg('recording_id')
  AND a.kind = 'thumbnail'
FOR UPDATE OF a, c;

-- thumbnail 差し替え時に、同じ論理資産のサイズだけを更新する。
-- name: UpdateThumbnailMediaAssetPath :exec
UPDATE media_assets AS asset
SET size_bytes = sqlc.arg('size_bytes'),
    derived_generation = sqlc.arg('derived_generation'),
    sha256     = NULL,
    updated_at = now()
WHERE asset.id = sqlc.arg('id')
  AND asset.kind = 'thumbnail'
  AND EXISTS (
      SELECT 1 FROM media_asset_copies c
      WHERE c.media_asset_id = asset.id
        AND c.store = sqlc.arg('store_name')
        AND c.state = 'active'
  );

-- 生成に使ったファイルがまだ使えるかを commit tx 内で確認する。
-- name: IsActiveThumbnailInput :one
SELECT EXISTS (
    SELECT 1
    FROM media_assets a
    WHERE a.id = sqlc.arg('media_asset_id')
      AND a.kind IN ('original', 'encoded')
      AND EXISTS (
          SELECT 1 FROM media_asset_copies c
          WHERE c.media_asset_id = a.id
            AND c.store = sqlc.arg('store_name')
            AND c.state = 'active'
      )
      AND NOT EXISTS (
          SELECT 1 FROM missing_media_assets m
          WHERE m.media_asset_id = a.id AND m.store = sqlc.arg('store_name')
      )
);

-- サムネイル作成時に抽出した原本時間軸の位置を、media_assets の公開と同じ tx で記録。
-- name: UpsertMediaAssetThumbnailSeek :exec
INSERT INTO media_asset_thumbnail_seeks (media_asset_id, seek_ms)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('seek_ms'))
ON CONFLICT (media_asset_id) DO UPDATE SET seek_ms = EXCLUDED.seek_ms;

-- seek_tiles の冪等性チェック用。active な seek_tiles 行があれば id を返す。
-- name: GetActiveSeekTilesMediaAssetID :one
SELECT media_assets.id FROM media_assets
WHERE recording_id = $1
  AND kind = 'seek_tiles'
  AND EXISTS (SELECT 1 FROM active_media_asset_copies(media_assets.id, sqlc.narg('store_name')::text));

-- pid_type は分類できなかった PID では NULL（空文字を入れない）。
-- 値の権威は internal/tsstat（列に CHECK は無い）。
-- name: InsertDropStat :batchexec
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- byte_offset は原本内の観測位置。elapsed_ms は PCR を観測できなかった位置では
-- NULL のまま保存する（導出できないこと自体を値で表すために 0 を使わない）。
-- name: InsertDropPosition :batchexec
INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms)
VALUES ($1, $2, $3, $4);

-- name: GetRecordingByID :one
SELECT * FROM recordings WHERE id = $1;

-- name: GetRecordingEncodeTimes :one
SELECT started_at, ended_at, program_duration_ms FROM recordings WHERE id = $1;

-- 配信対象の原本を引く。ごみ箱に入った録画・削除済みアセットは配らない。
-- name: GetOriginalMediaAssetForServing :one
SELECT a.id, c.rel_path, a.size_bytes, a.updated_at, r.title, c.store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN LATERAL (
    SELECT copy.rel_path, copy.store
    FROM preferred_active_media_asset_copy(a.id, sqlc.narg('store_name')::text) copy
) c ON true
WHERE a.recording_id = $1
  AND a.kind = 'original'
  AND r.deleted_at IS NULL;

-- 配信対象のサムネイルを引く。ごみ箱・削除済みは配らない（原本と同じ契約）。
-- name: GetThumbnailMediaAssetForServing :one
SELECT asset.id AS asset_id, copy.rel_path,
       asset.size_bytes, asset.updated_at, recording.title, copy.store
FROM media_assets asset
JOIN recordings recording ON recording.id = asset.recording_id
JOIN LATERAL (
    SELECT c.rel_path, c.store
    FROM preferred_active_media_asset_copy(asset.id, sqlc.narg('store_name')::text) c
) copy ON true
WHERE asset.recording_id = $1
  AND asset.kind = 'thumbnail'
  AND recording.deleted_at IS NULL;

-- 配信対象のシークタイルを引く。ごみ箱・削除済みは配らない。
-- name: GetSeekTilesMediaAssetForServing :one
SELECT a.id, c.rel_path, a.size_bytes, a.updated_at, r.title, c.store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN LATERAL (
    SELECT copy.rel_path, copy.store
    FROM preferred_active_media_asset_copy(a.id, sqlc.narg('store_name')::text) copy
) c ON true
WHERE a.recording_id = $1
  AND a.kind = 'seek_tiles'
  AND r.deleted_at IS NULL;

-- 配信対象の encoded 派生物を引く（?profile= 付き）。原本と同じ配信規律。
-- name: GetEncodedMediaAssetForServing :one
SELECT a.id, c.rel_path, a.size_bytes, a.updated_at, r.title, a.profile, c.store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN LATERAL (
    SELECT copy.rel_path, copy.store
    FROM preferred_active_media_asset_copy(a.id, sqlc.narg('store_name')::text) copy
) c ON true
WHERE a.recording_id = $1
  AND a.kind = 'encoded'
  AND a.profile = $2
  AND r.deleted_at IS NULL;
