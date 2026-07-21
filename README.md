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
│ (site A)    │    │  ├ api:        REST + SSE、UI 配信(go:embed)│
├────────────┤    │  ├ ruler:      EPG差分→ルール評価→予約生成    │
│ mirakc      │◀──▶│  ├ reconciler: 予約 ⇄ mirakc schedules 同期 │
│ (site B)    │    │  ├ watcher:    mirakc SSE購読→状態反映       │
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
| [docs/recording.md](docs/recording.md) | 録画エンジン |
| [docs/data.md](docs/data.md) | データ層 |
| [docs/storage.md](docs/storage.md) | メディアストレージ |
| [docs/api.md](docs/api.md) | API 設計 |
| [docs/frontend.md](docs/frontend.md) | フロントエンド |
| [docs/configuration.md](docs/configuration.md) | 設定 |
| [docs/operations.md](docs/operations.md) | 運用 |

## ステータス

設計フェーズ。実装はまだ開始していない。
