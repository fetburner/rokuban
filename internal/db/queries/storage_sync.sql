-- ストレージ観測の射影（issue #238 M7-5）。
--
-- storage_sync は tuner_sync / epg_services と同じ**使い捨てプロジェクション**で、
-- 真実は常にファイルシステム側にある。ただし tuner_sync とは全量置き換えの
-- 組み立て方が違う: tuner_sync は mirakc への 1 回の HTTP 呼び出しで「今回観測
-- できた全量」が初めて分かる（呼び出しが失敗/空だと全量が不明になるので、
-- クロックスキュー対策の sweep mark 方式で「今回観測した集合の外側だけ消す」
-- 必要があった）。一方 storage_sync の対象集合（'media' と、設定されていれば
-- 'scratch'）は config を読むだけで呼び出し前に確定するので、mark 方式は不要 ---
-- 「今回の対象集合」をそのまま DeleteStorageSyncExcept に渡して単純に差集合を
-- 消せる。設定から root が外れた（例: scratch_dir を空に変更した）ケースだけを
-- 掃除する。

-- name: UpsertStorageSync :exec
INSERT INTO storage_sync (
    root, path, total_bytes, used_bytes, available_bytes, observed_at
) VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (root) DO UPDATE SET
    path            = EXCLUDED.path,
    total_bytes     = EXCLUDED.total_bytes,
    used_bytes      = EXCLUDED.used_bytes,
    available_bytes = EXCLUDED.available_bytes,
    observed_at     = now();

-- 現在の config が要求する root 集合の外側を消す（config 変更で root が減った
-- ときの掃除）。$1 は config だけから決まる root 名の集合で、今回の statfs の
-- 成功/失敗には関わらない --- 統計に失敗した root もこの集合には残ったままなので
-- ここで消えない。statfs 失敗時に古い観測行を残して observed_at だけを陳腐化
-- させる（「最後にいつ観測できたか」が UI から見える鮮度情報になる）のは、
-- この DELETE の後で UpsertStorageSync を呼ばないだけで実現している
-- （storage.go の StorageSyncWorker.Work 参照）。
-- name: DeleteStorageSyncExcept :exec
DELETE FROM storage_sync WHERE NOT (root = ANY($1::text[]));

-- name: ListStorageSync :many
SELECT * FROM storage_sync
ORDER BY root;
