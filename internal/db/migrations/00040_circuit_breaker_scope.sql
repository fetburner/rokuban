-- +goose Up

ALTER TABLE circuit_breakers RENAME COLUMN site TO scope;

-- +goose Down

-- スキーマだけ戻す。旧版へ戻すと、置き場名の範囲の delete_reconcile ラッチは旧版から見えなくなる。

ALTER TABLE circuit_breakers RENAME COLUMN scope TO site;
