-- issue #161 決定（案 A）: never-scheduled 行の識別を quality_events の jsonb
-- マーカーへの EXISTS(jsonb_array_elements(...)) から型付き列に昇格する。
--
-- quality_events が jsonb で許された根拠は「そのテーブル自身のロジックが
-- 中身を一切使わない不透明なペイロード」（docs/schema/principles.md §2）で、
-- 同 §8「型の規律」は「クエリ軸（WHERE / JOIN に使う列）は型付きカラム、
-- 内容でクエリするなら型付き列」と定める。issue #98 がこのマーカーを
-- 同期除外・重なり判定・容量判定という core ロジックの WHERE 軸にしたことで、
-- 自前の jsonb 規則への静かな違反が生まれていた（GIN も効かない
-- jsonb_array_elements の展開なので、行数が増えれば性能面でも素直でない）。
--
-- quality_events 自身は消さない。「いつ・どの理由で never-scheduled と
-- 判定したか」という内訳ログとして引き続き価値があり、この列と同居しても
-- 二重化ではない --- 「クエリ軸は型付きカラム、詳細ペイロード（内訳・retry
-- 経緯）は jsonb」という型の規律そのものの形になる。
--
-- +goose Up

ALTER TABLE recordings ADD COLUMN never_scheduled boolean NOT NULL DEFAULT false;

-- backfill: 既存マーカー付きの行を 1 回の UPDATE で埋める。以後この列を
-- 書くのは internal/reconciler.recordNeverScheduled（CreateNeverScheduledRecording）
-- だけで、値は行と同時に生まれて不変（#156 の判定基準）。
UPDATE recordings
SET never_scheduled = true
WHERE status = 'failed'
  AND EXISTS (
      SELECT 1 FROM jsonb_array_elements(quality_events) qe
      WHERE qe->>'event' = 'recording.never-scheduled'
  );

-- never_scheduled_events view（00030、issue #157）の核をこの列に置き換える。
-- 出力列は変わらないので CREATE OR REPLACE で足り、消費側（同期除外 3 クエリ・
-- 表示用 never_recorded）は無変更のまま jsonb の中身を読まなくなる。
CREATE OR REPLACE VIEW never_scheduled_events AS
SELECT site, network_id, service_id, event_id, deleted_at, superseded_at
FROM recordings
WHERE status = 'failed' AND never_scheduled;

-- +goose Down

-- **片道であることを明示的に許容する**（導出テーブルなので再構築できる。
-- CLAUDE.md 不変条件 11、00008/00010/00012/00017/00025 と同じ前例）。
-- quality_events マーカーは消していないので、view を jsonb 版に戻せば
-- 意味的な喪失は無い。
CREATE OR REPLACE VIEW never_scheduled_events AS
SELECT site, network_id, service_id, event_id, deleted_at, superseded_at
FROM recordings
WHERE status = 'failed'
  AND EXISTS (
      SELECT 1 FROM jsonb_array_elements(quality_events) qe
      WHERE qe->>'event' = 'recording.never-scheduled'
  );

ALTER TABLE recordings DROP COLUMN never_scheduled;
