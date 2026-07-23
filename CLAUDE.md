# Rokuban 開発ガイド

## プロジェクト概要

Rokuban（録番）は EPGStation をゼロベースで再設計するクラウドネイティブ録画サーバー。録画実行は mirakc に全面委譲し、Go 単一バイナリでオーケストレーションと I/O に徹する。

## ビルド・テスト

```bash
sqlc generate           # SQL → Go コード生成
go build ./...          # ビルド
go test ./...           # テスト
golangci-lint run       # lint
```

## 設計ドキュメント

実装の根拠はすべて GitHub issue と docs/ に確定済み。実装中に設計判断を変えたくなったら実装せず issue にコメントで提起する。

### 資料マップ

| issue | 内容 | 対応 doc |
|---|---|---|
| #1 | 全体アーキテクチャ（nginx/認証/イメージ配布/サーバーレス/B-CAS） | [docs/overview.md](docs/overview.md) |
| #2 | 録画エンジン mirakc（ingest 詳細/ruler 仕様/base-overrides） | [docs/recording.md](docs/recording.md) |
| #3 | データ層（検索・ルール評価/EPG プロジェクション/DB 輻輳隔離） | [docs/data.md](docs/data.md) |
| #4 | ストレージ契約（2 階層/削除エンジン） | [docs/storage.md](docs/storage.md) |
| #5 | フロントエンド | [docs/frontend.md](docs/frontend.md) |
| #6 | 移行計画とマイルストーン | — |
| #9 | 設定 | [docs/configuration.md](docs/configuration.md) |
| #10 | EPGStation トリアージ | — |
| #11 | 懸念トラッキング | — |
| #13 | M1 タスク分解（スキーマ v1） | [docs/schema.md](docs/schema.md) |

### 不変条件

すべての実装タスクで遵守する。

1. **api ロールは mirakc に問い合わせない**。ファイルシステムにも依存しない（go:embed は可）
2. **mirakc とのやりとりは常に API**、自身のストレージとは常にファイル I/O（S3 SDK 禁止）
3. **コミット = DB 行**。ファイルの存在はコミットではない
4. **ffmpeg/ffprobe の exec は worker / streamer パッケージのみ**
5. **レベルトリガー**: イベント（SSE/NOTIFY）はヒント。真実は定期 reconcile が再取得する
6. **TS のストリーム処理をしない**（ingest 中の読み取り専用統計のみ例外）
7. **mirakc 固有の概念を永続テーブル（rules / media_assets / 履歴）に入れない**
8. **テストのないタスク完了はない**

### コーディング規約

- Go 標準プロジェクトレイアウト（`cmd/` + `internal/`）
- ログは `log/slog`
- エラーは握り潰さず `fmt.Errorf("...: %w", err)` で文脈付き wrap
- 各タスクは 1 PR 粒度。着手前に対応 issue の本文とコメントを必ず読む
