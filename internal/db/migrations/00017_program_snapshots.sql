-- +goose Up

-- Phase 1: 予約モデルの歪みの解消（#27 / #28 / #30）。
--
-- `reservations` は ruler の導出出力と API リソースの両方を兼ねていたため、
-- 「導出できないもの」が導出行に溜まり続けていた。修正はいつも同じ手 ---
-- 導出できないものを `(site, program_id)` の別表に引き剥がす --- で、
-- `program_intents`（#18）と `program_overrides`（M2-4）で 2 回済んでいる。
-- このマイグレーションは残りの 2 回を 1 度にやる。
--
--   1. 番組の事実（放送の寿命）      → program_snapshots へ抽出        （#27）
--   2. 不可逆な観測（orphaned）      → state 列から orphaned_at へ分離  （#28 / #30）
--
-- 完了後、`reservations` に残るのは ruler の 1 パスの出力だけになる:
-- (id, site, program_id, rule_id, base, dedup 根拠 2 列, timestamps)。
-- 1 表 = 1 つの書き手 = 1 つの寿命（CLAUDE.md 不変条件 12）。
--
-- 導出テーブルなので Down は片道を許容する（`00008` / `00010` / `00012` の前例）。
-- 何が復元できないかは Down 側のコメントに書く。

-- ---------------------------------------------------------------------------
-- 1. program_snapshots（#27）
-- ---------------------------------------------------------------------------
--
-- 同じ意味・同じ出所（書き込み時点の epg_programs ⋈ epg_services）の組が
-- reservations / program_intents / program_overrides の 3 箇所に複製されており、
-- しかも ruler が reservations 側だけを毎パス射影から更新していたため
-- **既にドリフトしていた**（同じ番組について 2 つの異なる開始時刻が保存され、
-- GC の判定が表ごとに違う時刻を使っていた）。
--
-- 目的は 3 表で同一である: EPG プロジェクションが使い捨てなので、番組が射影から
-- 消えても GC 判定と UI 表示が成立し続けるようにすること
-- （docs/schema.md §3「射影にある間は更新、消えたら凍結」）。目的が同じなら表も 1 つ。
--
-- **値の出所は EPG 射影ただ 1 つ**（#27 の決定）。書き手は api（作成時）と
-- ruler（毎パス）の 2 人だが、両者とも射影から引くので値の権威は割れない。
-- 移行前の api は title / 開始時刻 / 尺を**リクエストボディから**受け取っており、
-- それが GC の比較対象になっていた（クライアントが古い番組表を握っていると
-- ユーザーの skip 意図が早すぎる GC で消える）。この経路は同 PR で閉じる。
CREATE TABLE program_snapshots (
    site        text        NOT NULL,
    program_id  bigint      NOT NULL,
    title       text        NOT NULL DEFAULT '',
    start_at    timestamptz NOT NULL,
    duration_ms bigint      NOT NULL,
    -- チャンネル識別。移行前の reservations と同じく nullable
    -- （00009 の backfill でも埋められなかった残骸がありうる。
    -- reconciler は service_id が NULL の予約を意図的に schedule 化しない）。
    --
    -- 追記（issue #101。00026）: この nullable 理由は行の寿命
    -- （放送 + epg.retention_grace）と書き込み経路（INNER JOIN のみ）により
    -- 失効し、00026 でこの 4 列を含む 6 列すべてを NOT NULL 化した。
    network_id   integer,
    service_id   integer,
    channel_type text,
    channel      text,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id),
    -- 00009 の reservations_channel_type_check をそのまま引き継ぐ。
    CONSTRAINT program_snapshots_channel_type_check
        CHECK (channel_type IS NULL OR channel_type IN ('GR', 'BS', 'CS', 'SKY'))
);

-- backfill。reservations を最優先にする --- 3 表のうち ruler が毎パス射影から
-- 更新しているのはここだけで、他の 2 表は作成時のまま固まっている（それが
-- #27 の言うドリフトの中身）。つまり reservations の値が最も新しい。
INSERT INTO program_snapshots (
    site, program_id, title, start_at, duration_ms,
    network_id, service_id, channel_type, channel
)
SELECT site, program_id, title, program_start_at, program_duration_ms,
       network_id, service_id, channel_type, channel
FROM reservations
ON CONFLICT (site, program_id) DO NOTHING;

-- 予約行を持たない意図・上書き（intent{skip} だけの番組など）を拾う。
-- title / チャンネルは持っていないので既定値のまま。次の ruler パスが
-- 射影から更新する（射影から消えていれば凍結されたまま = 移行前と同じ）。
INSERT INTO program_snapshots (site, program_id, start_at, duration_ms)
SELECT site, program_id, program_start_at, program_duration_ms
FROM program_overrides
ON CONFLICT (site, program_id) DO NOTHING;

INSERT INTO program_snapshots (site, program_id, start_at, duration_ms)
SELECT site, program_id, program_start_at, program_duration_ms
FROM program_intents
ON CONFLICT (site, program_id) DO NOTHING;

-- FK は ON DELETE CASCADE（#27 の決定）。
--
-- GC が 3 本の DELETE から 1 本になるのが #27 の主要な利益で、それは CASCADE で
-- しか得られない。向きが直感に反して見える（番組情報が消えたら意図が消える）が、
-- GC の意味論「意図の寿命を放送の寿命に揃える」とは一致している。
--
-- **program_snapshots からの DELETE 経路は GC 1 本に限定すること。**
-- 他の場所から消せると意図を巻き添えにする。特に「参照が 1 つも無い
-- スナップショット行を掃除する」規則を足してはならない --- 掃除しないなら
-- 害はない（GC が拾う）が、掃除規則は intent の作成とレースする
-- （ruler の導出削除が並行して作られた手動予約を消したのと同じ形。#29）。
ALTER TABLE reservations
    ADD CONSTRAINT reservations_program_fkey
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
    ON DELETE CASCADE;

ALTER TABLE program_intents
    ADD CONSTRAINT program_intents_program_fkey
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
    ON DELETE CASCADE;

ALTER TABLE program_overrides
    ADD CONSTRAINT program_overrides_program_fkey
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
    ON DELETE CASCADE;

-- 重複していた列を落とす。title も落とす（#27 の決定）--- 更新規則も寿命も
-- 他のスナップショット列と同じで、残すと ruler が 2 箇所を更新することになり
-- 同じドリフトを再生産する。JOIN は PK 同士なので実質無害。
ALTER TABLE reservations
    DROP CONSTRAINT IF EXISTS reservations_channel_type_check,
    DROP COLUMN title,
    DROP COLUMN program_start_at,
    DROP COLUMN program_duration_ms,
    DROP COLUMN network_id,
    DROP COLUMN service_id,
    DROP COLUMN channel_type,
    DROP COLUMN channel;

ALTER TABLE program_intents
    DROP COLUMN program_start_at,
    DROP COLUMN program_duration_ms;

ALTER TABLE program_overrides
    DROP COLUMN program_start_at,
    DROP COLUMN program_duration_ms;

-- ---------------------------------------------------------------------------
-- 2. state → orphaned_at（#28 / #30）
-- ---------------------------------------------------------------------------
--
-- `state` は 2 種類の情報を持っていた。
--
--   orphaned          : 番組終了後に schedule が観測されなかったという
--                       **独立した観測事実**。再取得できないので列に持つ必要がある
--   active / detached : (rule_id, base) から**導出できる値**
--                       （detached ⟺ rule_id IS NULL AND base IS NOT NULL）
--
-- 同居が実際に 3 つのバグを産んだ:
--
--   * 手動予約が黙って録画されなくなる（listDesired が state='active' で絞っていた。M2-4 で修正）
--   * ルールを削除した経路で detached にならない（ruler の CASE が前パスの
--     rule_id を見ており、FK の ON DELETE SET NULL が先に走るため。#30 症状 1。**未修正**）
--   * detached の予約が永久に orphaned にならない（MarkReservationOrphaned の
--     WHERE に state='active' が残っていた。#30 症状 2。対症で修正済み）
--
-- timestamptz にするのは、2 値（NULL / 非 NULL）なら将来また導出値を混ぜる余地が
-- 構造的に無いため（text + CHECK だと `state = 'detached'` を条件に書きたくなる
-- 瞬間が再来する）。副産物として「いつ orphan と判定したか」が取れ、
-- docs/schema.md §3「録れなかったを説明可能にする」の材料が増える。
ALTER TABLE reservations ADD COLUMN orphaned_at timestamptz;

-- 判定時刻そのものは記録されていないので updated_at で代用する。
-- MarkReservationOrphaned が `state = 'orphaned', updated_at = now()` を
-- 同時に書いていたため、orphaned 化以降に他の更新が無い行では正確な値になる。
UPDATE reservations SET orphaned_at = updated_at WHERE state = 'orphaned';

-- state 列の索引（00002）は列と一緒に落ちる。orphaned_at には索引を張らない ---
-- 予約集合は ruler の GC でローリングウィンドウに有界なので、
-- `orphaned_at IS NULL` の絞り込みに索引が要る規模にならない。
ALTER TABLE reservations DROP COLUMN state;

-- +goose Down

-- **片道であることを明示的に許容する**（導出テーブルなので再構築できる。
-- CLAUDE.md 不変条件 11「導出表は churn がほぼ無害」、`00008` / `00010` / `00012`
-- と同じ性質）。復元できないものは次の 2 つ。
--
--   * `active` / `detached` の区別。orphaned でない行はすべて 'active' に戻る。
--     次の ruler パスが (rule_id, base) から作り直すので実害はない
--   * program_snapshots に「予約行を持たない番組」として入った行の title /
--     チャンネル。元々どの表にも無かった情報なので、失われるものは無い

ALTER TABLE reservations ADD COLUMN state text NOT NULL DEFAULT 'active';
UPDATE reservations SET state = 'orphaned' WHERE orphaned_at IS NOT NULL;
ALTER TABLE reservations
    ADD CONSTRAINT reservations_state_check
        CHECK (state IN ('active', 'detached', 'orphaned')),
    DROP COLUMN orphaned_at;
CREATE INDEX ON reservations (state);

ALTER TABLE reservations
    DROP CONSTRAINT IF EXISTS reservations_program_fkey,
    ADD COLUMN title               text NOT NULL DEFAULT '',
    ADD COLUMN program_start_at    timestamptz,
    ADD COLUMN program_duration_ms bigint,
    ADD COLUMN network_id          integer,
    ADD COLUMN service_id          integer,
    ADD COLUMN channel_type        text,
    ADD COLUMN channel             text;

ALTER TABLE program_intents
    DROP CONSTRAINT IF EXISTS program_intents_program_fkey,
    ADD COLUMN program_start_at    timestamptz,
    ADD COLUMN program_duration_ms bigint;

ALTER TABLE program_overrides
    DROP CONSTRAINT IF EXISTS program_overrides_program_fkey,
    ADD COLUMN program_start_at    timestamptz,
    ADD COLUMN program_duration_ms bigint;

UPDATE reservations r SET
    title               = s.title,
    program_start_at    = s.start_at,
    program_duration_ms = s.duration_ms,
    network_id          = s.network_id,
    service_id          = s.service_id,
    channel_type        = s.channel_type,
    channel             = s.channel
FROM program_snapshots s
WHERE s.site = r.site AND s.program_id = r.program_id;

UPDATE program_intents i SET
    program_start_at    = s.start_at,
    program_duration_ms = s.duration_ms
FROM program_snapshots s
WHERE s.site = i.site AND s.program_id = i.program_id;

UPDATE program_overrides o SET
    program_start_at    = s.start_at,
    program_duration_ms = s.duration_ms
FROM program_snapshots s
WHERE s.site = o.site AND s.program_id = o.program_id;

-- NOT NULL に戻す（Up 前の定義に揃える）。FK があったので取りこぼしは無い。
ALTER TABLE reservations
    ALTER COLUMN program_start_at SET NOT NULL,
    ALTER COLUMN program_duration_ms SET NOT NULL,
    ADD CONSTRAINT reservations_channel_type_check
        CHECK (channel_type IS NULL OR channel_type IN ('GR', 'BS', 'CS', 'SKY'));
ALTER TABLE program_intents
    ALTER COLUMN program_start_at SET NOT NULL,
    ALTER COLUMN program_duration_ms SET NOT NULL;
ALTER TABLE program_overrides
    ALTER COLUMN program_start_at SET NOT NULL,
    ALTER COLUMN program_duration_ms SET NOT NULL;

DROP TABLE program_snapshots;
