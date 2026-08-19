-- +goose Up

-- issue #319: `purge_after` は「今すぐ完全削除してほしい」という要求印だが、
-- `timestamptz` かつ `<= now()` の比較で実装しており、これは実質 boolean。
-- `deleted_at`（可逆な捨てた時刻）/ `superseded_at`（不可逆な枠明け渡し）/
-- `purged_at`（不可逆な完全削除の完了）と並べて 4 つ目の「時刻」に見えるが、
-- 未来の予約削除としては使っておらず、書く値は常に「今」しかない
-- （CLAUDE.md 不変条件 9: 導出値と不可逆な事実を同じ列に載せない ---
-- ここでは「時刻に見える弱い印」を型で正直にする）。
--
-- 印なので boolean にする。RestoreRecording が false に落とす（時刻を消す
-- のではなく印を下ろす）。削除 reconcile の即時腕は「印が立っているか」だけを
-- 見て `<= now()` の比較をやめる。猶予腕（deleted_at + 設定日数）は変えない。
ALTER TABLE recordings
    ADD COLUMN purge_requested boolean NOT NULL DEFAULT false;

-- 旧列に印が付いていた行を引き継ぐ（値そのものは使わず「NULL か否か」だけ見る）。
UPDATE recordings SET purge_requested = true WHERE purge_after IS NOT NULL;

DROP INDEX IF EXISTS recordings_purge_after_idx;
ALTER TABLE recordings DROP COLUMN purge_after;

-- 部分一意索引 recordings_unique_active_event の述語（deleted_at / superseded_at）
-- には触れない --- 枠が明くのはあの 2 列だけで、即時印はそれに影響しない
-- （CLAUDE.md 不変条件 13 の境界）。ここは削除 reconcile が即時対象を拾うための
-- 単なる絞り込み用の部分索引。
CREATE INDEX recordings_purge_requested_idx
    ON recordings (id) WHERE purge_requested;

-- ごみ箱腕の名前付き述語（00029）を新しい印を見る形に置き換える。
-- `<= now()` の比較を捨て、「印が立っているか」だけを見る。
CREATE OR REPLACE FUNCTION trash_deletable_recordings(grace_cutoff timestamptz)
RETURNS TABLE (recording_id bigint)
LANGUAGE sql STABLE AS $$
    SELECT r.id
    FROM recordings r
    WHERE r.deleted_at IS NOT NULL
      AND (
        r.purge_requested
        OR r.deleted_at <= grace_cutoff
      )
$$;

-- +goose Down

-- purge_after 列を先に戻してから、それを参照する関数を差し替える
-- （逆順だと「列がまだ無いのに関数が参照する」で 42703 になる）。
ALTER TABLE recordings
    ADD COLUMN purge_after timestamptz;

UPDATE recordings SET purge_after = now() WHERE purge_requested;

CREATE OR REPLACE FUNCTION trash_deletable_recordings(grace_cutoff timestamptz)
RETURNS TABLE (recording_id bigint)
LANGUAGE sql STABLE AS $$
    SELECT r.id
    FROM recordings r
    WHERE r.deleted_at IS NOT NULL
      AND (
        (r.purge_after IS NOT NULL AND r.purge_after <= now())
        OR r.deleted_at <= grace_cutoff
      )
$$;

DROP INDEX IF EXISTS recordings_purge_requested_idx;

ALTER TABLE recordings DROP COLUMN purge_requested;

CREATE INDEX recordings_purge_after_idx
    ON recordings (purge_after) WHERE purge_after IS NOT NULL;
