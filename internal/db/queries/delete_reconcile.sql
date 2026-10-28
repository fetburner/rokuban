-- 削除 reconcile（M3-8、docs/storage.md §7）。物理 unlink に至る 3 ソース
-- （ごみ箱 / until_encoded / 孤児）を 1 本のループに統一するためのクエリ群。
--
-- 削除プロトコルは冪等: コピーの active → deleting → deleted。deleting のまま
-- プロセスが落ちても ListMediaAssetsPendingDelete が次パスで拾い直す。
--
-- 「このアセットは消してよいか」の 3 つの腕（ごみ箱の猶予超過 or 今すぐ
-- purge / until_encoded の派生物完備 / ユーザーが外したエンコード版）は、
-- いずれもスキーマ側の名前付き述語を参照する。until_encoded 腕は
-- set-returning 関数 `until_encoded_deletable_originals(desired_store)`、
-- ごみ箱腕は set-returning 関数 `trash_deletable_recordings(grace_cutoff)`、
-- 外した版の腕は view `removed_encoded_assets`。この 3 つが唯一の定義であり、
-- 以下の入口・拾い直し・否定形のクエリはすべてこれらへの参照であって複製ではない。
-- 同名の旧 view `until_encoded_deletable_originals` は、ローリングアップデート中の
-- 旧 binary が旧列を読むための関数の包みで、このファイルは参照しない。旧列を消す
-- 変更（#1620）で view ごと消える。

-- 前パスで deleting にマークしたまま unlink できずに終わった行を拾い直す。
--
-- WHERE の 3 つの EXISTS は名前付き述語（上記）への参照であって複製ではない
-- （issue #160）。deleting は「再計算できる決定」であって不可逆な事実では
-- ないため、pending 経路は「既に決めた削除の再実行だから無条件に信じて
-- よい」とはできない —— ごみ箱からの復元は recordings.deleted_at だけを
-- 消す（RestoreRecording）ので、deleting の間に復元されると media_assets
-- 側は取り残されたまま unlink される（不変条件 9「適用の瞬間」。ruler が
-- toDelete を tx の外で計算していたのと同型の距離）。ここで判定条件を
-- 再評価し、該当しなくなった行は ListUnqualifiedDeletingAssets /
-- RevertMediaAssetToActive が active に戻す。
--
-- ブレーカーとの関係: この再評価は「新しく削除対象を増やす」判断ではない
-- （前パスで一度 active → deleting に遷移させた行の集合を超えて広げることは
-- ない。集合を絞る側にしか働かない）。したがって従来どおりサーキット
-- ブレーカーの対象外のままでよい。
--
-- 罠: until_encoded の原本も pending に乗る。ここを「recordings.deleted_at
-- IS NOT NULL」だけで判定すると、生きている録画の until_encoded 原本が
-- 永久に deleting のまま止まる（ごみ箱条件にも until_encoded 条件にも
-- 該当しない扱いになってしまうため）。各条件を OR で残すこと。
-- name: ListMediaAssetsPendingDelete :many
SELECT a.id, a.recording_id, copy.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN media_asset_copies copy
  ON copy.media_asset_id = a.id
 AND copy.store = sqlc.arg('store_name')
 AND copy.state = 'deleting'
WHERE (
    EXISTS (
      SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
      WHERE t.recording_id = a.recording_id
    )
    OR EXISTS (
      SELECT 1 FROM until_encoded_deletable_originals(sqlc.narg('desired_store')::text) AS v(asset_id, recording_id)
      WHERE v.asset_id = a.id
    )
    OR EXISTS (
      SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = a.id
    )
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- deleting のまま止まっていて、上の 3 条件のどれにも該当しなくなった行の
-- 候補を挙げる（issue #105）。判定条件は ListMediaAssetsPendingDelete の
-- WHERE をそのまま否定したもの（名前付き述語への NOT EXISTS なので、否定形を
-- 手で保守する必要がない。issue #160）。ここではまだ書き込まない ——
-- 呼び出し側が各行についてファイルの現存を確認してから、次のいずれかを選ぶ:
--
--   a. ファイルがまだ存在する → RevertMediaAssetToActive で active に戻す
--   b. ファイルが既に無い（unlink 成功後 MarkMediaAssetDeleted が
--      コミットされる前にプロセスが落ち、その間に復元された）→
--      MarkMediaAssetDeleted で deleted を確定する
--
-- (b) を単純にここで active へ戻してしまうと、案 B（復元時に deleting を
-- 同期的に active へ戻す）を却下した理由そのもの ——「active なのにファイルが
-- 無い行」を作ってしまう。この SELECT + Go 側の stat + 分岐は、その窓を
-- revert 経路自身に持ち込まないための構成。
-- name: ListUnqualifiedDeletingAssets :many
SELECT a.id, a.recording_id, copy.rel_path
FROM media_assets a
JOIN media_asset_copies copy
  ON copy.media_asset_id = a.id
 AND copy.store = sqlc.arg('store_name')
 AND copy.state = 'deleting'
WHERE NOT EXISTS (
    SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    WHERE t.recording_id = a.recording_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM until_encoded_deletable_originals(sqlc.narg('desired_store')::text) AS v(asset_id, recording_id)
    WHERE v.asset_id = a.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = a.id
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- ListUnqualifiedDeletingAssets が挙げた 1 行を active に戻す。ファイルが
-- まだ存在すると Go 側で確認できたときだけ呼ぶこと。
--
-- WHERE に同じ判定条件（名前付き述語、issue #160）を再度埋め込み、事前の
-- SELECT の結果（真偽値）を受け渡さずこの UPDATE 自体の瞬間に再評価する
-- （不変条件 9「適用の瞬間」）。SELECT から数行の Go コードを挟むだけの短い
-- 窓だが、その間に別の書き手（RestoreRecording・encode_profiles 変更 API 等）
-- が recordings 側を書き換えて再度条件を満たすようになっていれば、ここで
-- 0 行になり active には戻らない（= 正しく deleting のまま残り、pending
-- 経路が続行する）。
--
-- ここで active に戻すのは deleting → active の遷移のみで、進行中の unlink
-- とは競合しない: このワーカー自身が copy.state の唯一の書き手
-- （InsertOpts の UniqueOpts により同時に 1 パスしか走らない）であり、
-- deleting のまま次パスに持ち越された行は前パスのプロセスが既に終了して
-- いるので、いま unlink が進行中の行と衝突しようがない。復元 API から
-- deleting → active へ即座に戻す案を採らなかったのはこの前提が無い
-- （進行中の unlink と非同期に競合しうる）ため。
-- name: RevertMediaAssetToActive :execrows
UPDATE media_asset_copies copy
SET state = 'active', deleted_at = NULL
FROM media_assets a
WHERE a.id = sqlc.arg('id')
  AND copy.media_asset_id = a.id
  AND copy.store = sqlc.arg('store_name')
  AND copy.state = 'deleting'
  AND NOT EXISTS (
    SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    WHERE t.recording_id = a.recording_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM until_encoded_deletable_originals(sqlc.narg('desired_store')::text) AS v(asset_id, recording_id)
    WHERE v.asset_id = a.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = a.id
  );

-- ごみ箱の猶予超過、または「今すぐ完全削除」の要求（recording_purge_requests
-- の行）の対象
-- （名前付き述語 trash_deletable_recordings への参照。issue #160）。
-- name: ListTrashMediaAssetsToDelete :many
SELECT a.id, a.recording_id, copy.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    ON t.recording_id = a.recording_id
JOIN media_asset_copies copy
  ON copy.media_asset_id = a.id
 AND copy.store = sqlc.arg('store_name')
 AND copy.state = 'active'
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- keep_original='until_encoded' の原本削除候補。削除可否の条件は
-- until_encoded_deletable_originals の定義を参照する。ごみ箱経由の録画は
-- ListTrashMediaAssetsToDelete 側で扱う。
-- name: ListUntilEncodedOriginalsToDelete :many
SELECT v.asset_id::bigint AS id, v.recording_id::bigint, copy.rel_path, a.size_bytes, 'original'::text AS kind
FROM until_encoded_deletable_originals(sqlc.narg('desired_store')::text) AS v(asset_id, recording_id)
JOIN media_assets a ON a.id = v.asset_id
JOIN media_asset_copies copy
  ON copy.media_asset_id = a.id
 AND copy.store = sqlc.arg('store_name')
 AND copy.state = 'active'
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- ユーザーが外したエンコード版（名前付き述語 removed_encoded_assets への参照）。
-- ごみ箱は経由しない。容量を空けるための操作で、他の版が残ることは view が
-- 要求しているので、猶予で守るものが無い。
-- name: ListRemovedEncodedAssetsToDelete :many
SELECT x.asset_id AS id, x.recording_id, copy.rel_path, a.size_bytes, 'encoded'::text AS kind
FROM removed_encoded_assets x
JOIN media_assets a ON a.id = x.asset_id
JOIN media_asset_copies copy
  ON copy.media_asset_id = a.id
 AND copy.store = sqlc.arg('store_name')
 AND copy.state = 'active'
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- 「完全削除が完了した」という不可逆な事実を確定する（issue #135）。
--
-- 削除 reconcile のパス末尾（trash / until_encoded / pending 経路すべてが
-- 物理 unlink を終えた後）で 1 回だけ呼ぶ。ごみ箱条件は名前付き述語
-- trash_deletable_recordings への参照（issue #160）。即時削除の要求だけを
-- 条件にすると、30 日猶予超過の経路（要求の行が無いまま完全削除に到達する）を
-- 拾い損なう —— この区別は述語の定義側に集約されている。
--
-- recordings を起点に引く（media_assets を起点にすると、アセットを 1 行も
-- 持ったことがない録画が NOT EXISTS の対象になりようがなく、永久に拾えない。
-- issue #135 の実測 id 6/7/10/86 がこのケース）。判定は「state <> 'deleted'
-- のコピーが 1 つもない」であって「media_assets が 0 行」ではない
-- （NOT EXISTS は両方を等しく真にするので実装上は同じ式になるが、意図は
-- 前者 —— deleting は「消えた」に数えない。deleting はまだ unlink 待ちで、
-- issue #105 の経路で active に戻りうる）。
--
-- purged_at IS NULL を条件にも RETURNING にも使うことで、同じ録画を複数パスで
-- 数え直さない（冪等）。呼び出し側はこの結果をそのまま recording.deleted の
-- 発火対象にできる —— この WHERE が「発火してよい」の判定そのものであり、
-- 通知の瞬間に別途 EXISTS を計算して捨てる必要がない（旧 GetRecordingPurgeState
-- はこの計算を通知直前に行って結果を保存しなかったため、対象になる録画の
-- 集合そのものが存在しないという根本原因を残していた）。
-- name: MarkPurgedRecordings :many
UPDATE recordings r
SET purged_at = now(), updated_at = now()
WHERE r.purged_at IS NULL
  AND EXISTS (
    SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    WHERE t.recording_id = r.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM media_assets a
    WHERE a.recording_id = r.id
      AND EXISTS (
          SELECT 1 FROM media_asset_copies copy
          WHERE copy.media_asset_id = a.id AND copy.state <> 'deleted'
      )
  )
RETURNING r.id, r.site, r.title;

-- name: MarkMediaAssetDeleting :execrows
UPDATE media_asset_copies copy
SET state = 'deleting', deleted_at = NULL
WHERE copy.media_asset_id = sqlc.arg('asset_id')
  AND copy.store = sqlc.arg('store_name')
  AND copy.state = 'active';

-- until_encoded の候補は列挙後に利用者の再作成要求が追加されうるため、録画行を
-- ロックした tx 内で資格を再評価してから deleting にする。API の要求書き込みも
-- 同じ recordings 行を先にロックするため、要求の commit と原本削除は直列化される。
-- name: MarkUntilEncodedOriginalDeleting :execrows
UPDATE media_asset_copies copy
SET state = 'deleting', deleted_at = NULL
WHERE copy.media_asset_id = sqlc.arg('asset_id')
  AND copy.store = sqlc.arg('store_name')
  AND copy.state = 'active'
  AND EXISTS (
      SELECT 1 FROM until_encoded_deletable_originals(sqlc.narg('desired_store')::text) AS v(asset_id, recording_id)
      WHERE v.asset_id = sqlc.arg('asset_id')
  );

-- 外した版の要求行（encoded_asset_removal_requests）を deleted の確定と同じ
-- 文で消す。要求行は「この版を外した」という主張なので、版が消えたら残す意味が無い。
-- name: MarkMediaAssetDeleted :execrows
WITH cleared_removal_request AS (
    DELETE FROM encoded_asset_removal_requests q
    USING media_assets a
    WHERE a.id = sqlc.arg('id')
      AND EXISTS (
          SELECT 1 FROM media_asset_copies copy
          WHERE copy.media_asset_id = a.id
            AND copy.store = sqlc.arg('store_name')
            AND copy.state = 'deleting'
      )
      AND q.recording_id = a.recording_id
      AND q.profile = a.profile
)
UPDATE media_asset_copies copy
SET state = 'deleted', deleted_at = COALESCE(copy.deleted_at, now())
WHERE copy.media_asset_id = sqlc.arg('id')
  AND copy.store = sqlc.arg('store_name')
  AND copy.state = 'deleting';

-- 孤児判定用。テーブル全体を読んで Go 側でファイル一覧と突き合わせる
-- （家庭用サーバー規模の行数を前提。件数が増えたら bloom filter 等の検討対象）。
-- name: ListAllMediaAssetRelPaths :many
SELECT rel_path FROM media_asset_copies WHERE store = sqlc.arg('store_name');

-- 孤児候補を記録する。既存行があれば first_seen を保持する
-- （DO NOTHING。エイジングの起点は「最初に孤児だと気づいた時刻」）。
-- name: UpsertOrphanFile :exec
INSERT INTO orphan_files (store, rel_path) VALUES ($1, $2)
ON CONFLICT (store, rel_path) DO NOTHING;

-- name: DeleteOrphanFile :exec
DELETE FROM orphan_files WHERE store = $1 AND rel_path = $2;

-- name: ListAllOrphanFiles :many
SELECT rel_path, first_seen FROM orphan_files WHERE store = $1;

-- エイジング済みの孤児（first_seen が指定期間以上前）。DB が記録した first_seen と
-- 同じ DB の時計で比較し、呼び出し側ホストの時計差を持ち込まない。
-- name: ListAgedOrphanFiles :many
SELECT rel_path FROM orphan_files
WHERE store = sqlc.arg('store')
  AND first_seen <= now() - (sqlc.arg('age')::bigint * interval '1 microsecond');

-- 「active な media_asset_copy の実体が無い」検出用（issue #343、
-- docs/storage/missing-assets.md「孤児回収の逆」）。orphan_files の判定
-- （ListAllMediaAssetRelPaths）と対になる逆方向のクエリ: ここでは copy.state が
-- active な行だけを対象にする（deleting/deleted は物理削除の途中・完了
-- なのでファイルが無くて当然であり検出対象ではない）。
--
-- このファイルの他のソースと違って row_limit を持たない。判定が差集合なので
-- 入力を切ると答えが変わる（窓の外の行が「もう候補でない」と見なされて
-- first_seen が毎パス消え、エイジングが永久に完了しない）。理由の全文は
-- internal/worker/storage_audit.go の reconcileMissingAssets の
-- doc コメント。
-- name: ListActiveMediaAssets :many
SELECT a.id, a.recording_id, copy.rel_path, a.kind
FROM media_assets a
JOIN media_asset_copies copy ON copy.media_asset_id = a.id
WHERE copy.store = sqlc.arg('store_name')
  AND copy.state = 'active';

-- 実体無し候補を記録する。既存行があれば first_seen を保持する
-- （DO NOTHING。エイジングの起点は「最初に実体無しだと気づいた時刻」）。
-- name: UpsertMissingMediaAsset :exec
INSERT INTO missing_media_assets (media_asset_id, store) VALUES ($1, $2)
ON CONFLICT (media_asset_id, store) DO NOTHING;

-- name: DeleteMissingMediaAsset :exec
DELETE FROM missing_media_assets WHERE media_asset_id = $1 AND store = $2;

-- name: ListAllMissingMediaAssetIDs :many
SELECT media_asset_id FROM missing_media_assets WHERE store = $1;

-- エイジング済み（first_seen が指定期間以上前）で、なお active な行。経過時間は
-- 候補を記録した DB の時計で比べる。
-- rel_path / recording_id / kind は missing_media_assets に複製せず
-- media_assets / media_asset_copies との JOIN で引く（不変条件 9）。JOIN で copy.state = 'active' を
-- 再確認するのは、対象行がその後 deleting/deleted に遷移した、または
-- resolveUnqualifiedDeletingAsset 等で active に戻った場合に、報告の瞬間の
-- 状態を見るため（適用の瞬間の再評価。不変条件 9）。
-- name: ListAgedMissingMediaAssets :many
SELECT a.id, a.recording_id, copy.rel_path, a.kind, m.first_seen
FROM missing_media_assets m
JOIN media_assets a ON a.id = m.media_asset_id
JOIN media_asset_copies copy
  ON copy.media_asset_id = a.id
 AND copy.store = sqlc.arg('store')
 AND copy.state = 'active'
WHERE m.store = sqlc.arg('store')
  AND m.first_seen <= now() - (sqlc.arg('age')::bigint * interval '1 microsecond')
ORDER BY m.media_asset_id;
