-- ingest 転送の途中経過（issue #212）。書き手は IngestWorker だけ。
-- 表そのものの設計判断は docs/schema/recordings/satellites.md
-- 「recording_ingest_progress --- 転送の途中経過（衛星表）」を参照。

-- name: GetRecordSyncIngestTarget :one
-- ingest ジョブが転送を始める前に読む、(site, record_id) の観測。
-- recording_id は転送結果のコミット先、content_length は進捗の分母。
-- record_sync.sql の GetRecordSyncRecordingID と重複するように見えるが、
-- あちらは recording_id しか返さないので分母を持ってこられない。
SELECT recording_id, content_length FROM record_sync
WHERE site = $1 AND record_id = $2;

-- name: GetRecordingIngestTempState :one
SELECT temp_rel_path, temp_generation
FROM recording_ingest_progress
WHERE recording_id = $1 AND temp_store = $2;

-- name: NextIngestTempGeneration :one
SELECT nextval('ingest_temp_generation_seq')::bigint AS generation;

-- name: UpsertRecordingIngestProgress :exec
-- 未 commit temp の進捗を上書きする。行の存在そのものが temp の置き場と世代を指す
-- （不変条件 10）。転送中の written_bytes = 0 は「転送を開始した」ことを表し、
-- record の cancel / fail 後も部分 temp を一時再生する間は行を残す。
--
-- written_bytes は GREATEST を取らない --- ジョブ再試行（層 2）は temp replay 後の
-- 実ファイル末尾から続くため、通常は戻らない。サイズ / ハッシュ不一致で temp を
-- 捨てた次の試行は 0 から始まる。cancel / fail では部分 temp と最後の観測値を残す
-- （written_bytes は単調増加を保証しない。docs/schema/recordings/satellites.md
-- 「recording_ingest_progress」参照）。
INSERT INTO recording_ingest_progress (
    recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation, temp_store, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (recording_id) DO UPDATE SET
    written_bytes  = EXCLUDED.written_bytes,
    expected_bytes = EXCLUDED.expected_bytes,
    temp_rel_path  = EXCLUDED.temp_rel_path,
    temp_generation = EXCLUDED.temp_generation,
    temp_store      = EXCLUDED.temp_store,
    observed_at    = now();

-- name: DeleteRecordingIngestProgress :exec
-- temp の行を消す。原本 media_asset を INSERT するのと同じ tx で呼ぶことで、
-- 「原本があるのに未 commit temp を指す」という中間状態を読者に見せない（不変条件 3）。
-- 行が無くても成功する（冪等）。
DELETE FROM recording_ingest_progress WHERE recording_id = $1;
