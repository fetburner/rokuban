-- 一括書き込みのようにトリガーでは粒度が粗すぎる（または細かすぎる）場合に、
-- 書き手が明示的にヒントを送るためのもの。トピック名の一覧は
-- migrations/00005_notify.sql のコメントを参照。
-- name: NotifyTopic :exec
SELECT pg_notify('rokuban', $1);
