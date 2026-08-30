> [docs/api.md](../api.md)（索引）の分割本文。REST API の設計判断だけを置く。**パス・パラメータ一覧・enum・既定値の権威は openapi.yaml** であり、ここには書き写さない。

## REST API（OpenAPI ファースト・コード生成・後方互換）

予約・ルール・録画一覧などの CRUD と検索はすべて REST API で提供する。**OpenAPI 定義を単一の真実**とし、クライアント・サーバー双方のコードを生成する。

### コード生成パイプライン

- **TypeScript 側**: orval で型付きクライアント（TanStack Query フック）を生成
- **Go 側**: ハンドラの型を OpenAPI 定義から導出（oapi-codegen）

### 契約の保護

- 破壊的変更は生成物の差分として **CI で検知**する
- UI と API のデプロイタイミングはずれ得るため、**運用開始後は API の後方互換を保つ**（参照: [frontend.md](../frontend.md) のアセット配信）。
  **運用開始前は保たない。** 互換のための分岐は「壊れたときに気付けない」形（旧パラメータが黙って無視され全件が返る等）で残り続けるので、
  外から見える資源同定（パス・識別子・クエリパラメータ）を直す機会は運用開始前しかない（不変条件 11「将来への先払いは高い方から」）

### エンドポイント設計の規約

- API パスは常に**ルート相対パス `/api/*`**（ドメインを含まない絶対パス）を使用する。CDN / リバースプロキシ構成で CORS 不要・実行時コンフィグ注入不要とするため
- **絶対 URL ビルダーは作らない。** Rokuban には絶対 URL を生成している箇所が現状ゼロ（API・webhook ペイロードともルート相対パスのみ）。無い箇所にビルダーを先回りで作らない（不変条件 11）。必要になった時点で単一ビルダーへ一元化する方針は維持する（棚卸しの経緯は末尾「経緯と失敗事例」）
- **site は資源同定に含める。** `programId` は site スコープ（[スキーマ](../schema.md) §1-5）なので、番組・意図・上書きを指すパスはすべて `/api/sites/{site}/programs/{programId}...` の形を取る。**api プロセス自身はどの site にも束縛されない**（不変条件 1: mirakc にもファイルシステムにも依存しない）。権威は `config.mirakc`/`mirakcs` レジストリに site が存在するかで、1 プロセスがレジストリの全 site を処理できる。レジストリに無い site を指定すると、読み取り系（GET）は 404、書き込み系（POST/PUT/PATCH/DELETE）は 400 を返す。存在する site の一覧は `GET /api/sites`（mirakc の URL は含まない）で取得できる

### 機能の有効/無効は能力 API で観測する

**フロントは config を読めない**（api ロールは設定ファイルを配らない。不変条件 1）。
一方で `live.enabled` のように「無効ならその機能への導線ごと出したくない」設定が
ある。無効な機能の導線を出すと、押した先で「無い」に当たるだけになる ---
issue #209 では `live.enabled: false` のときも主ナビに「ライブ」が出続け、
プレイリストの URL が SPA フォールバックの HTML 200 を返していたため、
**「無効な機能」ではなく「壊れた再生」として見えていた**。

`GET /api/capabilities`（真偽値の集合）で観測する。判断:

- **設定値そのものは返さない。** 返すのは「導線を出してよいか」だけで、config の
  キー名・値・ffmpeg のパス・プロファイル定義は載せない（`GET /api/encode-profiles`
  / `GET /api/sites` と同じ規律）。将来 `live.profiles` の一覧が要るなら、それは
  能力ではなく選択肢なので別のエンドポイントになる
- **フィールドは全て required にする。** 欠けた項目を「未対応の古いサーバー」と
  読むか「無効」と読むかをフロントに判断させると、判断が導線ごとにばらける
- **「有効」は「今すぐ使える」ではない。** `live: true` は config が有効という
  事実だけを表す。streamer が動いていない / チューナーが埋まっているといった
  実行時の状態は、従来どおり当該 API の 404 / 503 として出る
  （[frontend/live.md](../frontend/live.md)）
- **ビルド時フラグにはしない。** 実行時 config で on/off する設計と食い違う
  （同じ SPA アセットがどの構成にも配れる、という前提を壊す）

**能力の値はロールに依存させない。** 生成ルートはロールで絞られない（api ロールを
持たないプロセスでも `/api/capabilities` は生える）ので、注入を api ロールの分岐に
置くと同じ config の別プロセスだけ違う答えを返す（`cmd/rokuban/server.go`。
`Sites` / `MetricsRegistry` を無条件に渡しているのと同じ理由）。

**`/api/` 配下の未マッチは SPA に落とさず 404（JSON）にする。**（`internal/api/spa.go`）
落とすと「無い」が「200 の HTML」になり、プレイリストを probe するような
「取れたか」で判断するクライアントが成功と誤認する。ライブに限らず、
登録されないルート全般（api ロール単独の `/api/events` など）に効く。
`/api` そのもの（末尾スラッシュ無し）も同じ扱い --- 末尾 1 文字で 404 と
HTML 200 に分かれると、この規則を覚えていられない。

### EPG の読み取り

番組表を引く API の形。番組リスト UI とグリッドの両方がこれを使う。

#### 時間窓がカーソル。ページネーショントークンを持たない

`GET /api/sites/{site}/programs` は `start` / `end` の時間窓で引く。トークン方式を採らない理由:

- **データ量が有界** — EPG はローリングウィンドウで、実測 8 日分 2680 件
  （GR のみ 7 チャンネル）。「ページ 500」が原理的に発生しないので、
  ページネーションが解決する問題が存在しない
- **トークンの中身が時間窓の言い換えになる** — 安定ソート鍵は
  `(start_at, network_id, service_id)` であり、これを opaque token に包むと
  日付ディープリンクができず、キャッシュも効かなくなるだけ
- **プロジェクションが 10 分ごとに全量書き換わる** — カーソルが指していた番組が
  消えているケースを常に扱う必要が出る。時間窓なら「同じ窓 → 常に同じ完全な結果」で、
  クエリキーが自然に決まり EPG の churn にも強い

窓の最大幅を超えたら 400 を返し、**無言の切り詰めはしない**
（切り詰めると「全部取れた」と誤解される）。窓の重なり判定・幅の上限・
`service=<Service.id>` の複数指定は openapi.yaml の description が権威。

#### サービス一覧は `hasPrograms` を足すが、それでは絞らない

`Service.hasPrograms` は、EPG プロジェクション**全体**（表示中の時間窓ではない）に
そのサービスの番組が 1 件でもあるかを表す。時間窓に依存させないのは、フロントの
チャンネル絞り込み候補をこのフラグから作るため（[frontend.md](../frontend.md)）
--- 候補が時間窓や絞り込み選択に依存すると、「1 局に絞ると他局へ切り替えられ
なくなる」「ページを読み込むほど候補が増える」という壊れ方をする。

**このエンドポイント自体は `hasPrograms` で行を絞らない。** 番組を持たない
サービスも含めて全件返す。理由は 2 つ:

- ルール編集画面（`/rules`）がサービスを選ぶので、いま番組を持っていない局も
  選べる必要がある
- このエンドポイントは「EPG プロジェクションのサービス一覧」であり、射影に
  居るが番組ゼロのサービスもその定義上の正当な構成員である。絞ると一覧の
  名前が実体と食い違う

#### 一覧と詳細で形を分ける（段階的開示）

番組は一覧（`ProgramListItem`。軽い形）と詳細（`Program`。extended / video /
audios 込み）で形を分ける。`epg_programs` は UI 完全形なので `extended`
（出演者等）が数 KB あり、全列返すと 1 日分 335 行で 1.5 MB になるが、
一覧用の軽い列だけなら約 85 KB/日。UI は行を展開したときに詳細を取る。

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

### 録画一覧: 絞り込み + キーセットページング

`GET /api/recordings` の絞り込みパラメータ・キーセットカーソルの使い方・
既定値と上限は openapi.yaml の `listRecordings` の description が権威。
ここには openapi.yaml に載らない判断だけを残す。

**既定は全サイトを返す。** api は不変条件 1 により site に束縛されないため、
1 プロセスがレジストリの全 site を扱う。`GET /api/reservations` /
`GET /api/capacity/overages` も同じ形（全サイトを返し、各要素が `site` を持つ）。

**`?site=` は「絞り込み」であって「束縛」ではない。** 束縛はプロセスがどの
mirakc に触れるかの話で、絞り込みは読み出しの述語にすぎないので不変条件 1 とは
別の軸である。site を `service` の識別子に混ぜてはならない --- 混ぜると
「あるサイトの録画を全部」がチャンネルの列挙でしか表せず、`service` だけが
他の軸と違う意味論（組の選言）を持つことになる。全軸を「軸内は OR、軸間は
AND」に揃える。

**`GET /api/storage` は上記と違って `site` フィールドを持たない。** アーカイブ
（`storage.media_dir`）とスクラッチ（`storage.scratch_dir`）は mirakc サイトの
ように複数存在しうる資源ではなく単一なので、「全サイトを返し各要素に `site` を
持たせる」形は当てはまらない（issue #238 M7-5。詳細は
[docs/storage.md](../storage.md) §5「残量の観測」）。

**`trash=true` でもカーソル軸は `program_start_at` 降順のまま**（`deleted_at`
降順にしない）。一覧・ごみ箱を 1 つのキーセット契約に統一するにあたり、`trash` に
よってカーソル軸が変わる形は採らなかった --- `before` / `beforeId` の意味が
`trash` の値に依存すると、同じパラメータ名で違う軸を指すことになり API 契約として
破綻する（1 エンドポイントの前提が `trash` という別のフラグの値で変わるのは、
形をモード分岐させる代償の方が大きい。不変条件 11）。ごみ箱 UI で「最近捨てた
ものが上」が要る場合は、フロント側で `deletedAt` により再ソートする（1 ページ内
なら安価。ページを跨いだ再ソートが要るなら別途検討）。旧実装との関係は末尾
「経緯と失敗事例」。

#### 動的 WHERE ビルダ（sqlc の静的クエリにしない）

`internal/api/recordings_query.go` が `internal/rulequery.Compile` と同じ
`arg` クロージャ方式で WHERE を組む。sqlc の 1 querytext に `($n IS NULL OR ...)`
形で全軸を詰め込むと、`q` のような選択条件でも汎用プランに落ちて
`recordings_title_trgm` / `recordings_description_trgm`（式 GIN）が使われないことがある。
条件が実際に指定されたときだけ節を足す形にすることで、Postgres が最初に立てる
プランは常に具体的になる。

**これだけでは片方の劣化しか塞げない。** pgx の既定 `QueryExecModeCacheStatement`
は SQL テキストごとに named prepared statement を作ってキャッシュし、
Postgres 自身がその statement を 6 回目以降 custom plan から generic plan に
切り替えることがある（PostgreSQL の PREPARE のプラン選択規則。generic plan は
bind 値を見ないため trgm 式 GIN が選ばれない可能性がある。実測: 6 回目の実行で
0.7ms → 290ms）。これは動的 WHERE
ビルダが解決する「汎用述語（`$n IS NULL OR ...`）による劣化」とは別の劣化経路
なので、`queryRecordings` はこの経路だけ `pgx.QueryExecModeExec`
（unnamed statement で毎回明示的に再計画）を指定して別途塞いでいる。この経路は
絞り込みの組み合わせごとに SQL テキスト自体が変わるため、そもそも named
statement のキャッシュが効く場面が少ない（キャッシュを維持するコストに対して
利益が薄い）。

なお録画検索のキーワード正規化は EPG 検索と同じ `normalize_search_text`
（[data.md](../data.md) §5）を使うが、**エンジン（`internal/rulequery`）は
共有しない**。理由は [data.md](../data.md) §5「録画検索は rulequery を共有しない」。

### 取り込み状態は一覧の要素に載せる（別エンドポイントにしない）

原本の取り込み（ingest）がどこまで進んだかは `Recording.ingest` として一覧要素と
単体 GET の両方に載せる。**専用のエンドポイントを足さない** --- 取り込み中かどうかは
「その録画が再生できるか」と同じ質問であり、一覧を引いた時点で答えが要る（別
エンドポイントにすると、一覧の行数ぶん N+1 のリクエストになる）。

api ロールは mirakc に問い合わせない（不変条件 1）ので、進捗の真実は worker が
DB に残した観測（`recording_ingest_progress`）だけ。api は**毎リクエストでその行と
`media_assets` / `record_sync` の有無から状態を導出する**（列に焼かない。不変条件 9）。
状態の値の意味と、なぜ「リトライ中」を持たないかは `openapi.yaml` の
`IngestProgress` と [recording/ingest.md](../recording/ingest.md) §5.6。

**進捗は SSE で押さない。** SSE はヒントであって真実ではない（不変条件 5）ので、
クライアントは REST の再取得で収束させる。ただし短い周期（5 秒）を張るのは
**進捗の数字が動いている間だけ**で、それ以外は SSE グループの 60 秒 invalidate に
落とす（[frontend/recordings.md](../frontend/recordings.md)）。「未完了なら短い周期」に
すると、`pending` が恒久的に残りうる（`record_sync` 行は消えない）ためポーリングが
止まらない。

### エンコードの試行状態も一覧の要素に載せる

未完了のエンコードプロファイルの状態は `Recording.encodeStatus` として一覧要素と
単体 GET の両方に載せる。取り込み状態（上記）と同じ 3 点を同じ理由で決めている ---
**別エンドポイントにしない**（「この録画のエンコードは進んでいるのか」は一覧を
引いた時点で答えが要る質問で、分けると行数ぶんの N+1 になる）、**毎リクエスト
導出する**（真実は worker が書いた `recording_encode_attempts` の行で、api は
それと `media_assets` の active な `encoded` から毎回組み立てる。列に焼かない。
不変条件 9）、**SSE で押さない**（専用トピックも NOTIFY も持たず、既存の
`recordings` グループの 60 秒 invalidate で収束させる。不変条件 5）。

取り込み状態と違う点は 2 つある。

- **完了したプロファイルは出さない。** `encodedAssets` の存在が「完了」を表すので、
  同じ情報を 2 つの配列で主張しない（`encodeStatus` は desired − observed の差だけ）。
- **`queued` は「来る根拠」があるものにしか付けない。** api ロールは worker にも
  mirakc にも問い合わせない（不変条件 1）ので、「来る」と言えるかどうかは DB と
  自分の設定だけで決める必要がある。ごみ箱の録画（reconciler が `deleted_at IS NULL`
  で絞るのでジョブは二度と投入されない）と、`config.encode.profiles` から消えた
  プロファイル（reconciler の `known_profiles` 絞り込みが投入対象から外す）は、
  その要素自体を省略する --- 進捗の無いプログレスバーを出さないのと同じ判断
  （[frontend/recordings.md](../frontend/recordings.md)）。**「設定を読む」判定を
  api に置いたのはここだけで、mirakc への問い合わせは足していない。**

`running`/`failed` の各値の意味と、失敗が固定される条件は `openapi.yaml` の
`EncodeJobStatus`。表そのものの判断は
[schema/recordings.md](../schema/recordings.md) の `recording_encode_attempts` 節。
失敗理由（`error`）と最後の試行時刻（`attempted_at`）は**契約に載せない** ---
読み手は運用者の SELECT（[runbook/troubleshooting.md](../runbook/troubleshooting.md)）。

### 録画単体: `GET /api/recordings/{id}`

一覧要素と同形の 1 件（skip 理由・予約からの導線の着地先）。
**ごみ箱（`deleted_at IS NOT NULL`）の録画も 200 で返す** --- 一覧の
`trash=true` が既にメタデータを 200 で返しているため、単体 GET だけ厳しくする
理由が無い（メディア配信が `deleted_at IS NOT NULL` を 404 にする契約
[api/media.md](media.md) とは別の判断）。**完全削除（purge）済みの
tombstone（`purged_at` が立った行）だけは 404** にする --- ファイルが既に無く
通常一覧・ごみ箱一覧のどちらにも現れない行なので、単体 GET だけ見える形に
しない。

## 経緯と失敗事例

- **絶対 URL ビルダーの棚卸し**（M4-1、issue #89）: EPGStation#694 の教訓（絶対
  URL 生成が散らばって `X-Forwarded-Prefix` 対応が後から効かなかった）を踏まえて
  Go 側・TS 側双方を棚卸しした結果、絶対 URL を生成している箇所はゼロだった。
  同じ棚卸しで `X-Forwarded-*` 系の扱いも決めた（[deployment.md](deployment.md)
  末尾「経緯と失敗事例」）
- **site を資源同定に含める決定**は issue #29 / #31 / #53 の案 A（M3-1）。導出行
  （`reservations`）を書き込みの宛先にした旧 API の失敗は
  [docs/invariants.md](../invariants.md) §9「identity」。1 プロセスが全 site を
  処理する形（site 非依存の一覧 API）は issue #184（M4-12）
- **`trash=true` の並び順**は旧 `ListTrashRecordings`
  （`internal/db/queries/recordings_trash.sql`）の `deleted_at DESC, id DESC`
  （「最近捨てたものが上」）から `program_start_at` 降順へ意図的に変更した
  （PR #187 レビュー、M4。一覧・ごみ箱のキーセット契約統一時）
- **EPG の読み取り**は M1-6 / M1-7、**録画一覧のキーセット化**は M3-24 の成果物
- **録画単体（`GET /api/recordings/{id}`）**は M6-4（issue #232）
- **能力 API（`GET /api/capabilities`）と `/api/` の 404 化**は issue #209。
  「無効な機能への導線が常時出ていて壊れているように見える」という報告で、
  原因は 2 つ重なっていた: フロントが `live.enabled` を知る手段が無かったことと、
  未登録の `/api/` パスが SPA の HTML 200 になっていたこと。**後者だけを直しても
  ナビは消えず、前者だけを直してもディープリンクの再生は「壊れて見える」まま**
  だったので、両方を同じ PR で直している
