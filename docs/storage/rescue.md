> [storage.md](../storage.md) §8 の一部。索引から辿る

## 8. catalog エクスポートと rescue（災害復旧）

### 保護対象の仕分け

失うと痛いデータを仕分けすると、EPG プロジェクションは mirakc から再構築可能、ジョブキューは一時的で、**保護対象は「ルール・録画履歴・media_assets・ドロップ統計・tombstone・手動オーバーライド」のみ（数 MB）**。

### catalog エクスポート

- worker の定期ジョブが、このコアデータを JSON で**メディアストレージ自身の `catalog/` 配下に書き出す**（日次 + 世代保持）。メディアが生き残る障害では catalog も一緒に生き残る
- pg_dump に依存しない（distroless イメージに postgres クライアント不要）アプリレベルのエクスポートで、後述 rescue の入力形式を兼ねる
- フル忠実度が欲しい場合の日次 pg_dump 構成例はドキュメントに記載（推奨・非必須）

### rescue --- ストレージスキャンからの再構築

`rokuban rescue`: ストレージを走査し、

- `catalog/` があれば照合してフルメタデータ（番組情報・ドロップ統計・保持ポリシー）ごと復元
- catalog が無ければ TS / M2TS を `original`、MP4 / MKV / WebM を `encoded`
  （`profile = rescue-<拡張子>`）として、現在位置のまま登録する。タイトルと時刻は
  ファイル名 / mtime、番組・サービス情報は「metadata unavailable」と明示した素の録画になる
- `catalog/` 自身、未知拡張子、symlink は走査対象にしない。ファイル本体はコピーも変更もしない
- 同じ相対パスは安定した合成番組 identity へ写し、再実行しても録画・asset が増殖しない

登録トランザクションは `internal/inplace` に置き、`rokuban import epgstation` も同じ
in-place 登録機構を使う。DB 行のコミットが公開であるというストレージ契約は通常 ingest と同じ。

### Postgres 運用

世帯スケールでは catalog（+ 任意で日次 pg_dump）で十分。WAL アーカイビングは過剰。
