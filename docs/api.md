# API 設計

## 設計方針: REST + SSE + メディア配信の 3 本

フロントエンド・バックエンド間の通信経路は性質ごとに 3 本に分離する。双方向チャネル（WebSocket / Socket.IO）は使わない。

| 経路 | 方向 | 用途 |
|---|---|---|
| REST API (`/api/*`) | 双方向（リクエスト/レスポンス） | CRUD・検索・操作 |
| SSE (`/events`) | サーバー → クライアント | 変更ヒントの配送 |
| メディア配信（HTTP GET） | クライアント → サーバー | 録画再生・ライブ HLS・サムネイル |

クライアント → サーバーは常に REST、サーバー → クライアントは常に SSE。一方通行 x 2 の構成であり、EPGStation の Socket.IO のような双方向チャネルが必要になる場面（クライアントからの継続的入力をサーバーが受ける）がこのアプリには存在しない。

## REST API（OpenAPI ファースト・コード生成・後方互換）

予約・ルール・録画一覧などの CRUD と検索はすべて REST API で提供する。**OpenAPI 定義を単一の真実**とし、クライアント・サーバー双方のコードを生成する。

### コード生成パイプライン

- **TypeScript 側**: orval で型付きクライアント（TanStack Query フック）を生成
- **Go 側**: ハンドラの型を OpenAPI 定義から導出（oapi-codegen）

### 契約の保護

- 破壊的変更は生成物の差分として **CI で検知**する
- UI と API のデプロイタイミングはずれ得るため、**API は後方互換を保つ**（参照: [frontend.md](frontend.md) のアセット配信）

### エンドポイント設計の規約

- API パスは常に**相対パス `/api/*`** を使用する。CDN / リバースプロキシ構成で CORS 不要・実行時コンフィグ注入不要とするため
- 絶対 URL の生成は単一のビルダーに一元化し、`X-Forwarded-Prefix` ないし `public_url` 設定を尊重する（EPGStation#694 の教訓）
- **site を資源同定に含めるかは未決**（[#31](https://github.com/fetburner/rokuban/issues/31)）。現状パスに site は無く、`db.DefaultSite` のハードコードで単一サイトを前提にしている。ところが `programId` は site スコープ（[スキーマ](schema.md) §1-5）なので、`/api/programs/{programId}` 系は多拠点化した瞬間に意味が定まらない。DB 側には全表に `site` 列があるが、**多拠点化で本当に壊れるのは API のパス構造とクライアントのクエリキー**で、そこが未払いである。宛先を触る変更（[#29](https://github.com/fetburner/rokuban/issues/29)）と同時に決める

### EPG の読み取り（M1-6 / M1-7）

番組表を引く API の形。番組リスト UI（M1-7）とグリッド（M2）の両方がこれを使う。

#### 時間窓がカーソル。ページネーショントークンを持たない

```
GET /api/programs?start=&end=&networkId=&serviceId=
```

`start` / `end` の窓に**一部でも重なる**番組を `start_at` 昇順で返す
（`start_at < end AND end_at > start`）。窓開始前に始まった放送中の番組も含む。

トークン方式を採らない理由:

- **データ量が有界** — EPG はローリングウィンドウで、実測 8 日分 2680 件
  （GR のみ 7 チャンネル）。「ページ 500」が原理的に発生しないので、
  ページネーションが解決する問題が存在しない
- **トークンの中身が時間窓の言い換えになる** — 安定ソート鍵は
  `(start_at, network_id, service_id)` であり、これを opaque token に包むと
  日付ディープリンクができず、キャッシュも効かなくなるだけ
- **プロジェクションが 10 分ごとに全量書き換わる** — カーソルが指していた番組が
  消えているケースを常に扱う必要が出る。時間窓なら「同じ窓 → 常に同じ完全な結果」で、
  クエリキーが自然に決まり EPG の churn にも強い

窓の**最大幅は 7 日**で、超えたら 400 を返す。無言の切り詰めはしない
（切り詰めると「全部取れた」と誤解される）。

無限スクロールは窓を継ぎ足して実現する。窓は開区間なので**境界をまたぐ番組が
隣接する 2 つの窓の両方に現れる**。クライアントは `programId` で重複排除する
（OpenAPI の description に明記）。

#### 一覧と詳細で形を分ける（段階的開示）

```
GET /api/programs        → ProgramListItem[]   軽い形
GET /api/programs/{id}   → Program            extended / video / audios 込み
```

`epg_programs` は UI 完全形なので `extended`（出演者等）が数 KB ある。全列返すと
1 日分 335 行で 1.5 MB になるが、一覧用の軽い列だけなら約 85 KB/日。
UI は行を展開したときに詳細を取る。

**フィールド選択（`?fields=`）は入れない。** 生成される型が実質 `Partial<T>` に
劣化して OpenAPI + コード生成の利点が消え、TanStack Query のドキュメント
キャッシュも分裂する（フィールドセットが違うと別エントリになる）。GraphQL を
却下した論拠「クライアントは 1 つで、必要なクエリの形はすべて既知」に従い、
**形を名前付きで少数だけ用意する**。形が 4 つ 5 つと増えて組み合わせ爆発の兆候が
出たら、それが GraphQL を再検討するシグナルであり、`?fields=` を黙って足す話ではない。

#### 予約状態は番組と結合しない

番組リストの各行に「予約済み」を出すために `ProgramListItem` へ `reservationId` を
持たせる（サーバーで JOIN する）ことはしない。`/api/reservations` を別に取って
クライアント側で結合する。

- 予約は頻繁に変わり番組はほとんど変わらない。**キャッシュの寿命が違う**
- サーバーで JOIN すると、1 件予約するたびに SSE で番組リスト全体を
  invalidate することになる。分けておけば予約クエリだけ捨てれば済む
- 予約は数十件なのでクライアント結合は Map 1 つで済む

## SSE (`/api/events`) --- ヒント配送、状態の真実は REST から再取得

サーバー → クライアントの一方向プッシュ。役割は**「どのデータが変わったか」のヒント配送だけ**。

**実装は `internal/notifier` ロールの所有物。** api ロールは mirakc にもファイルシステムにも
依存しない（不変条件 1）のと同じ理由で、長寿命接続である SSE も持たない。api は desired
state を Postgres に書くだけの純粋なリクエスト/レスポンス層に留め、Postgres を LISTEN して
ブラウザへ配り直すだけの小さな常駐プロセスを notifier として分けている
（issue #24 M2-19、issue #25 §4。旧: `internal/api/events.go` の `EventHub`）。
monolith（`--all`）では `internal/api.RouterConfig.Mounter` 経由で streamer と同じ
リスナーに相乗りする（`api.Mounters` で束ねる）。ロールを分けて起動したときは、notifier
ロールが自分の HTTP サーバーで同じパスを serve する。**api ロール単独では `/api/events` は
登録されず 404 になる。**

### 実装（M1-7、M2-19 で notifier へ分離）

**OpenAPI には載せない。** 長寿命ストリームは OpenAPI のリクエスト/レスポンスモデルに乗らず、
コード生成させると応答をバッファリングする形になってしまう。クライアントも生成フックではなく
`EventSource` を直接使うため、生成物から得るものがない。ルーターに直接登録し、仕様はここに置く。

**トピック**は表ではなくクライアントの関心事に揃える。1 トピックが 1 つのクエリキー接頭辞に対応する。

| トピック | 発火元 | クライアントが invalidate するもの |
|---|---|---|
| `reservations` | `reservations` の行トリガー | 予約一覧・予約詳細 |
| `recordings` | `recordings` / `media_assets` の行トリガー | 録画一覧（サイズ・ドロップ統計も含む） |
| `epg` | EPG 同期ジョブが明示的に `pg_notify` | 番組リスト・サービス一覧 |

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

バックエンドは「NOTIFY はヒント、真実はテーブル再読」のレベルトリガー設計（参照: [data.md](data.md)）。フロントも同じ形にする:

1. SSE イベントを受信したら、該当クエリの `invalidateQueries` を実行
2. 真実は常に REST から再取得
3. **プッシュの中身を直接信頼して画面状態を書き換えることはしない**

SSE の取りこぼしは stale-time 経過後の再取得で自然回復する。プッシュデータを信頼して手元状態を書き換える設計（Socket.IO 時代の EPGStation）より壊れ方が大幅に単純になる。

### 水平スケール

notifier ロールを複数レプリカにしても、各レプリカが Postgres の NOTIFY を購読して配るだけなので **Redis アダプタ等の追加基盤は不要**。notifier は**シングルトンではない**（`cmd/rokuban/server.go` の `singletonRoles` に含まれない） --- 各レプリカが独立に LISTEN し、自分にぶら下がる SSE クライアントにだけ配る。レプリカ間で配送を調停する必要が構造的にない（参照: [data.md](data.md) §3）。

### SSE とサーバーレスの関係

SSE は長寿命接続でありサーバーレスとは相性が悪い。ハイブリッド構成では CDN のパスルーティングで `/api/events` だけ **notifier ロール**へ振り分ける（api ロールには SSE エンドポイント自体が存在しない）。自宅ダウン中は invalidation が止まるだけで読み書きは動く --- きれいな劣化（参照: [overview.md](overview.md) のハイブリッド構成）。

### 2 つの SSE を 1 つに集約しない

Rokuban には長寿命接続が 2 つある --- notifier がブラウザへ送る `/api/events` と、watcher が mirakc から受ける `/events` 購読。**どちらも「長寿命接続を張り続ける」という機構は同じだが、関心事は無関係なので 1 つのプロセス/抽象にまとめない**（issue #25 §4）。

| | 相手 | 向き | 落ちたときの影響 |
|---|---|---|---|
| notifier の `/api/events` | ブラウザ | 送る | UI の自動更新が止まる（読み書き自体は動く） |
| watcher の mirakc `/events` | mirakc | 受ける | ingest の投入が遅れる（`record_sweep` の定期突き合わせが拾う） |

機構でまとめると、mirakc が落ちたときにブラウザ配信も巻き込まれるといった不要な結合が生まれる。小規模構成で常駐プロセスを減らしたいという動機は monolith（`--all`）が既に満たしており、抽象を共有する理由にはならない。

## メディア配信（Range 対応・X-Accel-Redirect オプション）

録画再生・ライブ視聴の HLS プレイリスト/セグメント・サムネイル画像は HTTP GET で配信する。

### 録画済みファイルのストリーミング

Go の `http.ServeContent` は `*os.File` 相手なら sendfile が効き、Range 対応も標準。家庭サーバーの同時視聴数本でギガビット LAN を飽和させるのに問題はなく、**性能を理由とする nginx 導入は不要**。

#### 実装（M1-8）

```
GET  /api/recordings/{id}/file   →  video/MP2T（Range 対応）
HEAD /api/recordings/{id}/file   →  ヘッダーのみ
```

**HEAD も登録する。** VLC やブラウザはシーク前に HEAD で `Content-Length` と
`Accept-Ranges` を取るため、405 を返すとシーク再生に失敗しうる。
`http.ServeContent` は HEAD ならヘッダーだけを書くので実装は共通。

**OpenAPI には載せない。** SSE と同じ理由で、生成クライアントは JSON を前提にする
（`customInstance` が `response.json()` を呼ぶ）ためバイナリ配信では誤った
クライアントが生成される。UI は URL を `<video>` の src や保存リンクに直接使い、
生成フックを経由しない。守るべきスキーマがないので生成物から得るものもない。

**`internal/streamer` の所有物として実装する。** api ロールはファイルシステムに
依存しない（不変条件 1）ため、バイト転送はロールとして分ける。monolith では
`api.RouterConfig.Mounter` 経由で同一リスナーに相乗りするが、コードの境界は
最初から引いてある。`--roles streamer` を指定したときだけ登録される。

**対象は原本（`kind = 'original'`）のみ。** エンコード派生物の配信は M3。

**`rel_path` は配信側でも独立に検証する。** `internal/mediapath.Resolve` を
ingest と共有し、メディアディレクトリの外を指す `rel_path` は 404 にする。
書き込み時に検証済みでも、DB に不正な行が入った場合に任意ファイルを
読み出させないため片側だけでは足りない。

**配らないもの:** ごみ箱に入った録画（`recordings.deleted_at IS NOT NULL`）、
削除済みアセット（`media_assets.state <> 'active'`）、未 ingest の録画
（`media_assets` 行なし）。いずれも 404。コミット（DB 行）はあるのに
ファイルが無い不整合も 404 にしつつ WARN で記録する（孤児回収や外部からの削除）。

**`Cache-Control: private, max-age=0, must-revalidate`。** 原本は一度書いたら
変わらないが、ごみ箱からの復元で同じ URL の中身が入れ替わりうるので
`immutable` は付けない。`Last-Modified` による条件付きリクエストは効く。

**`media_assets.size_bytes` と実ファイルのサイズが違えば WARN で記録する。**
size_bytes は ingest 時に mirakc の Content-Length と照合した値なので、
違うならコミット後に改変・切り詰めが起きている。配信自体は続ける
（ユーザーは録画を見たい）。

#### X-Accel-Redirect（`storage.accel_location`）

意味があるのは **X-Accel-Redirect パターン**（認可判定はアプリ、バイト転送は nginx）。
設定フラグ + レスポンスヘッダー 1 個で対応でき、アプリ側コストがほぼゼロなので
オプションとして実装する（Mastodon / GitLab の X-Sendfile 対応と同じ位置づけ）。
性能を理由に必要なわけではなく、既に nginx が前段に居る構成で転送を任せられる、
という位置づけ。

```yaml
storage:
  media_dir: /mnt/media
  accel_location: /_media/     # 空なら Go が直接配る
```

有効時は本文を返さず `X-Accel-Redirect: /_media/<相対パス>` を返す。nginx 側は
`internal` な location で `media_dir` を alias する。

- **パス検証を通した後にヘッダーを返す。** 検証前に返すと、細工された `rel_path` で
  nginx に任意ファイルを配らせられる
- **値は URI として解釈されるのでパス要素を URL エスケープする。** 番組名由来の
  ファイル名には空白・括弧・日本語が入る
- Range の扱いは nginx 側に移る（`Accept-Ranges` も nginx が付ける）

### ライブ視聴の HLS --- アプリ配信を維持

ライブセッションはインメモリの使い捨て状態（全体アーキテクチャの crash-only 例外）で、「クライアントがいなくなったら ffmpeg を止める」idle GC が要る。セグメント要求がアプリを通れば last-access の更新がタダで手に入るが、nginx が scratch から直接配るとアプリはクライアントの生存を見失う。`auth_request` やログ監視で回収はできるが、セグメントは数 MB で転送負荷が軽く、複雑さに見合わない。**streamer ロールのアプリ配信のまま**とする。

### SPA アセット配信

go:embed 配信でハッシュ付きアセット immutable + それ以外 no-cache のヘッダーを正しく付ければ十分（参照: [frontend.md](frontend.md)）。本気の配信最適化は S3+CDN 経路の仕事。ここに nginx キャッシュを挟むと配信経路が 3 つになり、テストマトリクスが増える割に得るものがない。

### サービスロゴ: ドロップ（M2-12）

mirakc は起動中の局ロゴ抽出をサポートせず、運用者が事前抽出したファイルを静的登録して配るだけの機構しか持たない。Rokuban 側で再取得・自前配信する価値が薄いため実装しない（[data.md](data.md) の「サービスロゴ: ドロップ」参照）。

## 認証: アプリ内に持たない

**Rokuban はアプリ内に認証・認可機構を一切持たない。認証が必要な構成ではリバースプロキシに委譲する。**

### 根拠: 法的制約がスコープを決める

技術的な簡略化ではなく、スコープの問題として考える。日本の著作権法では放送の録画が適法なのは私的使用（30 条）の範囲内であり、世帯外に視聴させる形態は複製権・公衆送信権（送信可能化権）に抵触しうる（まねきTV / ロクラクII 最判 2011 が示した通り、機器・サービスを介した「公衆」向け提供は事業者側の侵害とされた）。

つまり **Rokuban は構造的に単一世帯用アプリ**であり、以下は将来も含めてスコープ外:

- ユーザーアカウント / マルチテナント
- 共有リンク・外部公開機能
- ユーザー単位の権限・認可

「認証を作り込む理由」が法的に存在しないので、作らない。

### 帰結

1. **ユーザーという概念を持たない** --- user テーブルなし、API に authn/authz 層なし。視聴履歴・再生位置などの状態は世帯グローバル
2. **リモートアクセスは私的使用の範囲で構成側が担保** --- 推奨は VPN / Tailscale。公開インターネット経由ならリバースプロキシで TLS + Basic 認証（や Authelia 等）。リバースプロキシ・フレンドリー要件がそのまま効く
3. **アプリ内に残る唯一のセキュリティ要件: Host ヘッダー検証** --- 認証なしの LAN アプリは DNS rebinding（悪意あるサイト → 攻撃者ドメインを LAN アドレスに解決 → ブラウザ経由で API 叩き放題）が定番の穴。許可 Host の allowlist 検証だけはアプリ側で持つ。Cookie 認証を持たないので CSRF は構造的にほぼ無関係
4. **ドキュメントで明示** --- 「インターネットに直接露出させない」「認証が要る構成の nginx 例」を同梱構成例に含める

## リバースプロキシ・フレンドリー要件

このジャンルのアプリ（EPGStation 含む）は認証・TLS をリバースプロキシに委ねるのが慣習で、真面目な公開構成では nginx / Caddy / k8s Ingress がどのみち前段に居る。Rokuban 自身は TLS や認証を抱え込まず、**リバースプロキシ前提で行儀よく振る舞う**ことを要件にする。

### 要件一覧

| 要件 | 詳細 |
|---|---|
| `X-Forwarded-*` の解釈 | `X-Forwarded-For` / `X-Forwarded-Proto` / `X-Forwarded-Host` / `X-Forwarded-Prefix` を正しく解釈する |
| 相対パスの徹底 | API・アセット参照はすべて相対パス。絶対 URL 生成は単一ビルダーに一元化 |
| WebSocket 不使用 | SSE は `proxy_buffering off` だけで通る。WebSocket のアップグレード要件を持ち込まない |
| SPA フォールバック | Go 側の catch-all で `index.html` を返す |

### 単一バイナリの自己完結は維持

`--all` で nginx なしでも全機能動作する。ミニ PC にコンポーネントを増やさないという基本方針を崩さない（参照: [overview.md](overview.md) の monolithic mode）。

nginx は「アーキテクチャ図に現れる箱」ではなく「推奨デプロイパターンの一部」。構成図は変更なし、HTTP 層の設計要件として現れる。

## nginx リファレンス構成の方針

nginx リファレンス構成例をドキュメントに同梱する。カバーする構成:

- **TLS 終端** + Let's Encrypt
- **Basic 認証**（または Authelia 等の外部認証連携）
- **X-Accel-Redirect** --- 録画ファイル配信のバイト転送を nginx に委譲
- **SSE 設定** --- `proxy_buffering off` + タイムアウト調整
- **SPA フォールバック** --- `try_files` で `index.html` へ

これは推奨構成であり、Caddy / k8s Ingress / Cloudflare Access 等でも同等の構成が可能。

## プロトコル選定の根拠

### REST + OpenAPI を選び、GraphQL / tRPC / Connect-RPC を選ばない理由

| 選択肢 | 判断 | 理由 |
|---|---|---|
| **tRPC** | 選外 | バックエンドが TypeScript であることが前提。Go なので不可 |
| **GraphQL** | 不採用 | クライアントが多様でクエリの形が予測できない場合に効く道具。このアプリのクライアントは自前の SPA 一つで、必要なクエリの形はすべて既知。導入コスト（スキーマ・リゾルバ・キャッシュ層）に見合う自由度の需要がない |
| **Connect-RPC / gRPC-web** | 不採用 | 型安全性は魅力だが、HTTP セマンティクスによるキャッシュ・curl で叩けるデバッグ容易性・HLS 配信との統一感で素の REST に軍配 |
| **REST + コード生成** | **採用** | 型安全性は OpenAPI + orval / oapi-codegen で実用上同水準を確保できる |

### SSE を選び、WebSocket / Socket.IO を選ばない理由

- mirakc と同じ「接続時に現在状態を再送し、以降差分」という設計に揃えることで、クライアントの再接続処理が対称的になる
- api ロールの水平スケールで Redis アダプタが不要
- リバースプロキシの設定が `proxy_buffering off` だけで済む（WebSocket のアップグレードハンドシェイクが不要）
- このアプリにクライアントからサーバーへの継続的プッシュが必要な場面が存在しない
