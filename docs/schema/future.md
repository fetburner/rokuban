> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§10 / §11）。

## 10. 後続マイグレーション

**マイグレーションの一覧は `internal/db/migrations/` が権威。** 各表の現行仕様は該当する分割ファイル（[schema.md](../schema.md) の表で引く）にある。ここに完了台帳は置かない。

## 11. 未決事項（実装前に issue で確定させる）

1. **`record_sync.recording_id` の NOT NULL 化**: 現設計は外部産 record を NULL で表現する。外部産を track しない（行を作らない）選択肢もある
