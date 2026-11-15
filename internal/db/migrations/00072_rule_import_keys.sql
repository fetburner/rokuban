-- +goose Up
CREATE TABLE rule_import_keys (
    rule_id bigint PRIMARY KEY REFERENCES rules(id) ON DELETE CASCADE,
    source text NOT NULL CHECK (source IN ('epgstation')),
    external_id text NOT NULL CHECK (external_id <> ''),
    UNIQUE (source, external_id)
);

-- 以前の取り込み検索が受け取れた int64 の整数値だけを移す。
-- 重複は UNIQUE で拒否し、全体をロールバックする。
INSERT INTO rule_import_keys (rule_id, source, external_id)
SELECT id, 'epgstation', (external_id::bigint)::text
FROM (
    SELECT id,
           CASE WHEN jsonb_typeof((metadata -> 'epgstation') -> 'ruleId') = 'number'
                THEN ((metadata -> 'epgstation') ->> 'ruleId')::numeric
           END AS external_id
    FROM rules
) AS legacy_keys
WHERE external_id = trunc(external_id)
  AND external_id BETWEEN (-9223372036854775808)::numeric AND 9223372036854775807::numeric;

UPDATE rules SET metadata =
    CASE WHEN ((metadata -> 'epgstation') - 'ruleId') = '{}'::jsonb
         THEN (metadata - 'epgstation')
         ELSE jsonb_set(metadata, '{epgstation}', ((metadata -> 'epgstation') - 'ruleId'))
    END
WHERE id IN (SELECT rule_id FROM rule_import_keys);

-- +goose Down
UPDATE rules r SET metadata = jsonb_set(
    r.metadata, '{epgstation}',
    ((CASE WHEN jsonb_typeof(r.metadata -> 'epgstation') = 'object'
           THEN (r.metadata -> 'epgstation') ELSE '{}'::jsonb END)
        || jsonb_build_object('ruleId', (k.external_id)::bigint))
)
FROM rule_import_keys k WHERE k.rule_id = r.id AND k.source = 'epgstation';

DROP TABLE rule_import_keys;
