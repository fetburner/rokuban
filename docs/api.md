# API 設計（索引）

**openapi.yaml に載っている API（`/api/*` の JSON REST）を触る → openapi.yaml + [api/rest.md](api/rest.md) の該当節（判断のみ。パス・パラメータ・enum・既定値は openapi.yaml が権威）。**
**SSE・メディア配信（録画バイト配信・ライブ HLS・サムネイル）・SPA アセット・認証・プロキシを触る → `docs/api/` が唯一の権威**（openapi.yaml には載せない方針。`internal/api/router.go` / `internal/streamer/streamer.go` のコメント参照）。

## 設計方針: REST + SSE + メディア配信の 3 本

フロントエンド・バックエンド間の通信経路は性質ごとに 3 本に分離する。双方向チャネル（WebSocket / Socket.IO）は使わない。

| 経路 | 方向 | 用途 |
|---|---|---|
| REST API (`/api/*`) | 双方向（リクエスト/レスポンス） | CRUD・検索・操作 |
| SSE (`/api/events`) | サーバー → クライアント | 変更ヒントの配送 |
| メディア配信（HTTP GET） | クライアント → サーバー | 録画再生・ライブ HLS・サムネイル |

クライアント → サーバーは常に REST、サーバー → クライアントは常に SSE。一方通行 x 2 の構成であり、EPGStation の Socket.IO のような双方向チャネルが必要になる場面（クライアントからの継続的入力をサーバーが受ける）がこのアプリには存在しない。

**本文は `docs/api/` に分割してある。節名は分割前のまま**なので、コードコメントの「docs/api.md §ライブ視聴の HLS」等はこの表で該当ファイルを引ける。

| 節 | 内容 | ファイル |
|---|---|---|
| REST API（OpenAPI ファースト・コード生成・後方互換）/ エンドポイント設計の規約 / **機能の有効/無効は能力 API で観測する**（`/api/` の未マッチを SPA に落とさない理由も同節）/ EPG の読み取り（**時間窓がカーソル**）/ **録画一覧: 絞り込み + キーセットページング**（動的 WHERE ビルダ含む） | REST 各節の**判断だけ** | [api/rest.md](api/rest.md) |
| **SSE (`/api/events`)** --- トピック / ストリーム形式 / レベルトリガーの対称性（**取りこぼしを回復する定期 invalidate の周期と対象**）/ 水平スケール / サーバーレスの関係 / 2 つの SSE を 1 つに集約しない | SSE の唯一の権威 | [api/sse.md](api/sse.md) |
| **メディア配信** --- 録画済みファイルのストリーミング / X-Accel-Redirect / **ライブ視聴の HLS** / SPA アセット配信 / サービスロゴ: ドロップ | メディア配信の唯一の権威 | [api/media.md](api/media.md) |
| **認証: アプリ内に持たない** / **リバースプロキシ・フレンドリー要件** / nginx リファレンス構成 / プロトコル選定の根拠 | デプロイ境界の要件 | [api/deployment.md](api/deployment.md) |

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [data.md](data.md)（データ層・NOTIFY）/ [frontend.md](frontend.md)（フロントエンド）/ [operations.md](operations.md)（ロールと分散デプロイ）/ [schema.md](schema.md)（スキーマ）
