-- name: FindRuleByImportKey :one
SELECT r.* FROM rules r
JOIN rule_import_keys k ON k.rule_id = r.id
WHERE k.source = sqlc.arg(source) AND k.external_id = sqlc.arg(external_id);

-- name: InsertRuleImportKey :exec
INSERT INTO rule_import_keys (rule_id, source, external_id) VALUES ($1, $2, $3);
