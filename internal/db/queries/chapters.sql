-- チャプターの所有モデル。自動層（recording_cm_detections）は導出値、
-- ユーザー層（recording_chapter_ownership + recording_chapter_spans）は
-- 二度と再取得できない事実。所有の行がある録画では自動層を読まない。

-- ---------------------------------------------------------------------------
-- 行ロック。書き込みの tx は必ずこれを先頭で取る。
-- ---------------------------------------------------------------------------

-- name: LockRecording :one
SELECT id,
       (deleted_at IS NOT NULL)::boolean AS is_trashed,
       (purged_at IS NOT NULL)::boolean AS is_purged
FROM recordings
WHERE id = sqlc.arg('recording_id')
FOR UPDATE;

-- ---------------------------------------------------------------------------
-- 読む
-- ---------------------------------------------------------------------------

-- name: GetRecordingChapterState :one
SELECT COALESCE(p.cm_detect, false)::boolean AS cm_detect,
       r.program_duration_ms,
       EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id) AS detected,
       -- 版の材料。行が無ければ 0（マイクロ秒の epoch）。
       COALESCE((SELECT (extract(epoch FROM d.detected_at) * 1000000)::bigint
                 FROM recording_cm_detections d WHERE d.recording_id = r.id), 0)::bigint AS detected_at_us,
       COALESCE((SELECT (extract(epoch FROM o.adopted_at) * 1000000)::bigint
                 FROM recording_chapter_ownership o WHERE o.recording_id = r.id), 0)::bigint AS adopted_at_us,
       EXISTS (
           SELECT 1 FROM recording_cm_attempts ca
           WHERE ca.recording_id = r.id AND ca.state = 'failed'
       ) AS failed,
       EXISTS (
           SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = r.id
       ) AS owned
FROM recordings r
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
WHERE r.id = sqlc.arg('recording_id')
  AND r.purged_at IS NULL;

-- 自動層の CM 区間。ms の半開区間。行が無ければ空配列を返す（COALESCE が要る
-- --- 集約は行が無いと NULL になる）。
-- name: GetRecordingCMRangesJSON :one
SELECT COALESCE(
    jsonb_agg(
        jsonb_build_object('startMs', lower(cr.cm_range), 'endMs', upper(cr.cm_range))
        ORDER BY lower(cr.cm_range)
    ),
    '[]'::jsonb
)::jsonb AS ranges
FROM recording_cm_detections d
CROSS JOIN LATERAL unnest(d.cm_ranges) AS cr(cm_range)
WHERE d.recording_id = sqlc.arg('recording_id');

-- ユーザー層。行が無ければ空配列。label は NULL を保つ（sqlc のポインタ型）。
-- name: GetRecordingChapterSpansJSON :one
SELECT COALESCE(
    jsonb_agg(
        jsonb_build_object('startMs', lower(s.span), 'endMs', upper(s.span), 'label', s.label, 'cut', s.cut)
        ORDER BY lower(s.span)
    ),
    '[]'::jsonb
)::jsonb AS spans
FROM recording_chapter_spans s
WHERE s.recording_id = sqlc.arg('recording_id');

-- ---------------------------------------------------------------------------
-- 書く
-- ---------------------------------------------------------------------------

-- 所有の行が無ければ作る。既にあれば adopted_at を書き換えない（「いつ確認したか」
-- は最初の 1 回の事実）。
-- name: UpsertRecordingChapterOwnership :exec
INSERT INTO recording_chapter_ownership (recording_id)
VALUES (sqlc.arg('recording_id'))
ON CONFLICT (recording_id) DO NOTHING;

-- 自動に戻す。spans は ON DELETE CASCADE で一緒に落ちる。
-- name: DeleteRecordingChapterOwnership :exec
DELETE FROM recording_chapter_ownership WHERE recording_id = sqlc.arg('recording_id');

-- name: DeleteRecordingChapterSpans :exec
DELETE FROM recording_chapter_spans WHERE recording_id = sqlc.arg('recording_id');

-- name: InsertRecordingChapterSpan :exec
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES (
    sqlc.arg('recording_id'),
    int8range(sqlc.arg('start_ms')::bigint, sqlc.arg('end_ms')::bigint),
    sqlc.narg('label'),
    sqlc.arg('cut')
);

-- ---------------------------------------------------------------------------
-- catalog（export / rescue）
-- ---------------------------------------------------------------------------

-- ユーザーの手作業（不可逆）なので rescue で戻す。所有の行が無い録画は
-- 自動層のまま = 何も復元しない（行の不在そのものが意味を持つ。不変条件 10）。
-- name: CatalogListRecordingChapterOwnerships :many
SELECT recording_id, adopted_at
FROM recording_chapter_ownership
ORDER BY recording_id;

-- name: CatalogListRecordingChapterSpans :many
SELECT recording_id, lower(span)::bigint AS start_ms, upper(span)::bigint AS end_ms, label, cut
FROM recording_chapter_spans
ORDER BY recording_id, lower(span);

-- name: CatalogUpsertRecordingChapterOwnership :exec
INSERT INTO recording_chapter_ownership (recording_id, adopted_at)
VALUES ($1, $2)
ON CONFLICT (recording_id) DO UPDATE SET
    adopted_at = EXCLUDED.adopted_at;

-- 所有の行を先に upsert してから呼ぶ（FK 先）。主キーが無く、同じ区間を 2 回
-- 入れると EXCLUDE が自分自身と衝突するので DO NOTHING で受ける（同じ世代を
-- 2 回当てても rescue が落ちない）。
-- name: CatalogInsertRecordingChapterSpan :exec
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES (
    sqlc.arg('recording_id'),
    int8range(sqlc.arg('start_ms')::bigint, sqlc.arg('end_ms')::bigint),
    sqlc.arg('label'),
    sqlc.arg('cut')
)
ON CONFLICT DO NOTHING;
