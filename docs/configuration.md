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
| `mirakcs` | —（必須。既定なし） | mirakc エンドポイントの `{site, url}` 配列。`site` も既定なしで各要素必須（下記「mirakc レジストリ」） |
| `storage.media_dir` | —（必須） | 原本 ingest 先。fsync・close・同一 FS rename・親ディレクトリ fsync を保証するローカル FS / JuiceFS / NFS。FUSE S3 は原本 ingest 先にしない |
| `storage.scratch_dir` | `/var/tmp/rokuban` | ローカルスクラッチ。worker ロールでは空にできない（起動時に拒否する） |
| `storage.accel_location` | `""`（Go が直接配る） | X-Accel-Redirect の internal location |
| `ingest.concurrency` | `3` | mirakc サイトあたりの同時転送数。`チューナー数 + 全速 pull の許容本数（1〜2）` で、既定 3 は 2 チューナー機の下端（[録画](recording.md) §5.4）。4 チューナー機では 5〜6。**接続プールの予算はこの値に追随する**（[DB 運用](operations/database.md)） |
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
| `encode.profiles` | `[]` | 構造化エンコードプロファイル（形は `config.example.yml`。`rate` は rescue 締切の倍率、既定 4.0） |
| `cm_detect.enabled` | `false` | ingest 時に CM 検出を録画へ凍結し、worker の定期投入を有効にする |
| `cm_detect.binary_dir` | `/usr/local/bin` | JLSE の `logoframe` / `chapter_exe` / `join_logo_scp` 配置先 |
| `live.enabled` | `false` | ライブ視聴のルートを登録するか（下記「live」） |
| `live.ffmpeg` | `ffmpeg` | PATH 検索 |
| `live.ffprobe` | `ffprobe` | `live.captions` 有効時の字幕ストリーム判定。PATH 検索 |
| `live.captions` | `false` | ARIB 字幕を WebVTT レンディションとして出力。`libaribcaption` 入り FFmpeg が必要 |
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
| `cleanup.orphan_age` | `0`（= 14 日） | 孤児候補を自動回収または報告するまでのエイジング期間 |
| `cleanup.max_deletes_per_pass` | `0`（= 100） | 一括削除サーキットブレーカーの閾値。ソースを問わず 1 パス全体の合計に対して働く（ruler と同じラッチ式） |
| `cleanup.missing_asset_age` | `0`（= 24 時間） | 「active なのに実体ファイルが無い」候補が確認済みとして報告されるまでのエイジング期間。自動削除はしない（[storage.md](storage.md) §7「孤児回収の逆」） |
| `log.level` | `info` | debug / info / warn / error |
| `log.format` | `json` | json / text |

`log.*` は config を読む全サブコマンドに共通の入口で `slog` の既定ロガーへ適用される。
対象は server / migrate / rescue / enqueue / catalog / config validate。
config の読み込みより前に出るログだけは既定（text 形式・Info 以上）のままになる
（`--config` のパース失敗など）。

### 必須キー

最小 3 つ: `db`（資格情報）・`mirakcs`（下記「mirakc レジストリ」参照）・`storage.media_dir`。残りは全部デフォルトを持ち、最小構成は 10 行程度に収まる。

### db の運用ノブ（max_conns / api_statement_timeout）

2 つとも詳細は [operations.md](operations.md) §3「DB 運用」に決まっている。要点だけ:

- `db.max_conns`: プロセスは常に 1 個のプールしか持たず全ロールが共有する。未指定（0）なら起動時の roles からロール別 budget を合計して自動算出する
- `db.api_statement_timeout`: api ロールを含むプロセスのプール全体に適用される（monolith では worker 側にもかかる）。既定 30s

### ingest.stall_timeout

Work slice 内の転送で無進捗を検知する時間。この時間バイトが進まないと接続を切り、temp の offset から Range 再開する。4 分の slice は有限な Work Timeout で snooze し、同じジョブを再開するため、この値は録画全体の時間を制限しない（[recording.md](recording.md) §5.3）。遅い回線や大きな録画で接続断と判定される場合は延ばす。

### サーキットブレーカーと検出器の閾値

- `ruler.max_deletes_per_pass` は 1 site・1 pass あたりの導出削除数の上限。超えると対象の削除をすべて止め、削除サーキットブレーカーを発動する。
  - 数えるのは、ルールが `base` を供給していた予約が desired から外れた場合だけ。
  - ユーザーが投資を手放す操作（intent の skip・クリア、最後の investment となる overrides の削除）で起きる削除は数えず、発動中も実行する。
  - 発動後は件数が下がっても自動では解除しない。上限を下げると、一時的な EPG 欠損や一括編集で発動しやすくなる一方、誤判定で失う予約数を抑えられる。既定の 50 は通常の 1 site の active 予約数を超える値である。
- `reconciler.start_delay_grace`: 開始遅延検出器（[recording.md](recording.md) §3.3）の猶予。「開始時刻 + この時間」を過ぎても `recordings.started_at` が観測されない予約を検出してアラートする。mirakc 側の未知の不具合への保険（EPGStation#724: チューナー再接続ハングで開始が 10 分遅延した実例あり）。開始直後は mirakc の SSE 到達と watcher の処理に遅れがあるため、短すぎると正常な録画開始でも誤検知してアラートが常時鳴る

<details><summary>根拠</summary>

- 削除対象と resume の境界: [recording.md](recording.md) §3.2「大量削除サーキットブレーカー」、`POST /api/sites/{site}/breakers/ruler_deletes/resume`

</details>

### worker.periodic_jobs と worker.queues

- `worker.periodic_jobs`: プロセス内で定期ジョブを投入するか。対象は `rokuban enqueue` で投入できるジョブと同じ集合（`rokuban enqueue --help`）。`cm_detect_reconcile` だけは `cm_detect.enabled` が条件。k8s では false にし、CronJob から `rokuban enqueue` で投入する。true だと ScaledJob の 1 件消化モード（`--once`）が起動を拒み、投入元も CronJob に一本化できないため（[data.md](data.md) §2）。
- `worker.queues` は worker ロールが引くキューを絞る。空なら全キューを対象にする。
- 指定値は論理名で、`ingest` / `epg` / `ruler` / `reconciler` / `watcher` / `encode` / `thumbnail` / `cleanup` / `storage` / `cm_detect` / `default` を使う。
- 同じ値を `--queues` からも指定できるが、両方を指定すると起動エラーになる。k8s は ConfigMap 1 個を共有し、Pod ごとの差を argv に置く。
- site 単位のキュー名への展開とロールとの関係（worker ロールが無いプロセスは引かない）は [operations.md](operations.md) §5 を参照する。

### ffmpeg の存在検査

`encode.ffmpeg` / `encode.ffprobe` は、worker ロールが encode/thumbnail キューを購読するときだけ LookPath で存在検査する。購読するのは、絞り込みが無い（`worker.queues` が空で `--queues` も無い）か、encode/thumbnail を含むときである。api ロールは呼ばない（不変条件 4）。`live.ffmpeg` は `live.enabled: true` の streamer 起動時だけ検査する。

### CM 区間の検出

- `cm_detect.enabled` の既定値は `false`。有効なら ingest が値を録画単位の `recording_encode_policy.cm_detect` に凍結する。
- worker は 15 分ごとに必要な検出を投入する。録画詳細の PATCH で個別に有効化できる。
- デプロイ設定が `enabled: false` のとき、API は `cmDetect: true` と再検出を 409 で拒否する。ジョブが投入されず、`until_encoded` の原本が結果を待って残るためである。
- worker 起動時に `cm_detect.binary_dir` の 3 コマンド、ffprobe（原本の実尺を読む）、固定ルール `/usr/local/share/rokuban/cm_detect/JL_標準.txt` を検査する。
- これらは `Dockerfile.full` にだけ含まれ、公式イメージには含まれないため、JLSE 対応には full イメージを使う。
- `cm_detect` キューは同時実行数 1 で動く。検出完了または 3 回失敗するまで、`until_encoded` の原本を削除対象にしない。

### live

ライブのルートを登録するのは streamer ロールだけ。`enabled: false`（既定）なら公式イメージ（ffmpeg 無し）で streamer を起動する構成（録画配信 / サムネイルのみ）を壊さない（不変条件 4）。

**`live.enabled` だけは公開面（`GET /api/capabilities` の `live`）にも出る**。フロントは config を読めないため、これが無いと無効な機能への導線を出し続ける。ロール分割構成でも config ファイルは共有する（上記「関心事別にセクションを切る」）ので、値はそのまま公開面に写す --- streamer に問い合わせはしない（不変条件 1）。

**この値を返すのは api ロールのプロセスに限らない**。REST の生成ルートはロールで絞られないので、HTTP を持つどのプロセスに聞いても同じ答えになる（そう**しない**とロールごとに答えが割れる。[api/rest.md](api/rest.md)「機能の有効/無効は能力 API で観測する」）。したがって答えが表すのは常に「この config が有効か」であり、**「api の config では有効だが streamer を動かしていない」構成では導線が出る**（押した先はプレイリストの 404）。

- `live.segment_dir` に既定値は無い。`live.enabled: true` なら必須で、空なら起動時エラーにする。任意のローカルパスへ黙って fallback しない。
- 録画バッファ（mirakc `recording.basedir`）とは別ディスクに置く。k8s では tmpfs の `emptyDir: {medium: Memory}` を使う。
- 起動時（HTTP listener の前）にディレクトリの中身を掃除する。掃除はこのパスの唯一の書き手を前提とする。tmpfs はノード再起動でしか消えないため、SIGKILL 等で残った前回プロセスのファイルも毎回消える。ディレクトリ自体は残す。

| 状況 | 扱い |
|---|---|
| k8s の `emptyDir` を `segment_dir` に直接 mount | Linux は mount point の rmdir を EBUSY で拒む（Linux container で実測済み）。中身だけを消せば起動時の Warn は出ない |
| `/dev/shm/rokuban-live` のような tmpfs subdirectory | subdirectory 自体は mount point ではないため rmdir の問題は起きない |
| 複数プロセス / レプリカで共有 | 共有しない。後発の起動時掃除が先発のセッションディレクトリを消す |

- 共有時は視聴が一度途切れるが、次のプレイリスト要求で新しいセッションが起きて自己修復する。
- 追っかけ再生は録画時間分の EVENT セグメントを idle GC まで保持する。同時視聴する録画の時間・本数・profile の bitrate を掛け合わせて tmpfs 容量を見積もる。
- `live.max_sessions` はこのプロセスが同時に持てるライブ/追っかけセッション（≒ ffmpeg プロセス）数。**プロセスローカルな上限であり、グローバルな天井（チューナー数、mirakc が裁定）ではない**（[operations.md](operations.md) §5「既定を 1 にする根拠」）
- `live.idle_timeout` はサービス単位の idle GC 猶予であり、その時間セグメント要求が無ければ ffmpeg を止める。クライアントごとの生存は追わない。
- 離脱ヒント（`POST .../live/leave`）の短い猶予は設定キーにせず、`3 × 最長の live.profiles[].segment_seconds + 2s`（既定 8 秒）から導出する。
- この猶予を `idle_timeout` でクリップしない。生きている視聴者の次の要求より短くなると、leave が他人の視聴を切るためである。

| 条件 | 結果 |
|---|---|
| `segment_seconds: 6` と `idle_timeout: 2s` を設定 | クリップすると猶予がセグメントより短くなり、離脱ヒントが他人の視聴を切る |
| 導出した猶予が `idle_timeout` 以上 | ヒントは GC を先へ動かさず no-op になる。起動は止めず、運用者の設定を尊重する |

離脱猶予は、生きている視聴者の次の要求が来るまでの間隔より長くする。その間隔はセグメント長が決めるため、別の設定キーにすると矛盾する値を設定できる。別の値から必ず導ける値は設定キーにしない。

詳細は [api.md](api.md) §ライブ視聴の HLS「離脱は『ヒント』であって停止命令ではない」。
- `live.tuner_priority` は mirakc への X-Mirakurun-Priority。ruler が生成する schedule の既定 priority（10）より低く保つことで、チューナー枯渇時に録画側を常に勝たせる（[recording.md](recording.md) §2「チューナー調停」）。**Rokuban はこの不等式を強制しない**。ルールの priority は DB 側（`rules.priority`、既定 10）でユーザーが自由に変えられ、ライブ側の `live.tuner_priority` は config 側なので、両者を跨いで比較検証する権威がどちらにも無い。ユーザーがルールの priority を 0 や 1 まで下げると、この既定値のままではライブが録画に勝ってしまう。運用者が両方の値を意識して選ぶ前提とする
- `live.profiles` は `encode.profiles` とは別の構造体 --- HLS はセグメント長・プレイリスト長という VOD には無い制約を持つ。`name` は `?profile=` から参照する一意な名前で、英数字・ハイフン・アンダースコアのみ（セグメントファイル名の接頭辞に使うため）。ISDB-T の映像は MPEG-2 でブラウザの HLS 経路では事実上再生できないため、H.264 への変換が前提。**この並びが `/live` の画質セレクタの並びで、先頭が `?profile=` 省略時の既定になる**（一覧は `GET /api/live-profiles` が返す。名前と `height` だけで、コーデック・品質・extra_args は出さない）

### encode/live の HW エンコード

構造化のまま HW エンコード（VAAPI 等）を表現する。追加するキーは「`extra_args` では構造的に届かない位置（`-i` より前のオプションと、アプリが `-vf` を握っているスケール filter）」に限る。コーデック指定より後ろのオプション（`extra_args` が既に出ている位置）は既存のキーで足りるので構造化しない。

| キー | 説明 |
|---|---|
| `encode.profiles[].scaler` / `live.profiles[].scaler` | スケール filter の系統名（`-vf` の filter 文字列そのものではない）。既定 `""`（= `software`）。**許す値は `software` と `vaapi` のみ** --- qsv / cuda はこの環境の ffmpeg で `scale_qsv` / `scale_cuda` の綴りを確認できておらず未検証のため除外してある。`height` が 0 のときに書くと起動エラー |
| `encode.profiles[].deinterlace` / `live.profiles[].deinterlace` | インターレース解除の系統スイッチ。既定 `false`（現行互換）。`true` なら `scaler` から software=`yadif` / vaapi=`deinterlace_vaapi` を導出し、scale があれば解除の後ろに連結する。filtergraph 文字列は直接指定しない |
| `encode.profiles[].qp` / `live.profiles[].qp` | `-qp`（品質指定）。`crf` との同時指定は起動エラー（優先順位を実行時に決めさせない） |
| `encode.profiles[].hwaccel` | プロファイル毎の `-i` 前置ブロック（`kind` 必須 / `device` / `output_format` 任意）。VOD はプロファイルごとに入力を開き直すため、プロファイル単位で持たせられる |
| `live.hwaccel` | **`live.profiles[]` 内ではなく `live:` 直下**。ライブは 1 回の ffmpeg で入力 1 本・出力 N 本なので、プロファイル毎に持たせると「プロファイル 2 つが別の hwaccel を要求する」という表現できない設定が書けてしまう --- セクション直下に置けばそれが表現不可能になる |
| `encode.profiles[].input_extra_args` / `live.input_extra_args` | `-i`（VOD）/ `-f mpegts -i pipe:0`（live）の直前に追加する引数 |
| `encode.profiles[].extra_args` / `live.profiles[].extra_args` | 既存キー。改名していない --- ただし VOD 側は位置が 1 点だけ動く（下記） |
| `encode.profiles[].subtitles` | `webvtt` のみ。原本の ARIB 字幕を encoded ファイル隣の `.vtt` サイドカーに出力する。libaribcaption 入り ffmpeg が必要 |
| `encode.profiles[].cut` | 既定 `false`。`true` なら確認済みチャプターの `cut=true` 区間を除いたカット版を出力する（下記「カット版」） |
| `live.captions` | 既定 `false`。`true` で全プロファイルを 1 つの master playlist にまとめ、WebVTT 字幕 rendition を出力する。字幕なし番組では映像・音声のみの master を出力する。libaribcaption 入り ffmpeg が無ければ起動エラー。複数プロファイルの `segment_seconds` / `playlist_size` は同一値が必要 |

argv の順序（VOD）:

```
-hide_banner -nostats -y                                      # アプリ
[-hwaccel K] [-hwaccel_device D] [-hwaccel_output_format F]   # hwaccel ブロック
[input_extra_args…]                                            # ユーザー（入力側）
-i INPUT                                                       # アプリ
-c:v VC -c:a AC
[-vf <deinterlace[, scaler が決めた scale]>]                   # deinterlace=true または height>0 のときだけ、常に 1 個
[-crf N | -qp N] [-preset P]
[extra_args…]                                                  # ユーザー（出力側）
-f CONTAINER -progress pipe:1 -loglevel error OUTPUT           # アプリ所有の末尾
```

live の argv は同じ規則を入力 1 本・出力 N 本へ展開する。

| 段階 | 順序 |
|---|---|
| 入力 | `live.hwaccel` → `-probesize` / `-analyzeduration` → `live.input_extra_args` → `-f mpegts -i pipe:0` |
| 各 profile | `-map`（映像 1 本と音声 3 本）→ `-c:v` / `-c:a` → 主 / 副の `-filter:a:N` |
| 映像 filter と品質 | `[-vf]`（captions 有効時は `-filter:v:N`）→ `[-crf|-qp]` → `[-preset]` |
| filter の組み立て | `deinterlace` が true なら解除を先頭に置き、`height` があれば後ろに scale を 1 個連結 |
| HLS 出力 | `-force_key_frames` → `profile.extra_args` → `-var_stream_map` / `-master_pl_name` → `-f hls ...` |

音声 3 本の意味は [api/media.md](api/media.md) §音声を参照する。

`deinterlace` は bool とし、filter の実体を `scaler` から導出する。`scaler: software` なのに `deinterlace_vaapi` を書くような矛盾した設定や、`-vf` を別名で自由に書く設定を表現できないようにするためである。`deinterlace: false`（省略）のときは、生成する argv を従来から変えない。

**`extra_args` の位置が 1 点だけ動いた**。VOD 側は以前 `-f`（コンテナ）の後ろだったが、今は前に移った --- 「ユーザーのオプションはコーデック/品質/スケール指定の後・アプリ所有の末尾の前」という規則を VOD と live で 1 つにするため。`-f` は下記の allowlist に含まれないので、この移動でユーザーが相対順序に依存していた挙動が変わることはない。

**起動エラーになる組み合わせ**: `crf` と `qp` の同時指定 / 未知の `scaler` / `height` が 0 なのに `scaler` を書く / `hwaccel` ブロックがあるのに `kind` が空 / `crf`・`qp` の負値。

### カット版（`encode.profiles[].cut`）

**切る対象は録画側の事実でチャプターが持ち、その出力に切り取りを適用するかは出力の性質でプロファイルが持つ**。
チャプターは自動検出（`cm_detect`）の結果をユーザーが確認したもの（`recording_chapter_ownership` の行がある状態）で、その `cut=true` 区間が落ちる。
自動検出のままでは作らない --- 誤検出のまま本編が削られ、原本がごみ箱を経由せずに消えると取り返せない。

**`cut: true` を選ぶなら、`cut` でないプロファイルを 1 つ以上含めること。**
原本 TS はブラウザで再生できないので、確認に再生が要り、再生に encode が要り、encode に確認が要る循環になる。
ルール保存・予約 overrides・事後追加 API・ingest の凍結の 4 経路が同じ判定を共有し、マージ結果が `cut` だけになった場合は凍結時に `cut` を落とす。
意図は overrides に残るので、`cut` でないプロファイルを足せば戻る。

argv は `cut` のときだけ filtergraph になる:

```
-hide_banner -nostats -y
[-hwaccel K [-hwaccel_device D] [-hwaccel_output_format F] | -vaapi_device D] [input_extra_args…]
-i INPUT
-filter_complex <trim/atrim → concat → deinterlace → scale>    # アプリが組む
-map [vout] -map [aout]
-c:v VC -c:a AC …
```

カット版は各 keep 区間を時刻で切り、結合後に映像 filter を適用する。

| 段階 | 規則 |
|---|---|
| 入力の HW 設定 | `hwaccel.output_format` があれば通常の encode と同じ引数を `-i` の前に置く。省略時は救済経路を使い、`hwaccel.kind: vaapi` なら `-vaapi_device D` を出す |
| 区間ごとの trim | 映像は `trim=start=…:end=…`、音声は `atrim=start=…:end=…` で切る |
| 結合後の filter | `concat` の後ろに `scaler` 由来の deinterlace / scale を連結する |
| VAAPI GPU 経路 | `deinterlace_vaapi` → `scale_vaapi` を使い、`format=nv12,hwupload` は付けない |
| CPU 救済経路 | CPU filter の後ろに `format=nv12,hwupload` を付ける |
| 時刻と PTS | A/V とも入力の最早 `start_time` を原点に切る。境界は共通の `frame / fps` で秒へ換算し、trim 後は区間開始時刻を PTS から引く。`PTS-STARTPTS` ではなく時刻を引くため、映像先頭の遅れを残す |

映像を最初のフレームから数えた番号で切ると放送 TS で映像だけがずれる。チャプターの原点も入力の最早 start_time である。

**HW フレームも trim / setpts / concat を通せる。** VAAPI GPU 経路は FFmpeg 7.1 以降を推奨する。

| 経路・版 | 挙動と測定 |
|---|---|
| VideoToolbox / FFmpeg 9.0.2 | `trim` → `setpts` → `concat` → `scale_vt` → `h264_videotoolbox` を実測した |
| VAAPI / FFmpeg 5.1〜7.0 | MPEG-2 decode surface は 6 枚固定。カット版では枯渇して `get_buffer() failed` とつなぎ目のブロックノイズが出る。ジョブは成功扱いで終わり、壊れた版が公開される |
| VAAPI / FFmpeg 7.1 以降 | surface pool は動的。7.1.5-0+deb13u1 / libva 2.22.0-3 / iHD 25.2.3 でつなぎ目 3 の 30 分原本全体を処理しても `get_buffer() failed` は出なかった |
| 未検証 | 7.1 以降でつなぎ目の多い録画 |

**5.1〜7.0 は `-extra_hw_frames` で直さず、救済経路を使う。** 足りない枚数はつなぎ目の数で増えるので、固定値では足りない。
5.1.9-0+deb12u1 / libva 2.17.0 / iHD 23.1.1 では、つなぎ目 1 は 1 枚、つなぎ目 3 は 4 枚の追加で解消した。
つなぎ目 15 は 16 枚の追加でも `Cannot allocate memory` で止まった。
救済経路は `-hwaccel` を付けずに CPU でデコードするので、このプールを使わない。

**出力側に `-map` を書けないので、アプリがストリームを選ぶ。** ffmpeg の既定の選択（映像は最大解像度、音声は最大チャンネル数、同点は若い番号）を ffprobe で再現する。再現しないとカット版だけ別のストリームが選ばれ、カットしない版と音声が食い違う。

`cut` にだけ掛かる起動エラー:

- `hwaccel.output_format` があり、`height > 0` または `deinterlace: true` なのに `scaler: vaapi` でない --- HW フレームの後ろに CPU filter は置けない
- `hwaccel.output_format` を指定する場合は `vaapi` のみ --- 指定すると hwupload を付けないので、ソフトウェア形式を HW へ上げる手段が無い
- `scaler: vaapi` で filter があるのに `hwaccel.output_format: vaapi` が無い --- upload は filter 後なので VAAPI filter に CPU フレームを渡せない
- `hwaccel.kind` が `vaapi` 以外 --- VAAPI scaler と救済経路を使えない
- `hwaccel.kind: vaapi` なのに `device` が無い --- `-hwaccel_device` または `-vaapi_device` に渡すものが無い
- `extra_args` の `-map` --- ストリームの並びはアプリが握る（live と同じ理由）

**字幕サイドカーは同じ ffmpeg 起動の別出力なので `trim` が効かない。** 書き出した後に同じ keep 区間の写像で時刻を付け替える。CM 区間の中に収まるキューは捨て、境界をまたぐキューは keep 側でクリップする。

カット版の `rel_path` には世代番号が入る（`…_{profile}.g{n}.{container}`。1 世代目から付ける）。チャプターを直しても自動では作り直さず、UI の「編集前の内容です」から `POST /api/recordings/{id}/encoded/{profile}/reencode` で作り直す（[storage/contract.md](storage/contract.md) §3）。

**`extra_args` / `input_extra_args` は値の個数まで既知の allowlist だけを受け付ける。**

| 引数の形 | 許可する値 |
|---|---|
| 値を取らない | `-an` / `-vn` / `-sn` / `-dn` / `-shortest` / `-nostdin` / `-re` |
| 直後の 1 token を値に取る | `-movflags` / `-map` / `-global_quality` / `-cq` / `-q:v` / `-b:v` / `-b:a` / `-probesize` / `-analyzeduration` / `-extra_hw_frames` |

表に無い引数と裸の位置引数は起動エラーにする。値なしフラグを明示するため、`["-an", "/tmp/evil.mp4"]` のように 2 本目の出力パスを値として偽装できない。`-filter:v:0` / `-lavfi` など別名や stream 指定子つき表記も許さず、denylist が別名を取りこぼす形にしない。

live ではさらに `-an` / `-vn` / `-sn` / `-map` を拒否する。live は `-var_stream_map` で映像・音声 rendition 3 本・字幕の順を持つため、利用者には ffmpeg 起動失敗が 504 として見える（[api/media.md](api/media.md) §音声）。

**範囲外**: device ノードのマウントはデプロイ側（k8s `resources.limits` / Docker `--device`）。**`hwaccel.device` の存在は起動時に検査しない** --- 公式イメージや device の無い CI を壊す。無い device を書いたプロファイルはジョブ / セッションの失敗として現れる。`-global_quality` / `-cq` / `-q:v` のような、コーデック指定より後ろに出せる（= `extra_args` で届く）品質オプションはキー化しない。

<details><summary>根拠</summary>

- HW pixel format を trim / concat が除外しない根拠: FFmpeg n5.1.6 / n9.0 の trim.c と avf_concat.c
- `hw_frames_ctx` を出力へ引き継ぐ根拠: avfilter.c

</details>

### mirakc レジストリ: `mirakcs:`

`mirakcs:`（`{site, url}` の配列）に mirakc エンドポイントを登録する。単一サイト構成でも 1 要素の配列で書く（単一オブジェクトの糖衣は無い）。複数 mirakc を許すと programId のスコープが mirakc 単位になり、予約・EPG 射影・ingest の全スキーマに「どの mirakc か」が波及するため、要素ごとに `site` でスコープする。チューナー集約は mirakc 自身のリモートチューナー機能で賄えるため、単一サイトなら Rokuban 側は 1 エンドポイントで足りる。ハブ mirakc への集約は採らない（WAN リアルタイム依存 = 録画中の回線瞬断が録画欠損に直結するため）。

**旧 `mirakc:`（単数）キーを書いたまま起動すると `unknown field "mirakc"` で fail-fast する。** アップグレード時は `mirakcs:` の 1 要素（`site` と `url` を書いた形）に書き換える。

`site` は DB の全テーブルの `site` 列の値になる。API の資源同定（`/api/sites/{site}/...`）の権威は `config.mirakcs` レジストリに site が存在するかであり、api 自身は不変条件 1 によりどの site にも束縛されない。

- **`mirakcs:` は非空が必須**。空（未指定含む）は起動エラー
- **各要素の `site` は省略できない**。`mirakc:`（単一形式）の時代のような "default" への既定値フォールバックは無い --- `site` を書かない要素は空文字列のまま下記の構文制約に落ちて起動エラーになる
- **要素は `site` と `url` の 2 つだけ**。`storage` / `worker` / `ingest` 等のチューニング値は要素に入れない。アーカイブは単一（`media_assets` に site 列が無い）であり、`worker.queues` 等はデプロイ時のパラメータであって site の属性ではない。site ごとのチューニング値は、それを読むコードができたときに足す（不変条件 11）
- **site 名の構文制約**: `^[a-z0-9]([_-]?[a-z0-9])*$`、53 文字以内。**文字種は River のキュー名の制約と同一で、緩めない** --- キュー名を site で修飾するため、緩めると site 名がキュー名として弾かれる。**上限の 53 は、River のキュー名上限 64 から、site 修飾される論理キューのうち最長の prefix（`reconciler_`、11 文字）を引いた値**（64 − 11 = 53）。この検査は**設定のロード時**に行うので、`--sites` で束縛していないレジストリ上の site 名も対象になる。束縛した瞬間に初めて起動エラーになる、ということは無い
- **予約名**: `catalog` と `thumbnails` は site 名にできない。**この禁止はもう load-bearing ではない** --- `rel_path` の前置が `sites/{site}/` になったので、site 名がトップレベルの `catalog/` / `thumbnails/` と直接衝突することはない。緩めても得られる自由度（この 2 語を site 名にしたい運用要求は無い）が緩めるコスト（`internal/config` のバリデーション・テストの変更）に見合わないため、禁止だけ残してある。トップレベルディレクトリ名の予約（`catalog/` / `thumbnails/` / `sites/` の 3 つ。今も load-bearing）とは別の話で、[docs/storage.md](storage.md) §5 で分けて説明している
- レジストリ内の site 名の重複も不可。違反はすべて起動エラーとして全件列挙される（規約 4）
- **削除 reconcile のサーキットブレーカー（`breaker.DeleteReconcile`）は site 列を持たない**（`internal/worker/delete_reconcile.go`）。値は空文字列で、他の 2 種のブレーカー名のように実際の束縛 site をキーにしない。再開は site を含まない `POST /api/breakers/delete_reconcile/resume` を使う。理由と経緯は `internal/worker/delete_reconcile.go` の `DeleteReconcileWorker` doc コメント参照

**多サイトでどのロールがどのプロセスに乗るかは [docs/overview.md](overview.md) の役割分類に決定済み**。site に縛られるのは「mirakc に到達する必要がある仕事」（watcher / ingest / reconciler / epg / record_sweep / ライブ streamer）だけである。DB もアーカイブも単一なので、api / notifier / 録画配信 streamer / ruler / encode / thumbnail / 削除 reconcile は site 非依存の中央プロセスになる。

**プロセスがどのサイトに束縛されるかは `server` サブコマンドの `--sites` フラグで表す**。**config キーにはしない**（下記「やらないこと」の CLI フラグ方針に反しない --- `--all` / ロール選択と同じ「起動形態」の軸）。

- 未指定でレジストリが 1 要素ならその 1 つに束縛する。未指定でレジストリが 2 要素以上なら起動エラー（暗黙に「全部」にしない）
- `--sites=`（明示的な空）は束縛なし = 中央プロセス
- `--sites tokyo` は tokyo に束縛する。`--sites tokyo,tokyo` のような重複は 1 つに畳む（束縛数の判定が紛らわしいエラーにならないようにするため）
- `watcher` ロールは 1 プロセスが N サイトを束縛できる。束縛サイトごとに独立した goroutine + advisory lock（`watcher:<site>`）を持つため、複数サイトを同じプロセスで watch しても互いに干渉しない。0 サイト束縛では watch 対象が無いだけで、起動エラーにはしない
- `worker` ロールも 1 プロセスが N サイトを束縛できる。mirakc クライアントは site → 値の map を持つ。
- site 単位のキュー（ingest / epg / reconciler / watcher）は束縛 site ごとに `<queue>_<site>` を購読する。site 単位の定期ジョブ（epg_sync / tuner_sync / ruler_pass / reconcile_pass / record_sweep）も site ごとに 1 本登録する。
- 0 site（中央プロセス）の worker は、`worker.queues` / `--queues` を site 非依存キューだけに絞った場合に限り起動できる。
- `worker.queues` が空（既定は全キュー）、または site 単位のキューを含む状態で 0 site 起動すると、site ジョブは束縛 site に一致せず再試行し続けるため起動エラーにする。
- site 単位キューと物理名への展開は [operations.md](operations.md) §5 を参照する。
- `enqueue` サブコマンドは **site 束縛ジョブだけ** `--site` で投入先を選ぶ（未指定かつレジストリ 1 要素ならその 1 つ、2 要素以上なら必須）
- site 非依存ジョブには `--site` を付けない。どれが site 非依存かは `rokuban enqueue --help` が列挙する（詳細は [operations.md](operations.md) §1「ジョブ化されたループの監視」）
- `rescue` は site 非依存で、`--site` を持たない。catalog の各行は自分の site を持ち、catalog が無い場合のストレージ走査も `sites/{site}/` 前置から site を決める。前置の無いファイルは登録せず Warn にする

### server.allowed_hosts と server.trust_forwarded_host（X-Forwarded-Host は opt-in）

`server.allowed_hosts` は Host ヘッダーの allowlist で、アプリ内に残る唯一のセキュリティ機構（DNS rebinding 対策、[api.md](api.md) §認証 帰結3）。localhost 系（`localhost` / `127.0.0.1` / `::1`）は、**`X-Forwarded-Host` を検証対象にしておらず `Host` を直接使っているときに限り**、allowlist の設定に関わらず常に許可する。`X-Forwarded-Host` はクライアント側が自己申告できる値なので、転送値にも同じ緩和を適用すると `X-Forwarded-Host: localhost` で allowlist を素通りできてしまう。

- `allowed_hosts` が空でなく、`/healthz` / `/readyz` / `/metrics` 以外のパスなら Host の allowlist を検査する。`trust_forwarded_host: true` の場合は `X-Forwarded-Host` の先頭要素、そうでなければ `Host` を使う。
- `Host` のポート部は、1 文字以上の数字だけなら取り除く。それ以外（数値でない値や空値を含む）は入力全体を比較し、許可されない。
- allowlist の検査対象パスでは、absolute-form（`GET http://host/path HTTP/1.1`）を 400 で拒否する。`net/http` が absolute-form の request-target で `Host` を削除するため、リクエストラインから allowlist を満たしたり Host との不一致を判定したりできない。
- 比較前に両側を同じように正規化する。ASCII 範囲だけを小文字化し、末尾の dot 1 個だけを除く。接尾辞・wildcard の一致は無い。
- `allowed_hosts` が空ならこの検証を飛ばし、absolute-form も拒否しない。health path の免除は [operations/monitoring.md](operations/monitoring.md) を参照する。

アプリ内に認証を持たないため `allowed_hosts` はこの防壁の唯一の要であり、docker-compose の既定構成（`.env` に `ROKUBAN_ALLOWED_HOSTS` を設定しない）でも空になって同じ緩和が掛かる。空のまま利用者が気付かず公開ポートに晒す事故を防ぐため、起動時に `allowed_hosts` が空なら WARN ログを出す。この WARN はロールに関わらず出る（OpenAPI 由来のルートはどのロール構成でも生えるため、ロール分割デプロイでは全 Pod が同じ判定になる）。

- **`X-Forwarded-Host` は `server.trust_forwarded_host: true` を明示した構成でのみ検査する（既定 `false`）。**
- DNS rebinding のページは Rokuban と同一オリジンから、プリフライトなしで任意の request header を付けられる。直接露出構成（`--all` の既定）で無条件に trust すると、攻撃者は `X-Forwarded-Host: <allowlist に載っている値>` を自己申告して allowlist を通過できる。値は `config.example.yml` から推測できる。
- この設定を有効にしてよいのは、正しい reverse proxy が前段にあり、外から来た `X-Forwarded-Host` を必ず上書きする構成だけである。

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

`encode.profiles[].rate` は録画実尺に対する締切の倍率で、既定は 4.0。
encode ジョブ投入時に `recordings.ended_at - started_at` と rate から締切を解決して
args に保存する。これにより、JobRescuer を動かす leader と投入元の設定が異なっても、
実行ジョブに対して同じ締切を使える。締切は `max(1 時間, 実尺 × rate)` である。実尺は `ended_at - started_at` を使い、取れなければ番組長（`program_duration_ms`）で代用する。それも取れない場合と締切のない旧 args は 12 時間となる。River はこの値をそのまま ffmpeg の締切に使うので、短く見積もると長尺が毎回締切超過で失敗する。誤った締切超過より、死亡検知が遅れる方を選ぶ。

## 運用補助

- **`rokuban config validate`**: env 展開 + strict パース + 必須検証だけやって exit。ConfigMap 更新前チェックや CI に置ける
- **`config.example.yml`**: コメント付きでリポジトリに置き、**キーの網羅の権威**とする（ドキュメント生成までは YAGNI）

## やらないこと

- **env による config キーの自動オーバーライド**（viper/koanf の多層マージ）: どの値がどこから来たか追いにくくなる割に、この規模では利得がない
- **設定ファイルの分割・include 機構**: 分離の必要があるのは機微情報だけで、それは env で足りる
- **CLI フラグで環境設定値を渡さない。** `--config` はファイルのパス、`--all` / ロール選択はプロセスの起動形態を表す。
  - 例外は「このプロセスが何を担当し、どう畳むか」を表す `--sites` / `--queues` / `--once` / `--once-idle-timeout` / `--soft-stop-timeout`。
  - `--queues` は設定の `worker.queues` と同じ値を取るが、デプロイ時パラメータであり環境設定ではない。両方指定は起動エラーにし、値の出所を 1 つに保つ。
  - `--once` と `--once-idle-timeout` は起動形態だけに意味があり、常駐 process の設定項目にはしない。
  - `--soft-stop-timeout` は SIGTERM 後にジョブを打ち切る猶予で、k8s の `terminationGracePeriodSeconds` と対になる。プラットフォームの SIGKILL が真の上限になる。
  - encode は数時間、ruler は数秒と必要な猶予が大きく異なる。同じ Pod spec に 2 つの値を並べると対応関係を保てる。k8s は ConfigMap 1 個を共有し Pod ごとの差を argv に置くため、argv から外すと Pod セットごとに ConfigMap が必要になる（[operations.md](operations.md) §5）。
- **SIGHUP 等のホットリロード**: 設定変更は再起動。crash-only + level-triggered 設計なら再起動は無害で、リロードは「どの値がいつから有効か」という追跡困難性を別の形で持ち込むだけ
