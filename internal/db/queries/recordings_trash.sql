-- ごみ箱（論理削除 / 復元 / 即時 purge 要求）。M3-7 / issue #69。
-- 即時 purge 要求は recording_purge_requests 衛星表の**行の存在**で表す
-- （旧 recordings.purge_after。移設の理由はマイグレーションのコメント）。
-- 物理 unlink はしない（M3-8）。api ロールは DB だけ触る。

-- 論理削除。既に deleted_at が立っていても COALESCE で据え置き（冪等）。
-- 行が無ければ 0 行（:one なので pgx.ErrNoRows → API が 404）。
-- name: SoftDeleteRecording :one
UPDATE recordings
SET deleted_at = COALESCE(deleted_at, now()),
    updated_at = now()
WHERE id = $1
RETURNING id, deleted_at;

-- 復元の 2 文（api の RestoreRecording ハンドラが 1 トランザクションで順に流す）。
--
-- 2 表を 1 文のデータ変更 CTE で書くのは**やめた**。CTE の全アームは文全体で
-- 1 つのスナップショットを共有するので、`recordings` の行ロックで UPDATE アームが
-- 待たされて成功しても、DELETE アームは待っている間に別トランザクションが
-- commit した要求行を見られない。**1 文にしても「復元は成功したのに即時要求だけ
-- 残る」は観測される**（`TestRestoreRecording_ConcurrentPurgeRequest_Withdrawn` を
-- 旧 CTE 実装に戻すとこのアサーションで落ちる、を確認済み）。残った要求行は
-- trash_deletable_recordings が `deleted_at IS NOT NULL` を要求するのでその場では
-- 何も起こさないが、次の普通の soft-delete で 30 日の猶予をバイパスして即時 purge
-- の対象になる —— ユーザーは即時削除を要求していないのに。
--
-- 2 文に割ると、DELETE は UPDATE が返った**後に新しいスナップショット**を取るので
-- （READ COMMITTED）、UPDATE がロック待ちしている間に commit された要求行が見える。
-- 「23505 で UPDATE が落ちたら DELETE も巻き戻る」という CTE で得ていた性質は
-- トランザクションが保つ。
--
-- 先に `SELECT ... FOR UPDATE` で明示的に行を掴む形も試したが、上のテストは
-- ロック文を消しても通る（実測）—— 窓を閉じているのは 2 文に割ったことなので、
-- 効果を測れない文は置かない。

-- ごみ箱に入っている行だけを対象に deleted_at を消す。
-- 同一イベントに生きている録画がある場合は unique partial index で 23505。
-- purged_at が立っている行（完全削除が完了した tombstone、issue #135）は
-- 対象外 —— WHERE に条件を足して 0 行にし、既存の 404 経路に落とす。
-- ファイルは二度と戻らないので、それをライブラリに戻すと「再生できない
-- 録画」が並んでしまう。
-- name: RestoreRecording :one
UPDATE recordings
SET deleted_at = NULL,
    updated_at = now()
WHERE id = $1 AND deleted_at IS NOT NULL AND purged_at IS NULL
RETURNING id;

-- 即時 purge 要求の取り消し（不変条件 10: 取り消しは DELETE）。
-- 上の UPDATE が 0 行だった（ごみ箱に無い / purge 済み）ときは**呼ばない** ——
-- 404 を返しながら「消してと言った事実」だけ黙って取り消してはいけない
-- （TestRestoreRecording_NotInTrash_KeepsPurgeRequest）。
-- name: WithdrawRecordingPurgeRequest :exec
DELETE FROM recording_purge_requests WHERE recording_id = $1;

-- 即時物理削除の要求。ファイルは消さない。
-- purge は soft-delete も兼ねる（まだごみ箱に入っていなければ deleted_at を立てる）。
-- 既に要求の行があれば何もしない（DO NOTHING）。冪等に再要求できるが、
-- requested_at は最初の要求のまま据え置く（「いつ要求されたか」を後の再要求で
-- 上書きしない）。
--
-- 存在しない録画には 0 行（recordings 側の UPDATE が 0 行 → 主クエリも 0 行 →
-- :one が pgx.ErrNoRows → API が 404）。
-- name: MarkRecordingPurgeRequested :one
WITH trashed AS (
    UPDATE recordings
    SET deleted_at = COALESCE(deleted_at, now()),
        updated_at = now()
    WHERE id = $1
    RETURNING id, deleted_at
), requested AS (
    INSERT INTO recording_purge_requests (recording_id)
    SELECT id FROM trashed
    ON CONFLICT (recording_id) DO NOTHING
    RETURNING recording_id
)
SELECT id, deleted_at FROM trashed;

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
--
-- 罠（PR #187 レビュー M4）: GET /api/recordings?trash=true は M3-24（issue
-- #136）以降このクエリを使わない --- internal/api/recordings_query.go の動的
-- WHERE ビルダが同じ WHERE 述語（deleted_at IS NOT NULL AND purged_at IS NULL）
-- を再現しつつ、ORDER BY は他の絞り込み軸と 1 つのキーセット契約に統一するため
-- `program_start_at DESC, id DESC` を使う（このクエリの `deleted_at DESC, id
-- DESC` とは異なる並び順。docs/api.md「録画一覧: 絞り込み + キーセットページ
-- ング」参照）。このクエリ自体は削除していない（sqlc 生成物・将来の直接利用の
-- 参考実装として残すが、GET /api/recordings の実装を変えたい場合はここではなく
-- recordings_query.go を直す）。
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
