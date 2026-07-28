-- +goose Up

-- M2-6: 履歴ベース重複排除（issue #24）。
--
-- ruler の dedupe 判定根拠（マッチした録画・類似度）を予約に記録し、UI で
-- 「なぜスキップされたか」を説明可能にする。base 同様、ruler が毎パス作り直す
-- 導出列（recompute 可能）であり、program_intents / program_overrides が持つ
-- 不可逆な事実とは混ざらない（CLAUDE.md 不変条件 9）。
--
-- 両方 NULL か両方非 NULL のどちらかのみ許す（「類似度はあるが根拠の録画が無い」
-- という意味を持たない行を作らない。不変条件 10）。
--
-- dedup_match_recording_id に FK を張らない。recordings(id) への
-- REFERENCES ... ON DELETE SET NULL と上記 CHECK は両立しない: FK アクションは
-- FK 側の列しか NULL にできないため、参照されている recordings 行を物理削除すると
-- (NULL, 0.87) という行ができて CHECK に違反し、DELETE 自体が中断する。
-- 外すのは FK の方にする。
--
--   * 根拠 2 列は ruler が毎パス作り直す導出値（不変条件 9）。参照先が消えても
--     次のパスで両方 NULL に戻るので、孤立は自己修復する
--   * recordings.id は GENERATED ALWAYS AS IDENTITY で再利用されないため、
--     孤立した id が別の録画を指すことは構造的に起きない（FK を外して一番怖い
--     失敗モードが無い）
--   * docs/schema.md §5 の tombstone 契約により、本番では recordings 行を
--     物理削除しない。FK が守っているのは起きない事象
--   * CHECK は「あってはいけない組み合わせを表現不可能にする」（不変条件 10）を
--     担っている。仕事をしているのはこちら
ALTER TABLE reservations
    ADD COLUMN dedup_match_recording_id bigint,
    ADD COLUMN dedup_similarity real,
    ADD CONSTRAINT reservations_dedup_evidence_check
        CHECK ((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL));

-- recordings.title への trgm GIN インデックスは張らない。
-- gin_trgm_ops が加速するのは % / <% / LIKE / 正規表現で、重複排除が使う
-- similarity() の関数呼び出しには使われない。% はルール単位の閾値ではなく GUC
-- pg_trgm.similarity_threshold を読むため、rules.dedupe_threshold と噛み合わない。
-- 前段フィルタとして使う手順は internal/ruler/dedupe.go のクエリコメントに残す。

-- +goose Down

ALTER TABLE reservations
    DROP CONSTRAINT IF EXISTS reservations_dedup_evidence_check,
    DROP COLUMN IF EXISTS dedup_similarity,
    DROP COLUMN IF EXISTS dedup_match_recording_id;
