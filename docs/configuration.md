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

**キーの網羅の権威は [`config.example.yml`](../config.example.yml)**（コメント付き。リポジトリ直下）。ここに YAML を書き写すと片方だけ直って静かにずれるため、docs 側はキー / 既定値 / 一言の表に留め、判断（なぜその値か）だけを下記の各節に書く。既定値の実装は `internal/config/config.go`（`defaults()` と各 `applyDefaults` / 呼び出し先の既定）。

| キー | 既定値 | 説明 |
|---|---|---|
| `server.listen` | `":40773"` | HTTP リッスンアドレス（mirakc の 40772 の隣） |
| `server.allowed_hosts` | `[]` | Host ヘッダー allowlist（下記「server.allowed_hosts」） |
| `server.trust_forwarded_host` | `false` | `X-Forwarded-Host` を allowed_hosts の検証対象にするか（下記「server.allowed_hosts」） |
| `db.host` / `db.user` / `db.password` / `db.database` | —（必須） | PostgreSQL 接続情報 |
| `db.port` | `5432` | |
| `db.sslmode` | `disable` | |
| `db.max_conns` | `0`（roles から自動算出） | プロセスが持つ唯一のプールの上限（下記「db の運用ノブ」） |
| `db.api_statement_timeout` | `0`（= 30s） | api ロールを含むプロセスにだけ適用（同上） |
| `db.pooler_compat` | `false` | transaction pooling 互換モード（同上） |
| `mirakc.url` | —（`mirakcs` との排他でどちらか必須） | 単一サイトの mirakc エンドポイント |
| `mirakc.site` | `default` | サイト名（下記「mirakc は単一オブジェクト」） |
| `mirakcs` | — | `{site, url}` の配列（下記「複数サイト」） |
| `storage.media_dir` | —（必須） | アーカイブ層（S3-via-CSI 可） |
| `storage.scratch_dir` | `/var/tmp/rokuban` | ローカルスクラッチ |
| `storage.accel_location` | `""`（Go が直接配る） | X-Accel-Redirect の internal location |
| `ingest.concurrency` | `2` | mirakc サイトあたりの同時転送数 |
| `ingest.stall_timeout` | `30s` | 転送の無進捗検知（下記「ingest.stall_timeout」） |
| `epg.sync_interval` | `10m` | mirakc から EPG を全量取得する間隔 |
| `epg.retention_grace` | `24h` | 放送終了から番組を刈り取るまでの猶予。ruler の GC（予約と番組単位の意図）も同じ猶予を使う。**エッジの滞留を N 日許すなら N 以上が要る**（[ストレージ](storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」） |
| `ruler.max_deletes_per_pass` | `0`（= 50） | 大量削除サーキットブレーカーの閾値（下記「サーキットブレーカーと検出器の閾値」） |
| `reconciler.start_delay_grace` | `0`（= 3m） | 開始遅延検出器の猶予（同上） |
| `worker.periodic_jobs` | `true` | プロセス内で定期ジョブを投入するか（下記「worker.periodic_jobs と worker.queues」） |
| `worker.queues` | `[]`（全キュー） | worker ロールが引くキューの絞り込み（同上） |
| `encode.ffmpeg` / `encode.ffprobe` | `ffmpeg` / `ffprobe` | PATH 検索。フルパス指定可（下記「ffmpeg の存在検査」） |
| `encode.concurrency` | `1` | encode キューの MaxWorkers（ingest とは独立） |
| `encode.thumbnail_concurrency` | `1` | thumbnail キューの MaxWorkers |
| `encode.profiles` | `[]` | 構造化エンコードプロファイル（形は `config.example.yml`。下記「config と DB の境界」「encode/live の HW エンコード」） |
| `live.enabled` | `false` | ライブ視聴のルートを登録するか（下記「live」） |
| `live.ffmpeg` | `ffmpeg` | PATH 検索 |
| `live.segment_dir` | —（`enabled: true` なら必須） | HLS セグメントの書き出し先（下記「live」） |
| `live.max_sessions` | `4` | プロセスローカルの同時セッション上限（同上） |
| `live.idle_timeout` | `30s` | サービス単位の idle GC 猶予 |
| `live.tuner_priority` | `1` | mirakc への X-Mirakurun-Priority（同上） |
| `live.hwaccel` / `live.input_extra_args` | なし | live セクション直下の HW アクセラレーションブロック / 入力側追加引数（下記「encode/live の HW エンコード」） |
| `live.profiles` | —（`enabled: true` なら 1 つ以上必須） | HLS 用プロファイル（`scaler` / `crf` / `qp` を含む形は `config.example.yml`。`segment_seconds` 既定 2 / `playlist_size` 既定 6） |
| `webhook.url` | `""`（no-op） | POST 先（下記「webhook のイベントとペイロード」） |
| `webhook.secret` | `""` | 非空なら `X-Rokuban-Webhook-Secret` ヘッダに載せる |
| `webhook.timeout` | `5s` | 1 回の HTTP 要求タイムアウト。失敗時は同期で 1 回だけ再試行 |
| `webhook.events` | `[]`（全イベント有効） | 配送イベントの allowlist |
| `cleanup.trash_retention` | `0`（= 30 日） | ごみ箱（`recordings.deleted_at`）の猶予（[storage.md](storage.md) §7） |
| `cleanup.orphan_mtime_grace` | `0`（= 7 日） | 孤児候補にするまでの mtime 猶予 |
| `cleanup.orphan_age` | `0`（= 14 日） | 孤児候補が実削除されるまでのエイジング期間 |
| `cleanup.max_deletes_per_pass` | `0`（= 100） | 一括削除サーキットブレーカーの閾値。ソースを問わず 1 パス全体の合計に対して働く（ruler と同じラッチ式） |
| `log.level` | `info` | debug / info / warn / error |
| `log.format` | `json` | json / text |

### 必須キー

最小 3 つ: `db`（資格情報）・`mirakc.url` または `mirakcs`（どちらか一方。下記「複数サイト」参照）・`storage.media_dir`。残りは全部デフォルトを持ち、最小構成は 10 行程度に収まる。

### db の運用ノブ（max_conns / api_statement_timeout / pooler_compat）

3 つとも詳細は [operations.md](operations.md) §3「DB 運用」に決まっている。要点だけ:

- `db.max_conns`: プロセスは常に 1 個のプールしか持たず全ロールが共有する。未指定（0）なら起動時の roles からロール別 budget を合計して自動算出する
- `db.api_statement_timeout`: api ロールを含むプロセスのプール全体に適用される（monolith では worker 側にもかかる）。既定 30s
- `db.pooler_compat`: pooler（transaction pooling）を通せるのは api ロールと streamer ロールだけで、worker/watcher/notifier との組み合わせは起動時エラー

### ingest.stall_timeout

転送中の無進捗検知。この時間バイトが進まないと切断して Range 再開する。River の総時間タイムアウトは無効化しているため、**これが ingest の唯一のタイムアウト**（[recording.md](recording.md) §5.3）。総時間ではなく無進捗で切るのは、総時間タイムアウトが遅い回線の正常な転送を殺すため。遅い回線や大きな録画で誤爆する場合は延ばす。

### サーキットブレーカーと検出器の閾値

- `ruler.max_deletes_per_pass`: 1 サイト・1 パスで実行してよい導出削除数の上限。超えたら削除を一切実行せず、大量削除サーキットブレーカーとして発動する。数えるのは「ルールが base を供給しているのに desired から外れた」削除だけである。**ユーザーが投資を手放す書き込み（intent skip / intent クリア / 最後の investment だった overrides の削除）をしない限り起きない削除は数にも入らず、発動中でも実行される**。境界も同節にある（[recording.md](recording.md) §3.2）。手動で `POST /api/sites/{site}/breakers/ruler_deletes/resume` するまで止まり続けるラッチで、件数が閾値以下に戻っても自動では解けない（[recording.md](recording.md) §3.2「大量削除サーキットブレーカー」）。下げると EPG の一時的な欠損やルールの一括編集で発動しやすくなる代わりに、誤判定で一気に失う予約の上限が下がる。既定値 50 は、通常運用で 1 サイトが同時に抱えるアクティブ予約数を明確に超える値として選んである
- `reconciler.start_delay_grace`: 開始遅延検出器（[recording.md](recording.md) §3.3）の猶予。「開始時刻 + この時間」を過ぎても `recordings.started_at` が観測されない予約を検出してアラートする。mirakc 側の未知の不具合への保険（EPGStation#724: チューナー再接続ハングで開始が 10 分遅延した実例あり）。開始直後は mirakc の SSE 到達と watcher の処理に遅れがあるため、短すぎると正常な録画開始でも誤検知してアラートが常時鳴る

### worker.periodic_jobs と worker.queues

- `worker.periodic_jobs`: プロセス内で定期ジョブを投入するか。対象は epg_sync / tuner_sync / ruler_pass / reconcile_pass / record_sweep。catalog_export / delete_reconcile / encode_reconcile / storage_sync も対象。k8s では false にし、CronJob から `rokuban enqueue` で投入する（River の PeriodicJobs はリーダーだけが投入するため、KEDA で 0 にスケールすると誰も投入しなくなる。[data.md](data.md) §2）
- `worker.queues`: worker ロールが引くキューを絞る。空なら全部。ロールを増やさずに「ruler / reconciler だけ別 Pod」を実現するための knob。書くのは物理名ではなく**論理名**。使えるのは `ingest` / `epg` / `ruler` / `reconciler` / `watcher` / `encode`。`thumbnail` / `cleanup` / `storage` / `default` も使える。site 単位のキューの物理名への展開・ロールとの関係（worker ロールが無いプロセスはこの設定に関わらずキューを引かない）は [operations.md](operations.md) §5 を参照

### ffmpeg の存在検査

`encode.ffmpeg` / `encode.ffprobe` は、worker ロールが encode/thumbnail キューを購読するときだけ LookPath で存在検査する。購読するのは `worker.queues` が空、または encode/thumbnail を含むときである。api ロールは呼ばない（不変条件 4）。`live.ffmpeg` は `live.enabled: true` の streamer 起動時だけ検査する。

### live

ライブのルートを登録するのは streamer ロールだけ。`enabled: false`（既定）なら公式イメージ（ffmpeg 無し）で streamer を起動する構成（録画配信 / サムネイルのみ）を壊さない（不変条件 4）。

**`live.enabled` だけは公開面（`GET /api/capabilities` の `live`）にも出る**。フロントは config を読めないため、これが無いと無効な機能への導線を出し続ける。ロール分割構成でも config ファイルは共有する（上記「関心事別にセクションを切る」）ので、値はそのまま公開面に写す --- streamer に問い合わせはしない（不変条件 1）。

**この値を返すのは api ロールのプロセスに限らない**。REST の生成ルートはロールで絞られないので、HTTP を持つどのプロセスに聞いても同じ答えになる（そう**しない**とロールごとに答えが割れる。[api/rest.md](api/rest.md)「機能の有効/無効は能力 API で観測する」）。したがって答えが表すのは常に「この config が有効か」であり、**「api の config では有効だが streamer を動かしていない」構成では導線が出る**（押した先はプレイリストの 404）。

- `live.segment_dir` は**既定値を持たない --- `enabled: true` なら必須**（空だと起動時エラー。適当なローカルパスへ黙ってフォールバックしない）。録画バッファ（mirakc `recording.basedir`）とは別ディスク（tmpfs 前提。k8s なら `emptyDir: {medium: Memory}`）。起動時（HTTP リスナーが立つ前）にこのディレクトリの**中身**を掃く。tmpfs はノード再起動でしか消えないため、前回プロセスの残骸（SIGKILL 等で SIGTERM の後始末が効かなかった場合）が残っていても毎起動で必ず消える。**`segment_dir` 自体は消さない**。k8s の `emptyDir` を `segment_dir` に直接マウントする構成では、Linux はマウントポイント自体への rmdir を EBUSY で拒む（Linux コンテナで実測済み）。中身だけを掃く実装でなければ毎起動 Warn ログが出続ける。推奨値 `/dev/shm/rokuban-live` のように tmpfs の**サブディレクトリ**を掘って使う構成では、そのサブディレクトリ自体はマウントポイントではないのでこの問題は起きない。**複数プロセス / 複数レプリカで共有しない**。起動時の掃除は「自分がこのパスの唯一の書き手である」という前提に立っており、共有すると後発の起動が先発の生きているセッションディレクトリを消してしまう（視聴は一度途切れるが、次のプレイリスト要求で新しいセッションが起きて自己修復する）
- `live.max_sessions` はこのプロセスが同時に持てるライブセッション（≒ ffmpeg プロセス）数。**プロセスローカルな上限であり、グローバルな天井（チューナー数、mirakc が裁定）ではない**（[operations.md](operations.md) §5「既定を 1 にする根拠」）
- `live.idle_timeout` はサービス単位の idle GC 猶予。この時間セグメント要求が来なければ ffmpeg を止める（クライアント 1 人ごとの生存は追わない）。**離脱ヒント（`POST .../live/leave`）を受けたときに詰める短い猶予は設定キーにしない**。`3 × 最長の live.profiles[].segment_seconds + 2s`（既定 8 秒）として導出する。**`idle_timeout` ではクリップしない**。クリップすると `segment_seconds: 6` + `idle_timeout: 2s` のような設定で猶予がセグメント長を下回り、leave が「他人の視聴を切る道具」に化ける。猶予が `idle_timeout` 以上になる設定では、ヒントは詰める先が「いま」より後ろにならないので no-op になる（起動を止めるより運用者の意思を尊重する）。この猶予は「生きている視聴者の次の要求が来るまでの間隔」より長くなければならない。その間隔を決めているのはセグメント長そのものなので、独立したキーにすると 2 つの値が矛盾する組み合わせを運用者が書けてしまう（[api.md](api.md) §ライブ視聴の HLS「離脱は『ヒント』であって停止命令ではない」）。config と DB の境界と同じく、**別の値から必ず導けるものはキーにしない**
- `live.tuner_priority` は mirakc への X-Mirakurun-Priority。ruler が生成する schedule の既定 priority（10）より低く保つことで、チューナー枯渇時に録画側を常に勝たせる（[recording.md](recording.md) §2「チューナー調停」）。**Rokuban はこの不等式を強制しない**。ルールの priority は DB 側（`rules.priority`、既定 10）でユーザーが自由に変えられ、ライブ側の `live.tuner_priority` は config 側なので、両者を跨いで比較検証する権威がどちらにも無い。ユーザーがルールの priority を 0 や 1 まで下げると、この既定値のままではライブが録画に勝ってしまう。運用者が両方の値を意識して選ぶ前提とする
- `live.profiles` は `encode.profiles` とは別の構造体 --- HLS はセグメント長・プレイリスト長という VOD には無い制約を持つ。`name` は `?profile=` から参照する一意な名前で、英数字・ハイフン・アンダースコアのみ（セグメントファイル名の接頭辞に使うため）。ISDB-T の映像は MPEG-2 でブラウザの HLS 経路では事実上再生できないため、H.264 への変換が前提

### encode/live の HW エンコード

構造化のまま HW エンコード（VAAPI 等）を表現する。追加するキーは「`extra_args` では構造的に届かない位置（`-i` より前のオプションと、アプリが `-vf` を握っているスケール filter）」に限る。コーデック指定より後ろのオプション（`extra_args` が既に出ている位置）は既存のキーで足りるので構造化しない。

| キー | 説明 |
|---|---|
| `encode.profiles[].scaler` / `live.profiles[].scaler` | スケール filter の系統名（`-vf` の filter 文字列そのものではない）。既定 `""`（= `software`）。**許す値は `software` と `vaapi` のみ** --- qsv / cuda はこの環境の ffmpeg で `scale_qsv` / `scale_cuda` の綴りを確認できておらず未検証のため除外してある。`height` が 0 のときに書くと起動エラー |
| `encode.profiles[].qp` / `live.profiles[].qp` | `-qp`（品質指定）。`crf` との同時指定は起動エラー（優先順位を実行時に決めさせない） |
| `encode.profiles[].hwaccel` | プロファイル毎の `-i` 前置ブロック（`kind` 必須 / `device` / `output_format` 任意）。VOD はプロファイルごとに入力を開き直すため、プロファイル単位で持たせられる |
| `live.hwaccel` | **`live.profiles[]` 内ではなく `live:` 直下**。ライブは 1 回の ffmpeg で入力 1 本・出力 N 本なので、プロファイル毎に持たせると「プロファイル 2 つが別の hwaccel を要求する」という表現できない設定が書けてしまう --- セクション直下に置けばそれが表現不可能になる |
| `encode.profiles[].input_extra_args` / `live.input_extra_args` | `-i`（VOD）/ `-f mpegts -i pipe:0`（live）の直前に追加する引数 |
| `encode.profiles[].extra_args` / `live.profiles[].extra_args` | 既存キー。改名していない --- ただし VOD 側は位置が 1 点だけ動く（下記） |

argv の順序（VOD）:

```
-hide_banner -nostats -y                                      # アプリ
[-hwaccel K] [-hwaccel_device D] [-hwaccel_output_format F]   # hwaccel ブロック
[input_extra_args…]                                            # ユーザー（入力側）
-i INPUT                                                       # アプリ
-c:v VC -c:a AC
[-vf <scaler が決めた filter>]                                 # height>0 のときだけ、常に 1 個
[-crf N | -qp N] [-preset P]
[extra_args…]                                                  # ユーザー（出力側）
-f CONTAINER -progress pipe:1 -loglevel error OUTPUT           # アプリ所有の末尾
```

argv の順序（live）は同じ規則を入力 1 本・出力 N 本の形に展開したものである。入力側は `live.hwaccel` → `-probesize`/`-analyzeduration` → `live.input_extra_args` → `-f mpegts -i pipe:0`。そのあと、プロファイルごとに `-map` `-c:v`/`-c:a` → `[-vf]` → `[-crf|-qp]` → `[-preset]`。続けて `-force_key_frames` → `profile.extra_args` → `-f hls ...`。

**`extra_args` の位置が 1 点だけ動いた**。VOD 側は以前 `-f`（コンテナ）の後ろだったが、今は前に移った --- 「ユーザーのオプションはコーデック/品質/スケール指定の後・アプリ所有の末尾の前」という規則を VOD と live で 1 つにするため。`-f` は下記の allowlist に含まれないので、この移動でユーザーが相対順序に依存していた挙動が変わることはない。

**起動エラーになる組み合わせ**: `crf` と `qp` の同時指定 / 未知の `scaler` / `height` が 0 なのに `scaler` を書く / `hwaccel` ブロックがあるのに `kind` が空 / `crf`・`qp` の負値。

**`extra_args` / `input_extra_args` は値の個数まで既知の allowlist だけを受け付ける**。値を取らないのは `-an` `-vn` `-sn` `-dn` `-shortest` `-nostdin` `-re`。直後の 1 トークンを値として取るのは `-movflags` `-map` `-global_quality` `-cq` `-q:v` `-b:v` `-b:a`。`-probesize` `-analyzeduration` `-extra_hw_frames` も 1 トークンを取る。それ以外と裸の位置引数は起動エラーになる。値を取らないフラグも明示しているため、`["-an", "/tmp/evil.mp4"]` のように 2 本目の出力パスをフラグの値に見せかけることはできない。`-filter:v:0` / `-lavfi` のような filtergraph の別名・ストリーム指定子付き表記も allowlist 外であり、完全一致の denylist が別名を取りこぼす形は採らない。

**範囲外**: device ノードのマウントはデプロイ側（k8s `resources.limits` / Docker `--device`）。**`hwaccel.device` の存在は起動時に検査しない** --- 公式イメージや device の無い CI を壊す。無い device を書いたプロファイルはジョブ / セッションの失敗として現れる。`-global_quality` / `-cq` / `-q:v` のような、コーデック指定より後ろに出せる（= `extra_args` で届く）品質オプションはキー化しない。

### mirakc は単一オブジェクト

`mirakc` は（複数サイト用の `mirakcs:` を使わない限り）単一オブジェクトとする。複数 mirakc を許すと programId のスコープが mirakc 単位になり、予約・EPG 射影・ingest の全スキーマに「どの mirakc か」が波及する。チューナー集約は mirakc 自身のリモートチューナー機能で賄えるため、単一サイトなら Rokuban 側は 1 エンドポイントで足りる。ハブ mirakc への集約は採らない（WAN リアルタイム依存 = 録画中の回線瞬断が録画欠損に直結するため）。

`mirakc.site` は DB の全テーブルの `site` 列の値になる。API の資源同定（`/api/sites/{site}/...`）の権威は `config.mirakc`/`mirakcs` レジストリに site が存在するかであり、api 自身は不変条件 1 によりどの site にも束縛されない。単一 `mirakc:` 構成ではレジストリがこの 1 要素だけになるので、実質 `mirakc.site` と一致する。**省略時の既定値は `"default"` だが、`site: ""` と明示すると起動エラーになる**（下記の site 名の構文制約が空文字列を許さないため）。

#### 複数サイト: `mirakcs:` レジストリ

多拠点構成では `mirakc:` の代わりに `mirakcs:` （`{site, url}` の配列）を書く。`mirakc: {url, site}` は `mirakcs: [{site, url}]` の 1 要素と等価に解決される糖衣で、**両方を同時に書くと起動エラー**になる（どちらが勝つかを覚えさせない）。

- **相互排他は「`mirakc:` キーを書いたか」で判定する**。**「`mirakc.url` が非空か」ではない**。url を欠いた `mirakc: {site: tokyo}` は「書いていない」ではなく「url が足りない `mirakc:`」として扱われる。`mirakcs:` と併記すれば相互排他エラー、単独なら `mirakc.url is required` になる。**既定値を先に埋める設計では、「書かれたか」は値から復元できない**。既定値が入る `mirakc.site` は「書かれていない」と区別できない。値で判定すると `mirakc: {site: tokyo}` + `mirakcs:` の併記が検査を素通りして**書いた `mirakc.site` が黙って捨てられる**。判定はキーの有無で行う（`detectMirakcKeyWritten` が probe で復元する）

- **`mirakcs:` の要素は `site` と `url` の 2 つだけ**。`storage` / `worker` / `ingest` 等のチューニング値は要素に入れない。アーカイブは単一（`media_assets` に site 列が無い）であり、`worker.queues` 等はデプロイ時のパラメータであって site の属性ではない。site ごとのチューニング値は、それを読むコードができたときに足す（不変条件 11）
- **site 名の構文制約**: `^[a-z0-9]([_-]?[a-z0-9])*$`、53 文字以内。**文字種は River のキュー名の制約と同一で、緩めない** --- キュー名を site で修飾するため、緩めると site 名がキュー名として弾かれる。**上限の 53 は、River のキュー名上限 64 から、site 修飾される論理キューのうち最長の prefix（`reconciler_`、11 文字）を引いた値**（64 − 11 = 53）。この検査は**設定のロード時**に行うので、`--sites` で束縛していないレジストリ上の site 名も対象になる。束縛した瞬間に初めて起動エラーになる、ということは無い。`--sites` で実際に束縛したサイト、および `enqueue --site` で指定したサイトについては `worker.ValidateSiteForQueueNames` が同じ関係をキュー修飾後の名前で再検査する。config 以外の経路から site 名が来る場合の最後の砦である
- **予約名**: `catalog` と `thumbnails` は site 名にできない。**この禁止はもう load-bearing ではない** --- `rel_path` の前置が `sites/{site}/` になったので、site 名がトップレベルの `catalog/` / `thumbnails/` と直接衝突することはない。緩めても得られる自由度（この 2 語を site 名にしたい運用要求は無い）が緩めるコスト（`internal/config` のバリデーション・テストの変更）に見合わないため、禁止だけ残してある。トップレベルディレクトリ名の予約（`catalog/` / `thumbnails/` / `sites/` の 3 つ。今も load-bearing）とは別の話で、[docs/storage.md](storage.md) §5 で分けて説明している
- レジストリ内の site 名の重複も不可。違反はすべて起動エラーとして全件列挙される（規約 4）

**多サイトでどのロールがどのプロセスに乗るかは [docs/overview.md](overview.md) の役割分類に決定済み**。site に縛られるのは「mirakc に到達する必要がある仕事」（watcher / ingest / reconciler / epg / record_sweep / ライブ streamer）だけである。DB もアーカイブも単一なので、api / notifier / 録画配信 streamer / ruler / encode / thumbnail / 削除 reconcile は site 非依存の中央プロセスになる。

**プロセスがどのサイトに束縛されるかは `server` サブコマンドの `--sites` フラグで表す**。**config キーにはしない**（下記「やらないこと」の CLI フラグ方針に反しない --- `--all` / ロール選択と同じ「起動形態」の軸）。

- 未指定でレジストリが 1 要素ならその 1 つに束縛する。未指定でレジストリが 2 要素以上なら起動エラー（暗黙に「全部」にしない）
- `--sites=`（明示的な空）は束縛なし = 中央プロセス
- `--sites tokyo` は tokyo に束縛する。`--sites tokyo,tokyo` のような重複は 1 つに畳む（束縛数の判定が紛らわしいエラーにならないようにするため）
- `watcher` ロールは 1 プロセス 1 サイトのループしか持たないため、束縛サイト数がちょうど 1 でなければ起動エラーになる。watcher の advisory lock のキーも束縛サイトで修飾される（`watcher:<site>`）ので、2 サイトそれぞれに 1 プロセスずつ立てれば両方が自分の mirakc の SSE を購読する
- `worker` ロールは site 単位の仕事と site 非依存の仕事が同居している（`worker.Deps.Site` / `worker.ClientConfig` の各 `*Site` フィールドがいずれも単一文字列のため）。2 サイト以上の束縛は起動エラーになる。**0 サイト（中央プロセス）の束縛は `worker.queues` を site 非依存キューに絞ったときだけ許す**。`worker.queues` が空（既定=全キュー）のまま、または site 単位のキューを含んだまま 0 サイトで起動すると、届く site 単位のジョブが空文字列 site と一致せず全滅して再試行し続けるだけになる。そのため起動エラーにする。どのキューが site 単位か・物理キュー名への展開は [operations.md](operations.md) §5 を参照。**1 プロセスが N サイトの watcher / worker のループを回す形は書き手がまだいないので決めない**（不変条件 11）
- `enqueue` サブコマンドは **site 束縛ジョブだけ** `--site` で投入先を選ぶ（未指定かつレジストリ 1 要素ならその 1 つ、2 要素以上なら必須）。`catalog-export` は site 非依存で `--site` を付けない（詳細は [operations.md](operations.md) §1「ジョブ化されたループの監視」）
- `rescue` / `shadow-diff` は単一サイト用のまま。`mirakcs:` が 2 要素以上の構成では明示的なエラーで落ちる（多サイトでの意味論を決める書き手がまだいないため）

### server.allowed_hosts と server.trust_forwarded_host（X-Forwarded-Host は opt-in）

`server.allowed_hosts` は Host ヘッダーの allowlist で、アプリ内に残る唯一のセキュリティ機構（DNS rebinding 対策、[api.md](api.md) §認証 帰結3）。localhost 系（`localhost` / `127.0.0.1` / `::1`）は、**`X-Forwarded-Host` を検証対象にしておらず `Host` を直接使っているときに限り**、allowlist の設定に関わらず常に許可する。`X-Forwarded-Host` はクライアント側が自己申告できる値なので、転送値にも同じ緩和を適用すると `X-Forwarded-Host: localhost` で allowlist を素通りできてしまう。

`allowed_hosts` が空でなく、かつ `/healthz` / `/metrics` 以外のパスでは、判定の入力は **`Host` ヘッダー**（ポート部が 1 文字以上の数字だけで構成されるときに限りそれを落としたもの）である。ポート部が数字だけでない場合（数値でない場合や空の場合を含む）は落とさず入力全体を比較対象にするため、`allowed_hosts` のどの値とも一致せず拒否される（fail-closed）。`trust_forwarded_host: true` のときは `X-Forwarded-Host` の先頭要素を使う。この条件下では、リクエストラインに absolute-form（`GET http://host/path HTTP/1.1`）でホストを書いた要求は 400 で拒否する。リクエストラインからこの allowlist を満たすことはできない。`net/http` は absolute-form の request-target が来ると `Host` ヘッダーを常に削除してしまう。そのためサーバー側からは「リクエストラインと `Host` ヘッダーが食い違うか」を判定できない。比較は正規化後の完全一致で、正規化は「ASCII 範囲だけの小文字化」と「末尾ドット 1 個の除去」の 2 つだけを行う。`Host`（または `X-Forwarded-Host`）側と `allowed_hosts` の両側に同じ正規化が掛かる（`allowed_hosts` に大文字を書いてもよい）。接尾辞・ワイルドカードの一致は無い。`allowed_hosts` が空（開発用にチェック無効）のときはこの節の検証自体が丸ごとスキップされ、absolute-form も拒否されない。`/healthz` / `/metrics` の Host allowlist 免除は [operations/monitoring.md](operations/monitoring.md) を参照。

**`X-Forwarded-Host` は `server.trust_forwarded_host: true` を明示した構成でのみ検証対象にする（既定 `false`）**。DNS rebinding の攻撃ページはブラウザから見て Rokuban と同一オリジンなので、CORS のプリフライトなしに任意のリクエストヘッダーを付けられる。前段にリバースプロキシが存在しない直接露出構成（`--all` の既定構成）で `X-Forwarded-Host` を無条件に信頼する場合を考える。攻撃ページが `X-Forwarded-Host: <allowlist に載っている値>` を自己申告するだけで allowlist を素通りできてしまう。この値は `config.example.yml` に載る類の推測可能な名前である。前段に正しいリバースプロキシが居て、かつそのプロキシが外来の `X-Forwarded-Host` を必ず上書きする構成に限り、この設定を有効にしてよい。

`trust_forwarded_host: true` の構成では、`Host` がプロキシ自身の値（例: コンテナ内部の DNS 名）に書き換わり、元のクライアント向け Host は `X-Forwarded-Host` に移る。これを前提に、**`X-Forwarded-Host` があればそちらを検証対象にし、無ければ `Host` を使う**。nginx リファレンス構成では `proxy_set_header X-Forwarded-Host $host;` を設定する。`allowed_hosts` にはブラウザからアクセスする外部ホスト名（例: `rokuban.example.com`）を書く。**この構成では localhost 系の常時許可は効かなくなるので、nginx 経由で `http://localhost/` を見る運用なら `allowed_hosts` に明示的に列挙する**。

`trust_forwarded_host` が既定の `false` のままの構成（直接露出）では `X-Forwarded-Host` を一切見ず、常に `Host` で検証する。リバースプロキシ構成でこれを有効にし忘れると `Host` がプロキシ自身の値のまま allowlist に弾かれるだけで、安全側に倒れる（実害は 400 で気付ける）。

`X-Forwarded-For` / `X-Forwarded-Proto` / `X-Forwarded-Prefix` / `public_url` は解釈・設定項目とも持たない。理由は [api.md](api.md) §リバースプロキシ・フレンドリー要件「検討したが実装しないもの」を参照。

### db は構造化フィールド

`url:` 1 本の DSN ではなく構造化フィールドを採る。パスワードの URL エスケープ事故を避けられる。

### webhook のイベントとペイロード

`webhook.events` の allowlist に書ける type と、共通 envelope。

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

- **`id` は配送ごとに変わる**。同じ論理イベントが再送されうるので、受け側の冪等キーには `(type, recordingId, profile, attempt)` を使う。エンコード失敗は River が再試行するため、恒久的な失敗では試行ごとに配送される（最終試行だけ通知したいなら `attempt == maxAttempts` で絞る）
- **`recording.deleted` は録画の寿命で決まる**。アセットの kind では決めない。`keep_original: until_encoded` の原本削除は「録画が消えた」ではないので発火せず（encoded で再生できる）、逆に原本を先に消してある録画のごみ箱削除では最後のアセットの削除で発火する
- 絶対パス・credentials は載せない。1 パスの通知に時間上限があり、超過分は捨ててログに件数を残す（削除の進捗を webhook 先に引きずられないため）

## 規約

1. **snake_case**: キーは snake_case（Prometheus/Loki/mirakc に揃える）。JSON API 側の camelCase とは世界が違うので混在してよい
2. **Go duration 文字列**: 期間は `30s` / `5m` の `time.Duration` 文字列（goccy/go-yaml のカスタム型 1 つで済む）
3. **strict パース**: 未知キーは起動失敗（goccy の strict モード）。`encode_profles` のような typo が黙ってデフォルト動作になるのは、未定義環境変数の全件列挙 fail-fast と同じ思想で塞ぐべき穴
4. **エラーは全件列挙**: 未定義環境変数・必須キー欠落ともに、検出できるものは全件列挙して起動失敗する（1 個ずつ直させない）

## config と DB の境界

**config = デプロイ環境の性質**（そのホスト/クラスタを再構築すると変わるもの）。**DB = 運用中に UI から変えたい意思**（ルール・予約・視聴履歴）。

この原則により**エンコードプロファイルは config 側**に落ちる。プロファイルの実体は「その環境の ffmpeg ビルドと HW (VAAPI/QSV/NVENC) で何ができるか」であり、`ffmpeg` パスと同じデプロイ属性。DB のルールからは名前参照とし、ルール保存時に存在検証（なければ **400**）。自由形式の cmd 文字列は採らない（構造化フィールドから worker が引数を組み立てる）。HW エンコードも同じ構造化フィールドで表す（下記「encode/live の HW エンコード」）。**`-vf`（filtergraph）を直接書けるキーも作らない**。filtergraph は第 2 のコマンド言語であり、`scale_vaapi=...,drawtext=...` のような式が書けた時点で cmd を別名で解禁したのと同じになる。

## 運用補助

- **`rokuban config validate`**: env 展開 + strict パース + 必須検証だけやって exit。ConfigMap 更新前チェックや CI に置ける
- **`config.example.yml`**: コメント付きでリポジトリに置き、**キーの網羅の権威**とする（ドキュメント生成までは YAGNI）

## やらないこと

- **env による config キーの自動オーバーライド**（viper/koanf の多層マージ）: どの値がどこから来たか追いにくくなる割に、この規模では利得がない
- **設定ファイルの分割・include 機構**: 分離の必要があるのは機微情報だけで、それは env で足りる
- **CLI フラグで設定値を渡すこと**: CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定する
- **SIGHUP 等のホットリロード**: 設定変更は再起動。crash-only + level-triggered 設計なら再起動は無害で、リロードは「どの値がいつから有効か」という追跡困難性を別の形で持ち込むだけ

