-- 「番組終了時点で schedule が一度も観測されなかった」は試行ではなく欠測の
-- 観測である。これを recordings に status='failed' + never_scheduled=true の
-- 擬似行として書くのをやめ、放送イベントを主キーにした専用表の「行の存在」で
-- 表す（CLAUDE.md 不変条件 10「行の存在そのものを主張として使う」、12「表は
-- 行の寿命で割る」）。recordings は観測された試行だけを持つ脊椎に戻る。
--
-- これで次がまとめて解ける:
--   * recordings の書き手が watcher（試行）1 人になる（reconciler は書かない）
--   * status='failed' なのに record が無い行が消える
--   * 欠測 ↔ 本物の record の supersede が要らなくなる（mirakc 由来 failed →
--     成功 record の supersede は recordings 内で従来どおり残る）
--   * never_scheduled 列と quality_events の同名マーカーの二重が消える
--
-- 表の名前は VIEW と同じ never_scheduled_events にする。同期除外の 3 読者
-- （ListReservationsForSyncEvaluation / ListOverlappingReservations /
-- ListCapacityDemand）は放送イベントキーで `SELECT 1 FROM never_scheduled_events`
-- しているだけなので、VIEW をこの表に置き換えれば SQL は無変更で通る。
--
-- +goose Up

-- 旧述語 VIEW（00030 → 00033）を落として同名の表を作る。
DROP VIEW IF EXISTS never_scheduled_events;

-- 主キーは放送イベント (site, network_id, service_id, event_id)。行の存在＝
-- 「このイベントは schedule されなかった」。書き手は reconciler だけ。
-- program_snapshots への FK は張らない —— snapshots は放送 + 猶予で GC されるが
-- 欠測は永続の観測で、CASCADE で消すと GC 後に同期除外が外れて同じ穴が
-- reservations 側で再発する（issue の罠）。
CREATE TABLE never_scheduled_events (
    site        text    NOT NULL,
    network_id  integer NOT NULL,
    service_id  integer NOT NULL,
    event_id    integer NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, network_id, service_id, event_id)
);

-- 既存の never-scheduled 擬似行から放送イベントを移設する。live 限定を掛けない
-- のは同期除外の意味（一度欠測と判定したイベントは、その後 recordings 行が
-- supersede されても対象に戻らない）を保つため。
INSERT INTO never_scheduled_events (site, network_id, service_id, event_id)
SELECT DISTINCT site, network_id, service_id, event_id
FROM recordings
WHERE never_scheduled
ON CONFLICT DO NOTHING;

-- 擬似行を recordings から掃除する。欠測は試行ではないので recordings に
-- 残さない（ライブラリに failed 行として出さない）。never-scheduled 行は
-- media_assets を 1 行も持たないので FK に引っかからない。
DELETE FROM recordings WHERE never_scheduled;

ALTER TABLE recordings DROP COLUMN never_scheduled;

-- +goose Down

-- **片道であることを明示的に許容する**（00025/00033 と同じ前例）。列を戻し、
-- 述語 VIEW を jsonb マーカー版で再建するが、DELETE した擬似行と、移設した
-- 欠測イベントは復元しない（Down は開発時のロールバック用途）。
ALTER TABLE recordings ADD COLUMN never_scheduled boolean NOT NULL DEFAULT false;

DROP TABLE never_scheduled_events;

CREATE VIEW never_scheduled_events AS
SELECT site, network_id, service_id, event_id, deleted_at, superseded_at
FROM recordings
WHERE status = 'failed'
  AND EXISTS (
      SELECT 1 FROM jsonb_array_elements(quality_events) qe
      WHERE qe->>'event' = 'recording.never-scheduled'
  );
