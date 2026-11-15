-- +goose Up
-- 重複があれば失敗させる。運用者が残すルールを選んでから再適用する。
CREATE UNIQUE INDEX rules_epgstation_rule_id_unique
ON rules ((metadata->'epgstation'->>'ruleId'))
WHERE metadata->'epgstation'->>'ruleId' IS NOT NULL;

-- +goose Down
DROP INDEX rules_epgstation_rule_id_unique;
