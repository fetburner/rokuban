-- EPGStation 移行キーは検索も部分一意索引と同じテキスト抽出式で比較する。
-- name: FindRuleByEPGStationRuleID :one
SELECT * FROM rules
WHERE metadata->'epgstation'->>'ruleId' = sqlc.arg(rule_id)::text;
