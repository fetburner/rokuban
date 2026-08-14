-- ingest 転送の途中経過（issue #212）。書き手は IngestWorker だけ。
-- 表そのものの設計判断は internal/db/migrations/00036_recording_ingest_progress.sql
-- の doc コメント参照。

-- name: GetRecordSyncIngestTarget :one
-- ingest ジョブが転送を始める前に読む、(site, record_id) の観測。
-- recording_id は転送結果のコミット先、content_length は進捗の分母。
-- record_sync.sql の GetRecordSyncRecordingID と重複するように見えるが、
-- あちらは recording_id しか返さないので分母を持ってこられない。
SELECT recording_id, content_length FROM record_sync
WHERE site = $1 AND record_id = $2;

-- name: UpsertRecordingIngestProgress :exec
-- 転送中の進捗を上書きする。行の存在そのものが「転送中」の主張なので
-- （不変条件 10）、written_bytes = 0 の初回書き込みにも意味がある
-- （「転送を開始した」）。
--
-- written_bytes は GREATEST を取らない --- ジョブ再試行（層 2）は部分ファイルを
-- truncate してゼロから作り直すため、戻りが事実である
-- （00036_recording_ingest_progress.sql の「written_bytes は単調増加しない」）。
INSERT INTO recording_ingest_progress (
    recording_id, written_bytes, expected_bytes, observed_at
) VALUES ($1, $2, $3, now())
ON CONFLICT (recording_id) DO UPDATE SET
    written_bytes  = EXCLUDED.written_bytes,
    expected_bytes = EXCLUDED.expected_bytes,
    observed_at    = now();

-- name: DeleteRecordingIngestProgress :exec
-- 進捗行を消す。原本 media_asset を INSERT するのと同じ tx で呼ぶことで、
-- 「原本があるのに取り込み中」という中間状態を読者に見せない（不変条件 3）。
-- 行が無くても成功する（冪等）。
DELETE FROM recording_ingest_progress WHERE recording_id = $1;
