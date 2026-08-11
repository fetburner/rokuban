> [docs/api.md](../api.md)（索引）の分割本文。SSE（`/api/events`）の仕様はここが唯一の権威（openapi.yaml には載せない）。

## SSE (`/api/events`) --- ヒント配送、状態の真実は REST から再取得

サーバー → クライアントの一方向プッシュ。役割は**「どのデータが変わったか」のヒント配送だけ**。

**実装は `internal/notifier` ロールの所有物。** api ロールは mirakc にもファイルシステムにも
依存しない（不変条件 1）のと同じ理由で、長寿命接続である SSE も持たない。api は desired
state を Postgres に書くだけの純粋なリクエスト/レスポンス層に留め、Postgres を LISTEN して
ブラウザへ配り直すだけの小さな常駐プロセスを notifier として分けている。
monolith（`--all`）では `internal/api.RouterConfig.Mounter` 経由で streamer と同じ
リスナーに相乗りする（`api.Mounters` で束ねる）。ロールを分けて起動したときは、notifier
ロールが自分の HTTP サーバーで同じパスを serve する。**api ロール単独では `/api/events` は
登録されず 404 になる。**

### 実装

**OpenAPI には載せない。** 長寿命ストリームは OpenAPI のリクエスト/レスポンスモデルに乗らず、
コード生成させると応答をバッファリングする形になってしまう。クライアントも生成フックではなく
`EventSource` を直接使うため、生成物から得るものがない。ルーターに直接登録し、仕様はここに置く。

**トピック**は表ではなくクライアントの関心事に揃える。1 トピックが 1 つのクエリキー接頭辞に対応する。

| トピック | 発火元 | クライアントが invalidate するもの |
|---|---|---|
| `reservations` | `reservations` の行トリガー | 予約一覧・予約詳細（容量超過の導出値も一緒に） |
| `recordings` | `recordings` / `media_assets` の行トリガー | 録画一覧（サイズ・ドロップ統計も含む） |
| `epg` | EPG 同期ジョブが明示的に `pg_notify` | 番組リスト・サービス一覧 |
| `breakers` | `circuit_breakers` の行トリガー | ブレーカー一覧（バナー） |
| `rules` | `rules` の行トリガー | （現在クライアントは購読していない。notifier は選別せず全トピックを転送する — `web/src/lib/events.ts` の `topicQueryKeys` が購読の一覧） |

**通知の出し方はテーブルの書き込み量で分ける。**

- **行トリガー**（`reservations` / `recordings` / `media_assets`）: 書き手が通知を忘れる種類のバグを
  構造的に消せる。これらは書き込み量が小さい
- **明示的な `pg_notify`**（`epg_programs`）: 全量 upsert で 1 パス数千行になるためトリガーでは
  細かすぎる。同期ジョブがパス完了時に 1 回だけ送る。EPG が実際に更新されたときだけ通知できる利点もある

**重複・空振りの通知は起こりうる**（`updated_at` だけが変わる UPDATE 等）。ヒントなので害はないが、
notifier 側の `EventHub` がトピックごとに 200ms の窓で合流させて量を抑える。

**ストリームの形式:**

```
retry: 3000

event: recordings
data: {"topic":"recordings"}

: ping
```

- `data` は必須。`EventSource` は data が空のイベントを dispatch しない
- 25 秒ごとにコメント行（`: ping`）を送る。リバースプロキシ・CDN のアイドルタイムアウト対策
- `X-Accel-Buffering: no` を付ける。nginx がイベントを溜め込むのを防ぐ
- クライアントのバッファが埋まっていたら通知を**捨てる**。詰まった 1 クライアントのために
  全体を止めない。落とした通知は stale-time 経過後の再取得で回復する
- LISTEN コネクションが切れたら 5 秒後に再接続する。切断中の変更も同様に回復する

### レベルトリガーの対称性

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計（参照: [data.md](../data.md)）。フロントも同じ形にする:

1. SSE イベントを受信したら、該当クエリの `invalidateQueries` を実行
2. 真実は常に REST から再取得
3. **プッシュの中身を直接信頼して画面状態を書き換えることはしない**

SSE の取りこぼしは stale-time 経過後の再取得で自然回復する。プッシュデータを信頼して手元状態を書き換える設計（Socket.IO 時代の EPGStation）より壊れ方が大幅に単純になる。

### 水平スケール

notifier ロールを複数レプリカにしても、各レプリカが Postgres の NOTIFY を購読して配るだけなので **Redis アダプタ等の追加基盤は不要**。notifier は**シングルトンではない**（`cmd/rokuban/server.go` の `singletonRoles` に含まれない） --- 各レプリカが独立に LISTEN し、自分にぶら下がる SSE クライアントにだけ配る。レプリカ間で配送を調停する必要が構造的にない（参照: [data.md](../data.md) §3）。

### SSE とサーバーレスの関係

SSE は長寿命接続でありサーバーレスとは相性が悪い。ハイブリッド構成では CDN のパスルーティングで `/api/events` だけ **notifier ロール**へ振り分ける（api ロールには SSE エンドポイント自体が存在しない）。自宅ダウン中は invalidation が止まるだけで読み書きは動く --- きれいな劣化（参照: [overview.md](../overview.md) のハイブリッド構成）。

### 2 つの SSE を 1 つに集約しない

Rokuban には長寿命接続が 2 つある --- notifier がブラウザへ送る `/api/events` と、watcher が mirakc から受ける mirakc 側の `/events` 購読。**どちらも「長寿命接続を張り続ける」という機構は同じだが、関心事は無関係なので 1 つのプロセス/抽象にまとめない。**

| | 相手 | 向き | 落ちたときの影響 |
|---|---|---|---|
| notifier の `/api/events` | ブラウザ | 送る | UI の自動更新が止まる（読み書き自体は動く） |
| watcher の mirakc `/events` | mirakc | 受ける | ingest の投入が遅れる（`record_sweep` の定期突き合わせが拾う） |

機構でまとめると、mirakc が落ちたときにブラウザ配信も巻き込まれるといった不要な結合が生まれる。小規模構成で常駐プロセスを減らしたいという動機は monolith（`--all`）が既に満たしており、抽象を共有する理由にはならない。

## 経緯と失敗事例

- SSE の初期実装は M1-7 で api ロール内（`internal/api/events.go` の `EventHub`）に
  置かれ、M2-19（issue #24）で notifier ロールへ分離した。ロールを分ける判断と
  「2 つの SSE を集約しない」判断は issue #25 §4
