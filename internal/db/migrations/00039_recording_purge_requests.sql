-- +goose Up

-- 即時完全削除の要求（「ごみ箱の猶予を待たず今すぐ消してほしい」）を
-- recordings.purge_after から衛星表に出す。旧列が弱かった理由は 2 つある。
--
-- 1. 書き手が脊椎ではなかった（不変条件 13）。この要求を立てる / 取り消すのは
--    api ロール（POST /api/recordings/{id}/purge と .../restore）で、
--    recordings 本体を書く watcher / reconciler（試行の帰結の観測）ではない。
--    「purge_after が既に本体にあったから」は根拠にならない --- 既存の
--    テナントであることを根拠にすると、間借りが次の間借りを正当化する。
-- 2. 型が寿命を偽っていた。書く値は常に now()、読む側も `<= now()` でしか
--    比較しない。未来の予約削除としては一度も使っていないのに、
--    deleted_at（可逆な「ユーザーが捨てた時刻」）/ superseded_at（不可逆な
--    枠の明け渡し）/ purged_at（不可逆な完全削除の完了）と並ぶ「4 つ目の
--    時刻」に見えていた。
--
-- deleted_at / superseded_at を本体に残しているのは、部分一意索引
-- recordings_unique_active_event の述語がこの 2 列を参照し、述語が他表を
-- 参照できないから。即時要求はその述語に出ない（枠が明くのは deleted_at /
-- superseded_at だけ）ので、衛星表に出しても索引の形に影響しない。
--
-- 行の存在そのものが「即時要求」を表す（不変条件 10。circuit_breakers と同じ
-- 形）。「要求していない」を表す行は存在しえないので、false と NULL の
-- 取り違えも掃除の規則も要らない。取り消し（restore）は DELETE。
--
-- 定常運用の書き手は api だけにする。purge 完了後（purged_at）にこの行を掃除
-- する経路は作らない --- 削除 reconcile を 2 人目の書き手にすると 1 表 1 書き手
-- （不変条件 12）が崩れる。「ユーザーが即時削除を要求した」は完了後も真で
-- あり続けるので、tombstone と一緒に残す。
-- （rescue は別枠 --- 災害復旧で catalog ダンプから全表を書き戻すので、この表も
-- 他の表と同じように書く。定常運用のループではない。）
CREATE TABLE recording_purge_requests (
    -- ON DELETE CASCADE: 録画行が物理的に消えた後の「その録画を今すぐ消して
    -- ほしい」は何も主張していない（不変条件 10）。
    recording_id bigint PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    -- いつ要求されたか。**判定には使わない**（判定は行の存在だけ）。読み手は
    -- catalog の export / rescue で、往復の間この事実を落とさないために持つ。
    -- ここを `<= now()` で比較し始めたら旧列に戻る。
    requested_at timestamptz NOT NULL DEFAULT now()
);

-- 旧列に要求が付いていた行を引き継ぐ。判定に使うのは「NULL か否か」だけ
-- （旧実装が書いた値は常に now() だった）。時刻そのものは requested_at に
-- そのまま移す --- 「いつ要求されたか」としては意味があるので捨てない。
INSERT INTO recording_purge_requests (recording_id, requested_at)
SELECT id, purge_after FROM recordings WHERE purge_after IS NOT NULL;

DROP INDEX IF EXISTS recordings_purge_after_idx;
ALTER TABLE recordings DROP COLUMN purge_after;

-- 旧 recordings_purge_after_idx に相当する索引は作らない。即時要求は行そのもの
-- なので、下の EXISTS は主キー索引で引ける（行数も「猶予を待てない要求」の数
-- しかない）。旧索引は `recordings (purge_after) WHERE purge_after IS NOT NULL`
-- で、下の OR（`purge_after <= now() OR deleted_at <= grace_cutoff`）の中では
-- 単独では選ばれず BitmapOr 経由しかなかった
-- （20 万行・1/37 が purge_after 非 NULL の合成データで EXPLAIN を実測。
-- `Bitmap Heap Scan` の下が `BitmapOr` → 両索引への `Bitmap Index Scan` で、
-- 片方だけを使う実行計画にはならなかった）。

-- ごみ箱腕の名前付き述語（00029）を新しい形に置き換える。`<= now()` の比較を
-- 捨て、「要求の行があるか」だけを見る。
CREATE OR REPLACE FUNCTION trash_deletable_recordings(grace_cutoff timestamptz)
RETURNS TABLE (recording_id bigint)
LANGUAGE sql STABLE AS $$
    SELECT r.id
    FROM recordings r
    WHERE r.deleted_at IS NOT NULL
      AND (
        EXISTS (
          SELECT 1 FROM recording_purge_requests p WHERE p.recording_id = r.id
        )
        OR r.deleted_at <= grace_cutoff
      )
$$;

-- +goose Down

-- purge_after 列を先に戻してから、それを参照する関数を差し替える
-- （逆順だと「列がまだ無いのに関数が参照する」で 42703 になる）。
ALTER TABLE recordings
    ADD COLUMN purge_after timestamptz;

UPDATE recordings r
SET purge_after = p.requested_at
FROM recording_purge_requests p
WHERE p.recording_id = r.id;

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

DROP TABLE recording_purge_requests;

CREATE INDEX recordings_purge_after_idx
    ON recordings (purge_after) WHERE purge_after IS NOT NULL;
