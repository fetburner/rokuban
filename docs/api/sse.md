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
| `recordings` | `recordings` / `media_assets` の行トリガー | 録画一覧・録画詳細（サイズ・ドロップ統計も含む） |
| `epg` | EPG 同期ジョブが明示的に `pg_notify` | 番組表グリッド・番組リスト・サービス一覧 |
| `breakers` | `circuit_breakers` の行トリガー | ブレーカー一覧（バナー） |
| `rules` | `rules` の行トリガー | （現在クライアントは購読していない。notifier は選別せず全トピックを転送する — `web/src/lib/events.ts` の `queryGroups` が購読の一覧） |

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
  全体を止めない。落とした通知はクライアント側の定期 invalidate で回復する（下記
  「レベルトリガーの対称性」）
- LISTEN コネクションが切れたら 5 秒後に再接続する。切断中の変更も同様に回復する

### レベルトリガーの対称性

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計（参照: [data.md](../data.md)）。フロントも同じ形にする:

1. SSE イベントを受信したら、該当クエリの `invalidateQueries` を実行
2. 真実は常に REST から再取得
3. **プッシュの中身を直接信頼して画面状態を書き換えることはしない**

プッシュデータを信頼して手元状態を書き換える設計（Socket.IO 時代の EPGStation）より壊れ方が大幅に単純になる。

#### 取りこぼしを回復するのは定期 invalidate（`staleTime` ではない）

`staleTime` は「データを stale と判定する期限」であって、期限に達したら再取得を起こす
周期タイマーではない。したがって「通知が捨てられた・接続は生きたまま・再 mount も
window focus も別操作も起きない」画面は、`staleTime` だけでは古い表示が無期限に残る。
回復はクライアント側（`web/src/lib/events.ts`）が張るタイマーが担う。

**グループの寿命・変更頻度・応答の大きさで周期を分ける。**

| グループ | クエリキー接頭辞 | SSE 無しでの収束の上限 | 周期を決めた理由 |
|---|---|---|---|
| 運用状態 | `/api/reservations` `/api/capacity/overages` `/api/recordings` `/api/breakers` | 60 秒 | 応答が小さく、変化が速い |
| EPG | `/api/sites/`（番組表グリッド・サービス一覧・重なり）+ `/api/programs`（番組リストの手書きキー） | 10 分 | 数十チャンネル x 24 時間の大きな時間窓を 1 回で取る。EPG 同期ジョブ自体が分オーダーでしか動かないので、短周期で回しても得るものが無い |

**キーの先頭要素がグループの所属を決める。** 接頭辞の照合はクエリキーの先頭要素に対する
前方一致なので、**URL が `/api/sites/...` でも先頭要素が `'/api/reservations'` なら
運用状態グループに入る**（逆も同じ）。ここが所属を決める唯一の場所であり、生成キーを
そのまま使うと意図と食い違うことがある。フロント側の規律は 2 つ。

- **単体ページのキーは先頭要素を一覧と揃える**（`['/api/reservations', 'detail', site, programId]` /
  `['/api/recordings', 'detail', id]`）。orval の生成キーは URL 1 要素なので、
  一覧側の mutater の invalidate にも SSE トピックにも掛からない
- **URL をキーにできないクエリは接頭辞をここに登録する**。番組リスト
  （`pages/programs.tsx` の `useInfiniteQuery`）はページの形が「取得した半開区間」なので
  手書きの `['/api/programs', 'infinite', ...]` を使う

どちらも守らないと、そのクエリは SSE のトピックでも定期 invalidate でも取り直されない
（番組リスト）か、意図と違うグループの周期で収束する（予約詳細が EPG の 10 分側に
落ちていた）。**実例 2 件はどちらも下記「経緯と失敗事例」。**

- **背面タブでは投げない。** 復帰時は `refetchOnWindowFocus`（`main.tsx` の QueryClient 既定）が拾う
- **SSE が再接続したとき（切断を観測した後の `open`）は全グループを invalidate する。** 切断中に
  飛んだ通知は再送されないので、周期を待たずに取り直す。初回接続では invalidate しない
  （各クエリの mount 時の取得と二重になるだけ）
- **再接続時の invalidate だけでは足りない。** 接続を切らずに個別の通知だけ落としたケースを
  回復できるのは定期 invalidate の方だけ
- 上の接頭辞に載っていないクエリ（`/api/storage` `/api/tuners` `/api/rules` `/api/version` 等）は
  この定期経路に乗っていない。mount と window focus でのみ取り直す

周期と経路は 2 段で測っている。

- **jsdom**（`web/src/lib/events.test.tsx`）: 偽タイマーを進めて**再取得の回数を数える**
  （「SSE が来なくても運用状態のクエリは 60 秒周期で取り直す」「EPG は運用状態より長い周期で
  しか取り直さない」「再接続したら切断中の変更を全グループ取り直す」「背面タブでは定期取得を
  投げず、前面に戻ると再開する」「epg のイベントで番組リスト（手書きのクエリキー）も取り直す」）
- **実ブラウザ**（`web/e2e/sse-refresh.mjs`。Chromium + ビルド済み bundle + `page.clock`）:
  SSE を張ったまま 1 通も送らず、`/api/**` のリクエスト数を数える。実測は
  60 秒で `/api/reservations` `/api/breakers` が 1 → 2、10 分で
  `/api/sites/{site}/programs` `/api/sites/{site}/services` が 1 → 2、
  `/api/reservations` が 11（10 分ぶんの 10 回 + 初回）

**実 notifier に対する収束時間と、切断時に実際の `EventSource` が `error` → `open` を
この順で出すことは未計測**（仕様上はそうなる。再接続の経路を確かめているのはスタブの方だけ）。

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

- **「取りこぼしは stale-time 経過後の再取得で自然回復する」と docs 3 箇所（ここ・
  frontend/stack.md・frontend/shell.md）に書いていたが、そのような再取得はどこにも
  存在しなかった**（issue #181）。`staleTime` は判定の期限であってタイマーではない。
  レベルトリガー設計の「イベントはヒント、真実は定期再取得」のうち**定期の側が
  フロントに無い**まま、docs だけが在ることにしていた。定期 invalidate と再接続時の
  invalidate を足して埋めた
- **`epg` トピックが番組リストに一度も届いていなかった**（同 issue #181 で発見）。
  接頭辞は `/api/sites/` だけだったが、番組リストのキーは手書きの
  `['/api/programs', 'infinite', ...]`。**jsdom のテストは「トピックを撃つと
  `/api/sites/...` のクエリが stale になる」ことしか見ておらず、画面が実際に使って
  いるキーを通っていなかった**ので通り続けていた。見つかったのは実ブラウザ
  （Chromium + ビルド済み bundle + `page.clock` で時計を進め、`/api/**` をスタブして
  リクエスト数を数える）で「10 分進めても `/api/sites/tokyo/programs` の回数が
  1 のまま」を観測したとき。回帰テストは `web/src/lib/events.test.tsx` の
  「epg のイベントで番組リスト（手書きのクエリキー）も取り直す」
- **予約詳細も同じ形で漏れていた**（同 issue #181 のレビューで、生成キー 20 本 + 手書き
  2 本を全部 `startsWith` に当てる全数確認をして発見）。orval の生成キー
  `['/api/sites/{site}/programs/{programId}/reservation']` は `'/api/reservations'` に
  前方一致しないので `reservations` トピックが届かず、代わりに `'/api/sites/'` に
  掛かって**収束が 60 秒ではなく 10 分**になっていた。番組リストの件と**同じ形の漏れ**
  （キーの先頭要素が所属を決めることの見落とし）で、片方を直したときにもう片方に
  気付けなかった。回帰テストは `pages/reservation-detail.test.tsx` の
  「予約一覧の invalidate（`['/api/reservations']`）が詳細ページにも届く」と
  `lib/events.test.tsx` の「予約詳細は運用状態グループ（60 秒）で取り直す」
- SSE の初期実装は M1-7 で api ロール内（`internal/api/events.go` の `EventHub`）に
  置かれ、M2-19（issue #24）で notifier ロールへ分離した。ロールを分ける判断と
  「2 つの SSE を集約しない」判断は issue #25 §4
