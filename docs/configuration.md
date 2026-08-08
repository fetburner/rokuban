# 設定

設定は **YAML 1 ファイル + パース前の `${VAR}` 展開**の一本とする。

## 方針

設定の入口は「YAML 1 ファイル + `${VAR}` 展開」のみ。CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定し、設定値そのものはフラグで渡さない。

## 展開方式

Grafana Loki / Tempo の `-config.expand-env` と同じ、**YAML パース前の生テキスト展開**。実装は [drone/envsubst](https://github.com/drone/envsubst) で、`${VAR}` と `${VAR:-default}` のシェル風記法をサポートする。

- **展開は常時オン**。Loki がオプトインなのは後方互換のためで、グリーンフィールドには不要。リテラルの `${` は `$$` でエスケープ（エンコードコマンド内等）
- **`os.ExpandEnv` は使わない**。未定義変数を黙って空文字にするため、パスワード未設定のまま起動して謎の認証エラーになる。`LookupEnv` で未定義を検出し、**起動時に「未定義: POSTGRES_PASSWORD」と fail-fast** する（crash-only 方針と整合。[overview.md](overview.md) 参照）
- **YAML ライブラリは goccy/go-yaml**。yaml.v3 は事実上メンテ停止（2026 時点）

### 機微情報の供給

アプリから見えるのは常に環境変数だけなので、機構 1 つで全環境をカバーできる:

- 自宅 (Docker Compose): `.env` / `env_file:`
- 自宅 (systemd): `EnvironmentFile=/etc/rokuban/secrets.env`
- k8s: `Secret` → `envFrom` / `secretKeyRef`（config 本体は ConfigMap）

## スキーマ構造

### 関心事別にセクションを切る

ロール別ではなく関心事別にトップレベルを構成する。ロール別 Deployment でも config は同一ファイルを共有するため、ロールで切ると streamer と worker が同じ ffmpeg 設定を二重に持つことになる。各ロールは自分に関係するセクションだけを読む。

### リファレンス

```yaml
server:                          # HTTP を持つロール (api, streamer, notifier) 共通
  listen: ":40773"               # mirakc の 40772 の隣
  allowed_hosts: [rokuban.local] # Host ヘッダー allowlist（X-Forwarded-Host があれば優先）。localhost 系は常に許可

db:                              # 必須
  host: postgres
  port: 5432
  user: ${POSTGRES_USER}
  password: ${POSTGRES_PASSWORD}
  database: rokuban
  sslmode: disable
  max_conns: 0                    # プロセスが持つ唯一のプールの上限。0 なら起動時の roles から
                                  # ロール別 budget を合計して自動算出する（operations.md §3、issue #90）
  api_statement_timeout: 0s       # api ロールを含むプロセスにだけ適用。0 なら既定値 30s
  pooler_compat: false            # true は PgBouncer/Neon pooler 越しの接続を想定したモード。
                                  # pooler を通せるのは api ロールと streamer ロールだけ
                                  # （worker/watcher/notifier との組み合わせは起動時エラー。operations.md §3）

mirakc:                          # `mirakcs:` との排他（下記）。どちらか一方が必須
  url: http://mirakc.local:40772
  site: default                  # 省略時 "default"。このインスタンスのサイト名
                                 # （DB の全テーブルと API のパス /api/sites/{site}/...
                                 # をスコープする。issue #31）

# 複数 mirakc（issue #183 M4-11）。`mirakc:` の代わりにこちらを使う。
# `mirakc:` は `mirakcs:` の 1 要素と等価に解決される糖衣なので、両方を同時に
# 書くと起動エラーになる（どちらが勝つかを覚えさせない）。
# mirakcs:
#   - site: tokyo
#     url: http://mirakc-tokyo:40772
#   - site: takamatsu
#     url: http://mirakc-takamatsu:40772

storage:
  media_dir: /mnt/media          # 必須。アーカイブ層 (S3-via-CSI 可)
  scratch_dir: /var/tmp/rokuban  # ローカルスクラッチ
  accel_location: ""             # X-Accel-Redirect の internal location。空なら Go が直接配る

ingest:
  concurrency: 2                 # mirakc サイトあたり 1-2
  stall_timeout: 30s             # 転送中の無進捗検知。この時間バイトが進まないと切断して
                                 # Range 再開する。River の総時間タイムアウトは無効化している
                                 # ため、これが ingest の唯一のタイムアウト（recording.md §5.3）。
                                 # 遅い回線や大きな録画で誤爆する場合は延ばす

epg:
  sync_interval: 10m             # mirakc から EPG を全量取得する間隔
  retention_grace: 24h           # 放送終了からこの時間で番組を刈り取る。
                                 # ruler の GC（予約と番組単位の意図）も同じ猶予を使う

ruler:
  max_deletes_per_pass: 50       # 1 サイト・1 パスで実行してよい導出削除数の上限。超えたら
                                 # 削除を一切実行せず、大量削除サーキットブレーカーとして発動する。
                                 # 手動で resume するまで止まり続けるラッチで、件数が閾値以下に
                                 # 戻っても自動では解けない（recording.md §3.2「発動はラッチ」）

reconciler:
  start_delay_grace: 3m          # 開始遅延検出器（recording.md §3.3）の猶予。「開始時刻 +
                                 # この時間」を過ぎても recordings.started_at が観測されない
                                 # 予約を検出してアラートする。mirakc 側の未知の不具合への保険
                                 # （EPGStation#724 の実例あり）。短すぎると開始直後の SSE 到達・
                                 # watcher 処理の遅れを誤検知する

worker:
  periodic_jobs: true            # プロセス内で定期ジョブ（epg_sync / ruler_pass / reconcile_pass / record_sweep）を投入するか。
                                 # k8s では false にし、CronJob から rokuban enqueue で投入する
                                 # （River の PeriodicJobs はリーダーだけが投入するため、
                                 #  KEDA で 0 にスケールすると誰も投入しなくなる。data.md §2）
  queues: []                     # worker ロールが引くキューを絞る。空なら全部。
                                 # ロールを増やさずに「ruler / reconciler だけ別 Pod」を実現するための knob。
                                 # worker ロールが無いプロセス（watcher 単独等）はこの設定に関わらず
                                 # キューを一切引かない（operations.md §ロールとキュー購読、issue #113）。
                                 # ここに書くのは論理名（ingest / epg / ruler / reconciler / watcher /
                                 # encode / thumbnail / cleanup / default）のまま --- プロセスが自分の
                                 # 束縛サイト（--sites）で site 単位のキューを実際の物理名
                                 # （`ingest_tokyo` 等）に展開する。サイトごとに ConfigMap を分けずに
                                 # 済ませるための設計（issue #185 M4-13）

encode:
  ffmpeg: ffmpeg                 # 既定は PATH 検索。worker ロールが encode/thumbnail キューを
  ffprobe: ffprobe               # 購読するとき（worker.queues が空、または encode/thumbnail を含む
                                 # とき）だけ LookPath で存在検査する（api ロールは呼ばない。
                                 # 不変条件 4、issue #113）
  concurrency: 1                 # encode キューの MaxWorkers（ingest とは独立）
  thumbnail_concurrency: 1       # thumbnail キューの MaxWorkers
  profiles:
    - name: h264                 # ルール / overrides から参照する一意な名前
      container: mp4             # mp4 | mkv（拡張子と -f）
      video_codec: libx264
      audio_codec: aac
      height: 1080               # 0 or omit = スケールしない
      crf: 23                    # optional
      preset: medium             # optional
      extra_args: []             # 末尾に追加する ffmpeg 引数（自由形式 cmd 全体は禁止）

live:                            # ライブ視聴（M4-3、issue #91）。streamer ロールのみ使う
  enabled: false                 # true のときだけライブのルートを登録し、ffmpeg の
                                 # LookPath 検査も行う。false（既定）なら公式イメージ
                                 # （ffmpeg 無し）で streamer を起動する構成
                                 # （録画配信 / サムネイルのみ）を壊さない（不変条件 4）
  ffmpeg: ffmpeg                 # 既定は PATH 検索
  segment_dir: /dev/shm/rokuban-live  # HLS セグメント/プレイリストの書き出し先。
                                 # **既定値を持たない --- enabled: true なら必須**
                                 # （空だと起動時エラー。適当なローカルパスへ黙って
                                 # フォールバックしない）。録画バッファ（mirakc
                                 # recording.basedir）とは別ディスク（tmpfs 前提。
                                 # k8s なら emptyDir: {medium: Memory}）。起動時
                                 # （HTTP リスナーが立つ前）にこのディレクトリ全体を
                                 # 掃く --- tmpfs はノード再起動でしか消えないため、
                                 # 前回プロセスの残骸（SIGKILL 等で SIGTERM の
                                 # 後始末が効かなかった場合）が残っていても毎起動で
                                 # 必ず消える。**複数プロセス / 複数レプリカで共有し
                                 # ない。** 起動時の掃除は「自分がこのパスの唯一の
                                 # 書き手である」という前提に立っており、共有すると
                                 # 後発の起動が先発の生きているセッションディレクトリ
                                 # を消してしまう（視聴は一度途切れるが、次のプレイ
                                 # リスト要求で新しいセッションが起きて自己修復する）
  max_sessions: 4                # このプロセスが同時に持てるライブセッション（≒ ffmpeg
                                 # プロセス）数。**プロセスローカルな上限であり、
                                 # グローバルな天井（チューナー数、mirakc が裁定）では
                                 # ない**（operations.md §5「既定を 1 にする根拠」）
  idle_timeout: 30s              # サービス単位の idle GC 猶予。この時間セグメント要求が
                                 # 来なければ ffmpeg を止める（クライアント 1 人ごとの
                                 # 生存は追わない）
  tuner_priority: 1              # mirakc への X-Mirakurun-Priority。ruler が生成する
                                 # schedule の既定 priority（10）より低く保つことで、
                                 # チューナー枯渇時に録画側を常に勝たせる
                                 # （recording.md §2「チューナー調停」）。**Rokuban は
                                 # この不等式を強制しない** --- ルールの priority は
                                 # DB 側（rules.priority、既定 10）でユーザーが自由に
                                 # 変えられ、ライブ側の live.tuner_priority は config
                                 # 側なので、両者を跨いで比較検証する権威がどちらにも
                                 # 無い。ユーザーがルールの priority を 0 や 1 まで
                                 # 下げると、この既定値のままではライブが録画に勝って
                                 # しまう。運用者が両方の値を意識して選ぶ前提とする
  profiles:                     # 空配列は不可（enabled: true なら 1 つ以上必須）。
                                 # encode.profiles とは別の構造体 --- HLS はセグメント長・
                                 # プレイリスト長という VOD には無い制約を持つ
    - name: h264                 # `?profile=` から参照する一意な名前。英数字・
                                 # ハイフン・アンダースコアのみ（セグメントファイル名の
                                 # 接頭辞に使うため）
      video_codec: libx264       # ISDB-T の映像は MPEG-2 でブラウザの HLS 経路では
                                 # 事実上再生できないため、H.264 への変換が前提
      audio_codec: aac
      height: 720                # 0 or omit = スケールしない
      preset: veryfast           # optional
      segment_seconds: 2         # 0 なら既定値 2
      playlist_size: 6           # 0 なら既定値 6
      extra_args: []             # 末尾に追加する ffmpeg 引数

webhook:                         # 汎用 HTTP webhook（M3-11）。EPGStation の複数種外部コマンドを 1 本に置き換える
  url: ""                        # 空なら no-op。例: https://hooks.example.com/rokuban
  secret: ""                     # 非空なら X-Rokuban-Webhook-Secret ヘッダに載せる
  timeout: 5s                    # 1 回の HTTP 要求タイムアウト。失敗時は同期で 1 回だけ再試行
  events: []                     # 空なら既知の全イベント有効。絞る例: recording.finished / recording.failed
                                 # 本処理（ingest / encode / 削除 reconcile 等）は webhook 成否で止めない（at-least-once）
                                 # ペイロードは JSON。機微情報（絶対パス・credentials）は載せない

cleanup:                         # 削除 reconcile（M3-8、storage.md §7）
  trash_retention: 720h          # ごみ箱（recordings.deleted_at）の猶予。既定 30 日
  orphan_mtime_grace: 168h       # 孤児候補にするまでの mtime 猶予。既定 7 日
  orphan_age: 336h               # 孤児候補が実削除されるまでのエイジング期間。既定 14 日
  max_deletes_per_pass: 100      # 一括削除サーキットブレーカーの閾値。ソースを問わず
                                 # 1 パス全体の合計に対して働く（ruler と同じラッチ式）

log:
  level: info
  format: json                   # json | text
```

### 必須キー

最小 3 つ: `db`（資格情報）・`mirakc.url` または `mirakcs`（どちらか一方。下記「複数サイト」参照）・`storage.media_dir`。残りは全部デフォルトを持ち、最小構成は 10 行程度に収まる。

### mirakc は単一オブジェクト

`mirakc` は（複数サイト用の `mirakcs:` を使わない限り）単一オブジェクトとする。複数 mirakc を許すと programId のスコープが mirakc 単位になり、予約・EPG 射影・ingest の全スキーマに「どの mirakc か」が波及する。チューナー集約は mirakc 自身のリモートチューナー機能で賄えるため、単一サイトなら Rokuban 側は 1 エンドポイントで足りる。ハブ mirakc への集約は採らない（WAN リアルタイム依存 = 録画中の回線瞬断が録画欠損に直結するため）。

`mirakc.site` は DB の全テーブルの `site` 列の値になる（M3-1、issue #29 / #31 / #53）。API の資源同定（`/api/sites/{site}/...`）の権威は `config.mirakc`/`mirakcs` レジストリに site が存在するかであり、api 自身は不変条件 1 によりどの site にも束縛されない（issue #184 M4-12）。単一 `mirakc:` 構成ではレジストリがこの 1 要素だけになるので、実質 `mirakc.site` と一致する。**省略時の既定値は `"default"` だが、`site: ""` と明示すると起動エラーになる**（下記の site 名の構文制約が空文字列を許さないため。issue #183 M4-11 で `mirakcs:` レジストリを導入した際に、単一オブジェクト形式にも同じ制約を揃えた。従来は空文字列も黙って許容されていた）。

#### 複数サイト: `mirakcs:` レジストリ（issue #183 M4-11）

多拠点構成では `mirakc:` の代わりに `mirakcs:` （`{site, url}` の配列）を書く。`mirakc: {url, site}` は `mirakcs: [{site, url}]` の 1 要素と等価に解決される糖衣で、**両方を同時に書くと起動エラー**になる（どちらが勝つかを覚えさせない）。

- **`mirakcs:` の要素は `site` と `url` の 2 つだけ。** `storage` / `worker` / `ingest` 等のチューニング値は要素に入れない。アーカイブは単一（`media_assets` に site 列が無い）であり、`worker.queues` 等はデプロイ時のパラメータであって site の属性ではない。site ごとのチューニング値は、それを読むコードができたときに足す（不変条件 11）
- **site 名の構文制約**: `^[a-z0-9]([_-]?[a-z0-9])*$`、64 文字以内。**River のキュー名の制約と同一で、緩めない** --- キュー名を site で修飾する（issue #185 M4-13）ため、緩めると site 名がキュー名として弾かれる。ただし 64 文字はキュー名の prefix（例: `reconciler_`、11 文字）を見込んでいないため、修飾後に River の上限を超える長さの site は `--sites` の起動時検査（`worker.ValidateSiteForQueueNames`）で別途弾く。**この検査は束縛した site と `enqueue --site` にしか効かない** --- レジストリに載っているだけで束縛されていない site 名は検査対象外（未検証。`internal/config.mirakcSiteNameMaxLen` を締める案は issue #185 のコメントで提起した）
- **予約名**: `catalog` と `thumbnails` は site 名にできない。M4-11 導入時の根拠は「`rel_path` に `{site}/` を前置すると、この 2 つと衝突する site 名は削除 reconcile の孤児回収と rescue スキャンの走査対象から外れてしまう」だったが、実装された M4-14 の前置は `sites/{site}/`（site 名の前に固定の `sites/` を挟む形。[docs/storage.md](storage.md) §5「rel_path の名前空間」参照）になったため、site 名はトップレベルの `catalog/` / `thumbnails/` と直接衝突しなくなり、パス衝突というこの根拠は成立しなくなった。**ただし禁止自体は残している** --- 緩めても得られる自由度（`catalog` / `thumbnails` を site 名にしたい運用要求は無い）が、緩めるコスト（`internal/config` のバリデーション・テストの変更）に見合わないため（issue #186 のコメントで結論済み）。これはトップレベルディレクトリ名の予約（`catalog/` / `thumbnails/` / `sites/` の 3 つ。今も load-bearing）とは別の話で、docs/storage.md §5 で分けて説明している
- レジストリ内の site 名の重複も不可。違反はすべて起動エラーとして全件列挙される（規約 4）

**多サイトでどのロールがどのプロセスに乗るかは [docs/overview.md](overview.md) の役割分類に決定済み**（#138）: site に縛られるのは「mirakc に到達する必要がある仕事」（watcher / ingest / reconciler / epg / record_sweep / ライブ streamer）だけで、DB もアーカイブも単一なので、api / notifier / 録画配信 streamer / ruler / encode / thumbnail / 削除 reconcile は site 非依存の中央プロセスになる。

**プロセスがどのサイトに束縛されるかは `server` サブコマンドの `--sites` フラグで表す。config キーにはしない**（下記「やらないこと」の CLI フラグ方針に反しない --- `--all` / ロール選択と同じ「起動形態」の軸）。

- 未指定でレジストリが 1 要素ならその 1 つに束縛する。未指定でレジストリが 2 要素以上なら起動エラー（暗黙に「全部」にしない）
- `--sites=`（明示的な空）は束縛なし = 中央プロセス
- `--sites tokyo` は tokyo に束縛する。`--sites tokyo,tokyo` のような重複は 1 つに畳む（束縛数の判定が紛らわしいエラーにならないようにするため）
- `watcher` ロールは 1 プロセス 1 サイトのループしか持たないため、束縛サイト数がちょうど 1 でなければ起動エラーになる。watcher の advisory lock のキーも束縛サイトで修飾される（`watcher:<site>`）ので、2 サイトそれぞれに 1 プロセスずつ立てれば両方が自分の mirakc の SSE を購読する（issue #185 M4-13）
- `worker` ロールは今のところ site 単位の仕事（`ingest`/`epg`/`reconciler`/`watcher` キュー。キュー名は束縛サイトで `<論理名>_<site>` に修飾される）と site 非依存の仕事（`ruler`/`encode`/`thumbnail`/`cleanup`/`default` キュー。`catalog_export` / `delete_reconcile` はどちらもジョブ種別で、キューとしては `cleanup` に乗る。issue #185 M4-13）が同居しており（`worker.Deps.Site` / `worker.ClientConfig` の各 `*Site` フィールドがいずれも単一文字列のため）、2 サイト以上の束縛は起動エラーになる。**0 サイト（中央プロセス）の束縛は `worker.queues` を ruler/encode/thumbnail/cleanup/default 等の site 非依存キューに絞ったときだけ許す** --- `worker.queues` が空（既定=全キュー）のまま、または ingest/epg/reconciler/watcher のいずれかを含んだまま 0 サイトで起動すると、届く site 単位のジョブが空文字列 site と一致せず全滅して再試行し続けるだけになるため起動エラーにする。**1 プロセスが N サイトの watcher / worker のループを回す形は書き手がまだいないので決めない**（不変条件 11）
- `enqueue` サブコマンドは `--site` で投入先を選ぶ（未指定かつレジストリ 1 要素ならその 1 つ、2 要素以上なら必須）。M4-6 の CronJob がサイトごとに投入するため
- `rescue` / `shadow-diff` は単一サイト用のまま。`mirakcs:` が 2 要素以上の構成では明示的なエラーで落ちる（多サイトでの意味論を決める書き手がまだいないため）

### server.allowed_hosts は X-Forwarded-Host を優先する

`server.allowed_hosts` は Host ヘッダーの allowlist で、アプリ内に残る唯一のセキュリティ機構（DNS rebinding 対策、[api.md](api.md) §認証 帰結3）。localhost 系（`localhost` / `127.0.0.1` / `::1`）は、**`X-Forwarded-Host` が無く `Host` を直接使っているときに限り**、allowlist の設定に関わらず常に許可する。`X-Forwarded-Host` はクライアント側が自己申告できる値なので、転送値にも同じ緩和を適用すると `X-Forwarded-Host: localhost` で allowlist を素通りできてしまう。

リバースプロキシ前段では `Host` がプロキシ自身の値（例: コンテナ内部の DNS 名）に書き換わり、元のクライアント向け Host は `X-Forwarded-Host` に移る。**`X-Forwarded-Host` があればそちらを検証対象にし、無ければ `Host` を使う**（M4-1、issue #89）。nginx リファレンス構成では `proxy_set_header X-Forwarded-Host $host;` を設定し、`allowed_hosts` にはブラウザからアクセスする外部ホスト名（例: `rokuban.example.com`）を書く。**この構成では localhost 系の常時許可は効かなくなるので、nginx 経由で `http://localhost/` を見る運用なら `allowed_hosts` に明示的に列挙する。**

`X-Forwarded-For` / `X-Forwarded-Proto` / `X-Forwarded-Prefix` / `public_url` は解釈・設定項目とも持たない。理由は [api.md](api.md) §リバースプロキシ・フレンドリー要件「検討したが実装しないもの」を参照。

### db は構造化フィールド

`url:` 1 本の DSN ではなく構造化フィールドを採る。パスワードの URL エスケープ事故を避けられる。

### webhook のイベントとペイロード

`webhook.events` の allowlist に書ける type と、共通 envelope（M3-11 / issue #73）。

| type | 発火点 | 追加フィールド |
|---|---|---|
| `recording.finished` | watcher（`status` が finished へ遷移したとき 1 回） | — |
| `recording.failed` | watcher（mirakc の `recording.failed` イベントを受けるごと） | — |
| `encode.finished` | EncodeWorker（コミット成功時。冪等スキップでは発火しない） | `profile` |
| `encode.failed` | EncodeWorker（**River の試行ごと**） | `profile` / `attempt` / `maxAttempts` |
| `recording.deleted` | 削除 reconcile（ごみ箱の録画の**最後のアセット**を物理削除したとき 1 回） | — |

```json
{
  "id": "<配送単位の UUID>",
  "type": "encode.failed",
  "at": "<RFC3339 UTC>",
  "recordingId": 1,
  "site": "default",
  "title": "番組名",
  "status": "failed",
  "profile": "h264",
  "attempt": 3,
  "maxAttempts": 25
}
```

- **`id` は配送ごとに変わる。** 同じ論理イベントが再送されうるので、受け側の冪等キーには `(type, recordingId, profile, attempt)` を使う。エンコード失敗は River が再試行するため、恒久的な失敗では試行ごとに配送される（最終試行だけ通知したいなら `attempt == maxAttempts` で絞る）
- **`recording.deleted` は録画の寿命で決まる。** アセットの kind では決めない。`keep_original: until_encoded` の原本削除は「録画が消えた」ではないので発火せず（encoded で再生できる）、逆に原本を先に消してある録画のごみ箱削除では最後のアセットの削除で発火する
- 絶対パス・credentials は載せない。1 パスの通知に時間上限があり、超過分は捨ててログに件数を残す（削除の進捗を webhook 先に引きずられないため）

## 規約

1. **snake_case**: キーは snake_case（Prometheus/Loki/mirakc に揃える）。JSON API 側の camelCase とは世界が違うので混在してよい
2. **Go duration 文字列**: 期間は `30s` / `5m` の `time.Duration` 文字列（goccy/go-yaml のカスタム型 1 つで済む）
3. **strict パース**: 未知キーは起動失敗（goccy の strict モード）。`encode_profles` のような typo が黙ってデフォルト動作になるのは、未定義環境変数の全件列挙 fail-fast と同じ思想で塞ぐべき穴
4. **エラーは全件列挙**: 未定義環境変数・必須キー欠落ともに、検出できるものは全件列挙して起動失敗する（1 個ずつ直させない）

## config と DB の境界

**config = デプロイ環境の性質**（そのホスト/クラスタを再構築すると変わるもの）。**DB = 運用中に UI から変えたい意思**（ルール・予約・視聴履歴）。

この原則により**エンコードプロファイルは config 側**に落ちる。プロファイルの実体は「その環境の ffmpeg ビルドと HW (VAAPI/QSV/NVENC) で何ができるか」であり、`ffmpeg` パスと同じデプロイ属性。DB のルールからは名前参照とし、ルール保存時に存在検証（なければ **400**）。自由形式の cmd 文字列は採らない（構造化フィールドから worker が引数を組み立てる）。

## 運用補助

- **`rokuban config validate`**: env 展開 + strict パース + 必須検証だけやって exit。ConfigMap 更新前チェックや CI に置ける
- **`config.example.yml`**: コメント付きでリポジトリに置き、スキーマのリファレンスとする（ドキュメント生成までは YAGNI）

## やらないこと

- **env による config キーの自動オーバーライド**（viper/koanf の多層マージ）: どの値がどこから来たか追いにくくなる割に、この規模では利得がない
- **設定ファイルの分割・include 機構**: 分離の必要があるのは機微情報だけで、それは env で足りる
- **CLI フラグで設定値を渡すこと**: CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定する
- **SIGHUP 等のホットリロード**: 設定変更は再起動。crash-only + level-triggered 設計なら再起動は無害で、リロードは「どの値がいつから有効か」という追跡困難性を別の形で持ち込むだけ
