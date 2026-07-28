-- +goose Up

-- M2-10: チューナー射影（issue #21 の論点 1 → 案 B、docs/data.md §6.5）。
--
-- mirakc の GET /api/tuners の観測結果。epg_services / epg_programs と同じ
-- **使い捨てプロジェクション**で、真実は常に mirakc 側にありレベルトリガーで
-- いつでも全量再構築できる。永続資産ではないので寿命が違う（docs/schema.md §9 と同じ位置づけ）。
--
-- 存在理由は不変条件 1（api ロールは mirakc に問い合わせない）。EPGStation のように
-- 起動時に /api/tuners を叩いて in-memory に持つ形は取れない。チューナーの対応種別が
-- 必須（GR 専用チューナーに BS は載らない）なので、本数を設定に手書きする案も成立しない。
--
-- 投影しないもの: users / isFree / isUsing / command / pid（issue #21）。
-- **現在の利用者は容量から引かない** — 一時的な占有であり将来の区間の容量とは
-- 無関係で、「見えない消費者は数えない = 下界を主張する」性質と一貫する（docs/data.md §6.5）。
CREATE TABLE tuner_sync (
    site          text    NOT NULL,
    -- mirakc のレスポンスの index。PK に name ではなくこちらを使う（下記）
    tuner_index   integer NOT NULL,
    name          text    NOT NULL,
    types         text[]  NOT NULL CHECK (types <@ ARRAY['GR','BS','CS','SKY']),
    is_available  boolean NOT NULL,
    is_fault      boolean NOT NULL,
    observed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, tuner_index)
);

-- PK を (site, name) ではなく (site, tuner_index) にする。
--
-- issue #21 は投影する列に index を挙げているが（「投影する: index / name / types /
-- isAvailable / isFault」）、同 issue の DDL 案は index を持たず PK が (site, name) に
-- なっていた。この不整合は index を採る側で解消する。
--
-- 理由は失敗モードの向きが違うこと。name は運用者が付ける値で、mirakc の設定に
-- 同名のチューナーが 2 本あると upsert で 1 行に潰れ、cap(A) が 1 本少なく数えられる。
-- Σd ≤ cap(A) は破れやすくなるので**警告が過剰に出る**方向にずれ、しかも毎パス
-- 上書きされ続けるので自己修復しない。docs/data.md §6.5 は「既知の盲点はすべて
-- 警告しすぎない方向に偏っている」ことを性質として挙げており（沈黙は保証ではないが
-- 警告は信頼できる）、これはその性質を崩す。
--
-- 一方 tuner_index は設定の並びが変わると行が振り直されるが、この表は毎パス全量
-- 再構築 + スイープなので値は同じパスの中で正しくなる。恒久的な誤りが残らない。
--
-- types に cardinality > 0 の CHECK は置かない。空配列のチューナーは cap(A) に
-- 一切数えられないだけで無害であり、想定外の上流データで同期パス全体を失敗させる
-- 方が損（epg_services が未知の channel_type を持つサービスだけ捨てて続行するのと同じ規律）。
-- types の重複・順序も cap(A) が集合の交差判定なので影響しないため、
-- rules.encode_profiles のような正規集合チェックは要らない。

-- +goose Down

DROP TABLE IF EXISTS tuner_sync;
