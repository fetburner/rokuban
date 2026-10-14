# API 設計（索引）

フロントエンドとの通信を REST・SSE・メディア配信に分ける。REST の形（パス・パラメータ・enum・既定値）は OpenAPI 定義が権威である。SSE・メディア配信・SPA アセット・認証・プロキシは、分割本文が唯一の権威で OpenAPI 定義には載せない。

<details><summary>根拠</summary>

- REST の形の権威: `openapi.yaml`
- SSE・メディア配信・SPA アセット・認証・プロキシの権威（OpenAPI 定義には載せない）: `docs/api/`

</details>

## 分割先の一覧

本文は REST・SSE・メディア配信・認証とプロキシの 4 つのファイルに分けてある。**節名は分割前のままなので、コードコメントの「docs/api.md §ライブ視聴の HLS」等も、次の表から該当ファイルを引ける。**

| 節 | 内容 | ファイル |
|---|---|---|
| REST API（OpenAPI ファースト・コード生成・後方互換）/ エンドポイント設計の規約 / 機能の有効/無効は能力 API で観測する（`/api/` の未マッチを SPA に落とさない理由も同節）/ EPG の読み取り（時間窓がカーソル）/ 録画一覧: 絞り込み + キーセットページング（動的 WHERE ビルダ含む） | REST 各節の判断だけ | [api/rest.md](api/rest.md) |
| SSE (`/api/events`) / トピック / ストリーム形式 / レベルトリガーの対称性（取りこぼしを回復する定期 invalidate の周期と対象）/ 水平スケール / サーバーレスの関係 / 2 つの SSE を 1 つに集約しない | SSE の唯一の権威 | [api/sse.md](api/sse.md) |
| メディア配信 / 録画済みファイルのストリーミング / X-Accel-Redirect / ライブ視聴の HLS / 追っかけ再生 / SPA アセット配信 / サービスロゴ: ドロップ | メディア配信の索引 | [api/media.md](api/media.md) |
| 認証: アプリ内に持たない / リバースプロキシ・フレンドリー要件 / nginx リファレンス構成 / プロトコル選定の根拠 | デプロイ境界の要件 | [api/deployment.md](api/deployment.md) |

## 設計方針: REST + SSE + メディア配信の 3 本

フロントエンドとバックエンドの通信は、性質ごとに REST・SSE・メディア配信の 3 本に分ける。クライアントからサーバーは常に REST、サーバーからクライアントは常に SSE で、一方通行を 2 本並べる。双方向チャネル（WebSocket / Socket.IO）は使わない。クライアントからの継続的な入力をサーバーが受ける場面が無いためである。

| 経路 | 方向 | 用途 |
|---|---|---|
| REST API (`/api/*`) | 双方向（リクエスト/レスポンス） | CRUD・検索・操作 |
| SSE (`/api/events`) | サーバー → クライアント | 変更ヒントの配送 |
| メディア配信（HTTP GET） | クライアント → サーバー | 録画再生・ライブ HLS・サムネイル |

EPGStation は Socket.IO の双方向チャネルを使うが、Rokuban は一方通行の 2 本で足りる。

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [data.md](data.md)（データ層・NOTIFY）/ [frontend.md](frontend.md)（フロントエンド）/ [operations.md](operations.md)（ロールと分散デプロイ）/ [schema.md](schema.md)（スキーマ）
