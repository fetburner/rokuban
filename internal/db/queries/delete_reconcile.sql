-- 削除 reconcile（M3-8、docs/storage.md §7）。物理 unlink に至る 3 ソース
-- （ごみ箱 / until_encoded / 孤児）を 1 本のループに統一するためのクエリ群。
--
-- 削除プロトコルは冪等: active → deleting → deleted。deleting のまま
-- プロセスが落ちても ListMediaAssetsPendingDelete が次パスで拾い直す。
--
-- 「このアセットは消してよいか」の 2 つの腕（ごみ箱の猶予超過 or 今すぐ
-- purge / until_encoded の派生物完備）は、いずれもスキーマ側の名前付き述語
-- （00029_delete_reconcile_predicates.sql）を参照する。until_encoded 腕は
-- view `until_encoded_deletable_originals`、ごみ箱腕は set-returning 関数
-- `trash_deletable_recordings(grace_cutoff)`。この 2 つが唯一の定義であり、
-- 以下の 5 クエリはすべてこれらへの参照であって複製ではない（issue #160）。

-- 前パスで deleting にマークしたまま unlink できずに終わった行を拾い直す。
--
-- WHERE の 2 つの EXISTS は名前付き述語（上記）への参照であって複製ではない
-- （issue #160）。deleting は「再計算できる決定」であって不可逆な事実では
-- ないため、pending 経路は「既に決めた削除の再実行だから無条件に信じて
-- よい」とはできない —— ごみ箱からの復元は recordings.deleted_at だけを
-- 消す（RestoreRecording）ので、deleting の間に復元されると media_assets
-- 側は取り残されたまま unlink される（不変条件 9「適用の瞬間」。ruler が
-- toDelete を tx の外で計算していたのと同型の距離）。ここで判定条件を
-- 再評価し、該当しなくなった行は ListUnqualifiedDeletingAssets /
-- RevertMediaAssetToActive が active に戻す。
--
-- 意図的に再評価しない条件: docs/storage.md §6 の条件 3（原本を入力とする
-- 実行中・再試行中のジョブがない）は hasPendingDerivativeJob が Go 側で
-- river_job を見て判定するもので、ここでは再評価しない。この行は既に
-- deleting へ遷移済み = 最初に active → deleting へ遷移させた時点で条件 3 を
-- 満たしていたことが確定している。その後に新しいジョブがこの recording_id に
-- 積まれる競合は理論上あり得るが、#105 が対象とする「復元でも state が
-- 追随しない」問題とは別の競合であり、本 PR のスコープ外として明示的に
-- 残す（対応するなら pending 側でも hasPendingDerivativeJob 相当のチェックを
-- 挟む形になる）。
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
WHERE a.state = 'deleting'
  AND (
    EXISTS (
      SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
      WHERE t.recording_id = a.recording_id
    )
    OR EXISTS (
      SELECT 1 FROM until_encoded_deletable_originals v WHERE v.asset_id = a.id
    )
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- deleting のまま止まっていて、上の 2 条件のどちらにも該当しなくなった行の
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
SELECT a.id, a.recording_id, a.rel_path
FROM media_assets a
WHERE a.state = 'deleting'
  AND NOT EXISTS (
    SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    WHERE t.recording_id = a.recording_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM until_encoded_deletable_originals v WHERE v.asset_id = a.id
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
-- とは競合しない: このワーカー自身が media_assets.state の唯一の書き手
-- （InsertOpts の UniqueOpts により同時に 1 パスしか走らない）であり、
-- deleting のまま次パスに持ち越された行は前パスのプロセスが既に終了して
-- いるので、いま unlink が進行中の行と衝突しようがない。復元 API から
-- deleting → active へ即座に戻す案を採らなかったのはこの前提が無い
-- （進行中の unlink と非同期に競合しうる）ため。
-- name: RevertMediaAssetToActive :execrows
UPDATE media_assets a
SET state = 'active', updated_at = now()
WHERE a.id = sqlc.arg('id')
  AND a.state = 'deleting'
  AND NOT EXISTS (
    SELECT 1 FROM trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    WHERE t.recording_id = a.recording_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM until_encoded_deletable_originals v WHERE v.asset_id = a.id
  );

-- ごみ箱の猶予超過、または「今すぐ完全削除」の要求（recording_purge_requests
-- の行）の対象
-- （名前付き述語 trash_deletable_recordings への参照。issue #160）。
-- name: ListTrashMediaAssetsToDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN trash_deletable_recordings(sqlc.arg('grace_cutoff')::timestamptz) t
    ON t.recording_id = a.recording_id
WHERE a.state = 'active'
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- keep_original='until_encoded' で、desired な派生物（全 encode_profiles +
-- thumbnail）がすべて active でコミット済みの原本（名前付き述語
-- until_encoded_deletable_originals への参照。issue #160）。ごみ箱経由の
-- 録画は ListTrashMediaAssetsToDelete 側で扱うのでここでは除外する
-- （view 自身が r.deleted_at IS NULL を要求している）。
-- name: ListUntilEncodedOriginalsToDelete :many
SELECT v.asset_id AS id, v.recording_id, v.rel_path, v.size_bytes, 'original'::text AS kind
FROM until_encoded_deletable_originals v
WHERE v.state = 'active'
ORDER BY v.asset_id
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
-- の media_assets が 1 行もない」であって「media_assets が 0 行」ではない
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
    WHERE a.recording_id = r.id AND a.state <> 'deleted'
  )
RETURNING r.id, r.site, r.title;

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
