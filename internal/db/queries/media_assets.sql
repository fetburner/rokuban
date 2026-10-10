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

-- ingest の転送前ヒント用（issue #197）。worker/ingest.go の Work が
-- record 固有の一時ファイルを開く前に、別のまだ削除されていない
-- （state <> 'deleted'。'active' に限らず、delete_reconcile の unlink 前後の
-- 中間状態である 'deleting' も含む）media_asset が同じ rel_path を既に使って
-- いないかを確認する。名前を
-- "Active" ではなく "Live" にしているのは、他の Get*Active*MediaAsset* 系
-- クエリ（state = 'active' を厳密に見る）と述語が違うことを名前からも
-- 分かるようにするため（PR #267 のレビュー指摘: "active" という語だと
-- 'deleting' 行にも発火する事実とずれる）。
--
-- **ingest 対 ingest の決着はこの SELECT ではない**（issue #731）。複数の
-- 試行が一時ファイルへ並行転送でき、commit 内の media_assets INSERT と
-- 部分一意索引が採用を一つに決める。ここで拾うのは転送を始める価値が無い
-- 「別の recording が既にコミットした」という恒久的な衝突である。
-- delete_reconcile は canonical orphan の unlink 前に ingest commit と同じ
-- rel_path transaction-level advisory lock を取得するため、公開・回収の確定区間は
-- この SELECT と独立に直列化される。ただしこの関数自体は転送前の安価なヒントで、
-- ingest 同士の決着は commit 内の lock と media_assets の一意索引に任せる。
-- ここを一意性の最終判定に使わない。WHERE state <> 'deleted' はその一意索引の
-- 述語と同じにする --- 削除済みの行が使っていた rel_path は正当に再利用できるので、
-- 削除済み行と衝突させてはいけない。呼び出し側は recording_id しか使わないので
-- id は選択しない。該当行が無ければ pgx.ErrNoRows を返す。
-- name: GetLiveMediaAssetByRelPath :one
SELECT recording_id FROM media_assets
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

-- 置き換え（cut プロファイルの作り直し）で、旧パスの unlink と世代番号の導出に
-- 要る行。**state を問わない**: tombstone（state='deleted'）の rel_path からも
-- 世代番号を読む（次のカット版が同じパスを再利用しないため）。UNIQUE
-- (recording_id, kind, profile) があるので高々 1 行。
-- name: GetEncodedMediaAssetForProfile :one
SELECT id, rel_path, state
FROM media_assets
WHERE recording_id = $1
  AND kind = 'encoded'
  AND profile = $2;

-- 置き換えの同一 tx 内でパスとサイズを差し替える。**行は消さない** --- 消して
-- 作り直すと rel_path の部分一意索引から一瞬外れ、その隙間に別の行が同じパスを
-- 取れてしまう。世代番号で必ず新しいパスになるので、UPDATE で足りる。
-- name: UpdateEncodedMediaAssetPath :exec
UPDATE media_assets
SET rel_path   = sqlc.arg('rel_path'),
    size_bytes = sqlc.arg('size_bytes'),
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

-- name: UpsertSeekTilesMediaAsset :one
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'seek_tiles', $2, $3)
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

-- thumbnail 差し替えの commit tx 内で行を直列化し、世代付きパスへ UPDATE する。
-- seek_ms は衛星表から読む。行が無ければ旧サムネイルで位置不明。
-- name: LockActiveThumbnailMediaAsset :one
SELECT a.id, a.rel_path, s.seek_ms
FROM media_assets a
LEFT JOIN media_asset_thumbnail_seeks s ON s.media_asset_id = a.id
WHERE a.recording_id = sqlc.arg('recording_id')
  AND a.kind = 'thumbnail'
  AND a.state = 'active'
FOR UPDATE OF a;

-- thumbnail 差し替え時に、同じ media_asset 行の相対パスとサイズだけを更新する。
-- name: UpdateThumbnailMediaAssetPath :exec
UPDATE media_assets
SET rel_path   = sqlc.arg('rel_path'),
    size_bytes = sqlc.arg('size_bytes'),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND kind = 'thumbnail'
  AND state = 'active';

-- 生成に使ったファイルがまだ使えるかを commit tx 内で確認する。
-- name: IsActiveThumbnailInput :one
SELECT EXISTS (
    SELECT 1
    FROM media_assets a
    WHERE a.id = sqlc.arg('media_asset_id')
      AND a.kind IN ('original', 'encoded')
      AND a.state = 'active'
      AND NOT EXISTS (
          SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = a.id
      )
);

-- サムネイル作成時に抽出した原本時間軸の位置を、media_assets の公開と同じ tx で記録。
-- name: UpsertMediaAssetThumbnailSeek :exec
INSERT INTO media_asset_thumbnail_seeks (media_asset_id, seek_ms)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('seek_ms'))
ON CONFLICT (media_asset_id) DO UPDATE SET seek_ms = EXCLUDED.seek_ms;

-- seek_tiles の冪等性チェック用。active な seek_tiles 行があれば id を返す。
-- name: GetActiveSeekTilesMediaAssetID :one
SELECT id FROM media_assets
WHERE recording_id = $1
  AND kind = 'seek_tiles'
  AND state = 'active';

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
SELECT a.id, a.rel_path, a.size_bytes, a.updated_at, r.title
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'original'
  AND a.state = 'active'
  AND r.deleted_at IS NULL;

-- 原本 MPEG-2 を HLS に変換する対象を引く。録画完了済み・ごみ箱/ purge 前・
-- active original のみを開始可能とし、site は URL の site-scoped streamer と照合する。
-- 既存セッションの再取得では使わず、セッション開始前と rel_path lock 取得後に呼ぶ。
-- name: GetOriginalVODTarget :one
SELECT a.id, a.rel_path, a.size_bytes, r.site
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'original'
  AND a.state = 'active'
  AND r.site = $2
  AND r.status = 'finished'
  AND r.deleted_at IS NULL
  AND r.superseded_at IS NULL
  AND r.purged_at IS NULL;

-- 配信対象のサムネイルを引く。ごみ箱・削除済みは配らない（原本と同じ契約）。
-- name: GetThumbnailMediaAssetForServing :one
SELECT a.id, a.rel_path, a.size_bytes, a.updated_at, r.title
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'thumbnail'
  AND a.state = 'active'
  AND r.deleted_at IS NULL;

-- 配信対象のシークタイルを引く。ごみ箱・削除済みは配らない。
-- name: GetSeekTilesMediaAssetForServing :one
SELECT a.id, a.rel_path, a.size_bytes, a.updated_at, r.title
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = $1
  AND a.kind = 'seek_tiles'
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
