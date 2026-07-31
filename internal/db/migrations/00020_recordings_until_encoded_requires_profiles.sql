-- +goose Up

-- issue #104（M3-15）: keep_original='until_encoded' かつ encode_profiles='{}'
-- の録画は、削除 reconcile の派生物完備判定
-- （ListUntilEncodedOriginalsToDelete の NOT EXISTS(unnest(encode_profiles) ...)）
-- が空配列で恒真になるため、サムネイルが 1 枚あるだけで唯一のコピーである
-- 原本が物理削除される（docs/storage.md §6「唯一のコピーを消すパスがない」への
-- 違反）。delete_reconcile.sql 側に cardinality(encode_profiles) > 0 を足した
-- （load-bearing な修正）のに加え、この組み合わせ自体を recordings でも
-- 表現不可能にする（CLAUDE.md 不変条件 10）。rules（00006）・program_overrides
-- 検証（program_overrides.go）と同じ規律を、recordings への書き手が増えても
-- 漏れない最後の砦として揃える。
--
-- 既定値は keep_original='always' / encode_profiles='{}'（00002_schema_v1.sql）
-- なので、この CHECK は「keep_original を until_encoded にするならプロファイルを
-- 添える」という追加要求だけを課す。既存行が違反しうるとしたら
-- until_encoded への書き手（現状は無し。#103 で新設予定）がプロファイル無しで
-- 書いた場合だけなので、00016 の dedupe 同様に安全側（always に戻す）へ
-- 倒してから CHECK を足す。
UPDATE recordings
SET keep_original = 'always'
WHERE keep_original = 'until_encoded'
  AND cardinality(encode_profiles) = 0;

ALTER TABLE recordings
    ADD CONSTRAINT recordings_until_encoded_requires_profiles
        CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0);

-- +goose Down

ALTER TABLE recordings
    DROP CONSTRAINT IF EXISTS recordings_until_encoded_requires_profiles;
