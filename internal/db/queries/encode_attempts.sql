-- encode ジョブの直近の試行状態（issue #316）。書き手は EncodeWorker だけ。
-- 表そのものの設計判断は docs/schema/recordings.md
-- 「recording_encode_attempts --- encode ジョブの直近の試行状態（衛星表）」を参照。

-- name: UpsertRecordingEncodeAttemptRunning :exec
-- 試行の開始を記録する。行の存在そのものが「running か failed のどちらかを
-- 主張している」ことになる（不変条件 10）ので、直前が failed だった行も
-- ここで running に上書きする（再試行が始まったので古い失敗の主張を残さない）。
INSERT INTO recording_encode_attempts (
    recording_id, profile, state, error, attempted_at
) VALUES ($1, $2, 'running', NULL, now())
ON CONFLICT (recording_id, profile) DO UPDATE SET
    state        = 'running',
    error        = NULL,
    attempted_at = now();

-- name: UpsertRecordingEncodeAttemptFailed :exec
-- 試行の失敗を記録する。ctx キャンセル（River の停止・シャットダウン）由来の
-- 中断はここを呼ばない（呼び出し側 shouldNotifyEncodeFailure と同じ判定。
-- ジョブの失敗ではないので running のまま残す --- 次の実行が上書きする）。
INSERT INTO recording_encode_attempts (
    recording_id, profile, state, error, attempted_at
) VALUES ($1, $2, 'failed', $3, now())
ON CONFLICT (recording_id, profile) DO UPDATE SET
    state        = 'failed',
    error        = EXCLUDED.error,
    attempted_at = now();

-- name: DeleteRecordingEncodeAttempt :exec
-- 試行行を消す。呼ぶのは runEncode の defer（成功時）で、commitEncoded の
-- 直後ではない --- 間に webhook 通知（HTTP、タイムアウトまで待つ）が入り、
-- 同一トランザクションでもない。「完了しているのに失敗中」という中間状態を
-- 読者に見せないのは、この DELETE の速さではなく API 側が encoded 資産のある
-- プロファイルを encodeJobStatusesFromFields の対象から先に除外しているため。
-- 行が無くても成功する（冪等）。EncodeWorker の冪等スキップ経路（既に active
-- な encoded がある）でも、リークした古い試行行を掃除するために呼ぶ。
DELETE FROM recording_encode_attempts WHERE recording_id = $1 AND profile = $2;
