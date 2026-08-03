-- 「本物の record が推論に必ず勝つ」（issue #98 の決定、issue #129 症状 2 が最初の
-- 適用）の前段: 同一 active-event (site, network_id, service_id, event_id) に
-- status='failed' の行が「生きて」（deleted_at IS NULL AND superseded_at IS NULL、
-- 00023 で追加した recordings_unique_active_event の述語）残っていれば、
-- superseded_at を立てて枠を明け渡させる。呼び出し側（internal/watcher の
-- createRecording）はこのクエリを CreateRecording の直前に同じトランザクション内で
-- 呼ぶ。
--
-- **1 つの WITH 句にまとめて CreateRecording 側の INSERT と一体化させなかった。**
-- 最初はその形（1 クエリで完結させる CTE）で書いたが、Postgres の WITH 内の
-- データ変更文は「主クエリと同時並行に実行され、順序は不定」（PostgreSQL
-- documentation, `WITH` Queries (Common Table Expressions)）で、この CTE を
-- 主 INSERT が参照していない（RETURNING id を読み捨てるだけ）ため、実際に
-- INSERT が一意制約違反で失敗するケースを手元のテストで確認した
-- （TestProcessRecord_SupersedesFailedRecording が最初に落ちた形。「実装を
-- 壊すと落ちることを確認する」の逆側の学びとして残す）。2 つの独立した
-- 文（この UPDATE を先に確定させてから次の INSERT を発行）に分けることで、
-- 同一トランザクション内でコマンドカウンタが進み、後続の INSERT が
-- 確実に更新後の索引状態を見る。
--
-- superseded_at は「この行が active-event の枠を明け渡した」という不可逆な事実
-- だけを持つ列で、ユーザーのごみ箱操作を表す deleted_at とは別物にした
-- （不変条件 9: 2 つの事実を同じ列に同居させない。deleted_at を流用すると
-- ごみ箱ビュー・GC がユーザー操作でない行をユーザー操作と誤読する）。
--
-- WHERE status = 'failed' に絞っているので、'recording'/'finished'/'canceled' の
-- 生きている行は巻き込まない —— それらと衝突する INSERT は素の一意制約違反として
-- 従来どおりエラーになる（同一イベントの本物の重複 record を黙って追い出すのは
-- このクエリの責務ではない）。
--
-- media_assets を持つ failed 行（途中まで録れて failed になった行）でも扱いは同じ:
-- superseded にするだけで media_assets.recording_id は書き換えない。ファイルの
-- 所有者は superseded になった旧 recordings 行のままで、物理削除は媒体削除
-- reconcile が recordings.deleted_at を見て判断するので、superseded だけでは
-- 何も物理的に消えない（internal/watcher の
-- TestProcessRecord_SupersedesFailedRecordingWithMediaAsset で固定）。
--
-- 対象の failed 行が無ければ 0 行のまま何もしない。record_sweep 等が同一 record を
-- 再処理しても、processRecord は record_sync の行ロックで 2 回目以降
-- createRecording 自体を呼ばない（internal/watcher/watcher.go の AcquireRecordSync
-- 参照）ので、このクエリも 2 回目以降は呼ばれず、superseded_at が二重に進んだり
-- 行が重複したりしない。
-- name: SupersedeFailedRecording :execrows
UPDATE recordings
SET superseded_at = now(), updated_at = now()
WHERE site = sqlc.arg('site')
  AND network_id = sqlc.arg('network_id')
  AND service_id = sqlc.arg('service_id')
  AND event_id = sqlc.arg('event_id')
  AND deleted_at IS NULL AND superseded_at IS NULL AND status = 'failed';

-- name: CreateRecording :one
INSERT INTO recordings (
    reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, started_at, ended_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    $18, $19, $20
) RETURNING id;

-- name: UpdateRecordingStatus :exec
-- status は 'finished' / 'failed' / 'canceled' に達したら降格させない
-- （out-of-order な 'recording' イベントが後から来ても上書きしない。
-- 'canceled' は録画が再開しない取消なので他の 2 つと同じ終端として扱う。
-- issue #130）。
UPDATE recordings SET
    status     = CASE WHEN status IN ('finished', 'failed', 'canceled') THEN status ELSE sqlc.arg('new_status') END,
    started_at = COALESCE(started_at, sqlc.arg('started_at')),
    ended_at   = CASE WHEN sqlc.narg('ended_at')::timestamptz IS NOT NULL THEN sqlc.narg('ended_at') ELSE ended_at END,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- ON CONFLICT の述語は recordings_unique_active_event（00023 で
-- `AND superseded_at IS NULL` を追加済み）と一字一句一致させる必要がある
-- （Postgres は ON CONFLICT の対象インデックスを述語込みで照合するため、
-- ずれると「there is no unique or exclusion constraint matching」で落ちる）。
-- 一致させておくことで、この INSERT が狙う相手は常に「生きている」行
-- （superseded 済みの過去の failed 行ではない）になる。
-- name: CreateFailedRecording :exec
INSERT INTO recordings (
    reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, quality_events
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    'failed', $18
)
ON CONFLICT (site, network_id, service_id, event_id) WHERE deleted_at IS NULL AND superseded_at IS NULL
DO UPDATE SET
    quality_events = recordings.quality_events || EXCLUDED.quality_events,
    updated_at = now();

-- 録画一覧。原本のサイズと PID 別 drop_stats の合計、
-- 再生可能な encoded プロファイル名を同梱する。
-- PID 別の内訳は行数が多く一覧では使わないので ListRecordingDropStats で別に取る。
-- name: ListRecordings :many
SELECT
    r.*,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
    -- ブラウザ再生用。desired（r.encode_profiles）ではなく observed（active encoded）。
    -- sqlc は array_agg の型を推論しきれないことがあるので text[] に明示キャストする。
    (
        SELECT coalesce(array_agg(e.profile ORDER BY e.profile), '{}')::text[]
        FROM media_assets e
        WHERE e.recording_id = r.id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile IS NOT NULL
    ) AS available_encoded_profiles
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true
WHERE r.site = $1 AND r.deleted_at IS NULL
ORDER BY r.program_start_at DESC, r.id DESC;

-- name: ListRecordingDropStats :many
SELECT d.pid, d.packets, d.drops, d.errors, d.scrambled, d.pid_type
FROM drop_stats d
JOIN media_assets a ON a.id = d.media_asset_id
WHERE a.recording_id = $1 AND a.kind = 'original' AND a.state <> 'deleted'
ORDER BY d.pid;

-- name: AppendQualityEvents :exec
UPDATE recordings
SET quality_events = quality_events || sqlc.arg('events')::jsonb,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- ingest が原本 media_asset のコミットと同じ tx で焼く「この録画の望ましい
-- 最終状態」（M3-14、issue #103）。凍結する理由・瞬間・冪等性の詳細は
-- internal/worker/ingest.go の resolveAndSnapshotEncodePolicy の doc コメント参照。
-- 予約行が無い録画（手動で mirakc に起こされた録画等）は呼び出し側がこのクエリを
-- 呼ばないので、列は CREATE TABLE の既定値（'always' / '{}'）のまま残る。
-- name: SnapshotRecordingEncodePolicy :exec
UPDATE recordings SET
    keep_original   = sqlc.arg('keep_original'),
    encode_profiles = sqlc.arg('encode_profiles')::text[],
    updated_at      = now()
WHERE id = sqlc.arg('id');
