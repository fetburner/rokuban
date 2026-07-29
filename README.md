# Rokuban（録番）

クラウドネイティブに設計された録画サーバー。

EPGStation の漸進的改善ではなく、ゼロベースで再設計する。EPGStation には Node.js IPC によるプロセス結合、インメモリ状態への依存、共有ファイルシステム前提、シングルライター前提という構造的問題があり、クラウド/k8s ホスティングと根本的に相容れない。

## 基本方針

- 録画の実行は mirakc に全面委譲。Rokuban は TS パケットを触らず、オーケストレーションと I/O に徹する
- Go 単一バイナリ。monolithic mode（自宅 1 プロセス）と distributed mode（k8s ロール分割）を最初から設計に組み込む
- ステートフル基盤は PostgreSQL ただ一つ。Redis やメッセージブローカーを置かない
- レベルトリガー・crash-only・冪等を設計原則とする

## 構成図

```
[エッジ]                       [サーバー / クラウド]
┌────────────┐    ┌─────────────────────────────────────────┐
│ mirakc      │◀──▶│ rokuban（Go、単一バイナリ）                  │
│             │    │  ├ api:        REST + SSE、UI 配信(go:embed)│
│             │    │  ├ ruler:      EPG差分→ルール評価→予約生成    │
│             │    │  ├ reconciler: 予約 ⇄ mirakc schedules 同期 │
│             │    │  ├ watcher:    mirakc SSE購読→状態反映       │
└────────────┘    │  └ streamer:   ライブ視聴 (mirakc→ffmpeg→HLS)│
   ▲               │ rokuban worker（別イメージ、0〜Nスケール）     │
   │record pull    │  └ ingest / encode / thumbnail / cleanup    │
   └───────────────├─────────────────────────────────────────┤
                   │ PostgreSQL（唯一のステートフル基盤）           │
                   │ ファイルシステム（クラウドでは CSI で S3）       │
                   └─────────────────────────────────────────┘
```

## 設計ドキュメント

| ドキュメント | 内容 |
|---|---|
| [docs/overview.md](docs/overview.md) | 全体アーキテクチャ |
| [docs/recording.md](docs/recording.md) | 録画エンジン（索引。本文は `docs/recording/`） |
| [docs/schema.md](docs/schema.md) | DB スキーマ v1（索引。本文は `docs/schema/`） |
| [docs/data.md](docs/data.md) | データ層 |
| [docs/storage.md](docs/storage.md) | メディアストレージ |
| [docs/api.md](docs/api.md) | API 設計 |
| [docs/frontend.md](docs/frontend.md) | フロントエンド |
| [docs/configuration.md](docs/configuration.md) | 設定 |
| [docs/operations.md](docs/operations.md) | 運用 |
| [docs/runbook.md](docs/runbook.md) | シャドー運用 runbook（索引。本文は `docs/runbook/`） |

大きい 3 本は索引 + 分割本文になっている。**節番号は分割前のまま**なので、コードコメントの「recording.md §3.2」等は索引の表から該当ファイルを引ける。

## 使ってみる

既存の mirakc（EPGStation と共有可）に繋いで動かせる。

```sh
cp .env.example .env
$EDITOR .env          # MIRAKC_URL と POSTGRES_PASSWORD は必須
docker compose up -d
```

`http://localhost:40773` で UI が開く。手順と確認項目は
[docs/runbook.md](docs/runbook.md) を参照。

## ステータス

**M1（録れる）実装中。** 手動予約 → 録画 → ingest → 再生の縦切りが通る。

| マイルストーン | 状態 |
|---|---|
| M0 歩く骨格 | 完了 |
| M1 録れる | 手動予約 API・reconciler・watcher・ingest・EPG プロジェクション・UI・Range 配信・メトリクスまで実装済み |
| M2 任せられる（ルール録画） | 未着手 |
| M3 置き換えられる（エンコード・削除エンジン） | 未着手 |
| M4 広げられる（ロール分割・ライブ視聴） | 未着手 |

ルール録画・エンコード・保持ポリシー・ライブ視聴はまだ入っていない。
