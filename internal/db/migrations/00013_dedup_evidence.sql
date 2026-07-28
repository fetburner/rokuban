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
ALTER TABLE reservations
    ADD COLUMN dedup_match_recording_id bigint REFERENCES recordings (id) ON DELETE SET NULL,
    ADD COLUMN dedup_similarity real,
    ADD CONSTRAINT reservations_dedup_evidence_check
        CHECK ((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL));

-- 履歴ベース重複排除の類似度検索（pg_trgm の similarity()）を GIN で加速する。
-- pg_trgm 自体は 00006_rules.sql で有効化済み。
CREATE INDEX recordings_title_trgm ON recordings USING gin (title gin_trgm_ops);

-- +goose Down

DROP INDEX IF EXISTS recordings_title_trgm;

ALTER TABLE reservations
    DROP CONSTRAINT IF EXISTS reservations_dedup_evidence_check,
    DROP COLUMN IF EXISTS dedup_similarity,
    DROP COLUMN IF EXISTS dedup_match_recording_id;
