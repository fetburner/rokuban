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

-- 復元（store の Restore が、recordings 行のロック → RestoreRecording →
-- 要求の取り下げ、の順に 1 トランザクションで流す）。
--
-- 2 表を 1 文のデータ変更 CTE で書くのは**やめた**。CTE の全アームは文全体で
-- 1 つのスナップショットを共有するので、`recordings` の行ロックで UPDATE アームが
-- 待たされて成功しても、DELETE アームは待っている間に別トランザクションが
-- commit した要求行を見られない。**1 文にしても「復元は成功したのに即時要求だけ
-- 残る」は観測される**（store のごみ箱契約テストを旧 CTE 実装に戻すと、
-- 残存要求のアサーションで落ちる）。残った要求行は
-- trash_deletable_recordings が `deleted_at IS NOT NULL` を要求するのでその場では
-- 何も起こさないが、次の普通の soft-delete で 30 日の猶予をバイパスして即時 purge
-- の対象になる —— ユーザーは即時削除を要求していないのに。
--
-- 2 文に割ると、DELETE は UPDATE が返った**後に新しいスナップショット**を取るので
-- （READ COMMITTED）、UPDATE がロック待ちしている間に commit された要求行が見える。
-- 「23505 で UPDATE が落ちたら DELETE も巻き戻る」という CTE で得ていた性質は
-- トランザクションが保つ。
--
-- ただし**2 文に割っただけでは窓は閉じない**。DELETE が 0 行だった（まだ要求が
-- 無い）ときロックは何も残らない（READ COMMITTED に述語ロックは無い）ので、
-- 「DELETE の後・COMMIT の前」に要求行が commit されれば同じ害が出る。それを
-- 塞いでいるのは**要求行を入れる経路が対象の recordings 行を先にロックすること**
-- で、下の InsertRecordingPurgeRequest の前に store が行ロックを取る
-- （restore が行ロックを持っている間 purge が待たされることは store のごみ箱契約テストで見る）。
-- **要件: この表に INSERT する経路を足すときは recordings 行を先にロックする。**
-- 一括 purge を `INSERT INTO recording_purge_requests SELECT id FROM recordings
-- WHERE deleted_at IS NOT NULL ON CONFLICT DO NOTHING` と素直に書くと recordings
-- 行をロックしないので、上の窓が黙って開き直る（そして
-- restore 側のテストは blocker 側が `UPDATE recordings` してからロックを握る形なので、
-- この回帰は「purge request serializes behind restore」のテストで検出する）。
--
-- 行ロックは store の共通ヘルパ（LockRecording の FOR UPDATE）が、録画の行に触れる
-- 最初の文として取る。

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

-- 即時物理削除の要求行。ファイルは消さない。
-- 既に要求の行があれば何もしない（DO NOTHING）。冪等に再要求できるが、
-- requested_at は最初の要求のまま据え置く（「いつ要求されたか」を後の再要求で
-- 上書きしない）。
--
-- store の RequestPurge は、recordings 行のロック → SoftDeleteRecording（まだ
-- ごみ箱に無ければ deleted_at を立てる）→ この INSERT の 3 文を 1 トランザクション
-- で流す。**この INSERT は必ずロックの後に置く**（上の復元のコメント参照）。
-- 要求行を入れる前に対象の recordings 行をロックしないと、復元の窓が開き直る。
-- データ変更 CTE に畳まない理由も上のとおり（スナップショット共有）で、MySQL が
-- データ変更 CTE を書けないので両方言で同じ複数文の形にしてある。
-- name: InsertRecordingPurgeRequest :exec
INSERT INTO recording_purge_requests (recording_id)
VALUES ($1)
ON CONFLICT (recording_id) DO NOTHING;
