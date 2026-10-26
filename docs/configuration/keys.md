> [configuration.md](../configuration.md) §スキーマ構造のうち、キーの一覧・必須キー・運用ノブ・閾値・検出の節。索引から辿る。

## スキーマ構造

設定のトップレベルは関心事ごとに切る。個々のキーの判断は、この下の節に書く。

### 関心事別にセクションを切る

トップレベルはロール別に切らず、関心事別に構成する。ロール別 Deployment でも config は同一ファイルを共有し、ロールで切ると streamer と worker が同じ ffmpeg 設定を二重に持つためである。各ロールは自分に関係するセクションだけを読む。

### リファレンス

キーの網羅の基準は、リポジトリ直下のコメント付きサンプル設定ファイルである。この文書の表はキー・既定値・一言に留め、値を選んだ理由だけを各節に書く。

[`config.example.yml`](../../config.example.yml) の YAML を写さないのは、片方だけ直って気づかないうちにずれるためである。既定値の実装は `internal/config/config.go`（`defaults()` と各 `applyDefaults` / 呼び出し先の既定）にある。

#### サーバーから CM 検出までのキー

キーと既定値の表である。判断は各節にある。節がどのファイルにあるかは [索引](../configuration.md) の表で引く。

| キー | 既定値 | 説明 |
|---|---|---|
| `server.listen` | `":40773"` | HTTP 待ち受け（mirakc の 40772 の隣） |
| `server.allowed_hosts` | `[]` | Host の許可リスト |
| `server.trust_forwarded_host` | `false` | `X-Forwarded-Host` を検証するか |
| `db.host` / `db.user` / `db.password` / `db.database` | —（必須） | PostgreSQL 接続情報 |
| `db.port` | `5432` | |
| `db.sslmode` | `disable` | |
| `db.max_conns` | `0`（roles から自動算出） | プロセスが持つ唯一のプールの上限 |
| `db.api_statement_timeout` | `0`（= 30s） | api を含むプロセスの statement timeout |
| `mirakcs` | —（必須。既定なし） | mirakc の `{site, url}` 配列 |
| `storage.media_dir` | —（必須） | 原本の ingest 先 |
| `storage.scratch_dir` | `/var/tmp/rokuban` | ローカルスクラッチ |
| `storage.accel_location` | `""`（Go が直接配る） | X-Accel-Redirect の location |
| `ingest.concurrency` | `3` | mirakc サイトあたりの同時転送数 |
| `ingest.stall_timeout` | `30s` | 転送の無進捗検知 |
| `epg.sync_interval` | `10m` | EPG の全量取得間隔 |
| `epg.retention_grace` | `24h` | 放送終了から番組を刈り取るまでの猶予 |
| `ruler.max_deletes_per_pass` | `0`（= 50） | 大量削除ブレーカーの閾値 |
| `reconciler.start_delay_grace` | `0`（= 3m） | 開始遅延検出器の猶予 |
| `worker.periodic_jobs` | `true` | 定期ジョブをプロセス内で投入するか |
| `worker.queues` | `[]`（全キュー） | worker ロールが引くキューの絞り込み |
| `encode.ffmpeg` / `encode.ffprobe` | `ffmpeg` / `ffprobe` | PATH 検索。フルパス指定可 |
| `encode.concurrency` | `1` | encode キューの MaxWorkers（ingest とは独立） |
| `encode.thumbnail_concurrency` | `1` | thumbnail キューの MaxWorkers |
| `encode.profiles` | `[]` | 構造化エンコードプロファイル |
| `cm_detect.enabled` | `false` | CM 検出を有効にするか |
| `cm_detect.binary_dir` | `/usr/local/bin` | JLSE のコマンドの配置先 |

`mirakcs` は `site` も既定なしで各要素に必須である。`storage.media_dir` は、fsync・close・同一 FS rename・親ディレクトリ fsync を保証するローカル FS / JuiceFS / NFS を指す。FUSE S3 は原本にも派生物にも使わない。`storage.scratch_dir` は worker ロールでは空にできず、起動時に拒否する。

`ingest.concurrency` は `チューナー数 + 全速 pull の許容本数（1〜2）` で決める。既定 3 は 2 チューナー機の下端で（[録画](../recording.md) §5.4）、4 チューナー機では 5〜6 になる。接続プールの予算はこの値に追随する（[DB 運用](../operations/database.md)）。

`epg.retention_grace` は ruler の GC（予約と番組単位の意図）も同じ猶予に使う。**エッジの滞留を N 日許すなら N 以上が要る**（[ストレージ](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」）。

`encode.profiles` の形はサンプル設定ファイルにある。`rate` は rescue 締切の倍率で、既定は 4.0 である。`cm_detect.enabled` を有効にすると、ingest 時に CM 検出を録画へ凍結し、worker の定期投入が有効になる。`cm_detect.binary_dir` には JLSE の `logoframe` / `chapter_exe` / `join_logo_scp` を置く。

#### ライブ視聴・webhook・cleanup・log のキー

キーと既定値の表の続きである。

| キー | 既定値 | 説明 |
|---|---|---|
| `live.enabled` | `false` | ライブ視聴のルートを登録するか |
| `live.ffmpeg` | `ffmpeg` | PATH 検索 |
| `live.ffprobe` | `ffprobe` | `live.captions` 有効時の字幕ストリーム判定。PATH 検索 |
| `live.captions` | `false` | ARIB 字幕を WebVTT で出力するか |
| `live.segment_dir` | —（`enabled: true` なら必須） | HLS セグメントの書き出し先 |
| `live.max_sessions` | `4` | プロセスローカルの同時セッション上限 |
| `live.idle_timeout` | `30s` | サービス単位の idle GC 猶予 |
| `live.tuner_priority` | `1` | mirakc への X-Mirakurun-Priority |
| `live.hwaccel` / `live.input_extra_args` | なし | HW アクセラレーション / 入力側追加引数 |
| `live.profiles` | —（`enabled: true` なら 1 つ以上必須） | HLS 用プロファイル |
| `webhook.url` | `""`（no-op） | POST 先 |
| `webhook.secret` | `""` | 非空なら `X-Rokuban-Webhook-Secret` ヘッダに載せる |
| `webhook.timeout` | `5s` | 1 回の要求のタイムアウト。失敗時は同期で 1 回だけ再試行 |
| `webhook.events` | `[]`（全イベント有効） | 配送イベントの許可リスト |
| `cleanup.trash_retention` | `0`（= 30 日） | ごみ箱（`recordings.deleted_at`）の猶予 |
| `cleanup.orphan_mtime_grace` | `0`（= 7 日） | 孤児候補にするまでの mtime 猶予 |
| `cleanup.orphan_age` | `0`（= 14 日） | 孤児候補を自動回収または報告するまでの期間 |
| `cleanup.max_deletes_per_pass` | `0`（= 100） | 一括削除ブレーカーの閾値 |
| `cleanup.missing_asset_age` | `0`（= 24 時間） | 実体ファイルが無い候補を報告するまでの期間 |
| `log.level` | `info` | debug / info / warn / error |
| `log.format` | `json` | json / text |

`live.captions` には `libaribcaption` 入りの FFmpeg が必要である。`live.profiles` の形（`scaler` / `crf` / `qp` を含む）はサンプル設定ファイルにあり、`segment_seconds` の既定は 2、`playlist_size` の既定は 6 である。判断は [live.md](live.md)「live」と「encode/live の HW エンコード」にある。

`cleanup.trash_retention` はごみ箱の猶予である（[storage.md](../storage.md) §7）。`cleanup.max_deletes_per_pass` はソースを問わず 1 パス全体の合計に対して働く（ruler と同じラッチ式）。`cleanup.missing_asset_age` は、「active なのに実体ファイルが無い」候補の報告までの期間である。自動削除はしない（[storage.md](../storage.md) §7「孤児回収の逆」）。

`log.*` は config を読む全サブコマンドに共通の入口で、`slog` の既定ロガーへ適用される。対象は server / migrate / rescue / enqueue / catalog / config validate である。config の読み込みより前に出るログだけは既定（text 形式・Info 以上）のままになる（`--config` のパース失敗など）。

### 必須キー

最小構成で書くのは 3 つだけで、残りは全部既定値を持つ。最小構成は 10 行程度に収まる。

3 つは `db`（資格情報）、`mirakcs`（[sites.md](sites.md)「mirakc レジストリ」参照）、`storage.media_dir` である。

### db の運用ノブ（max_conns / api_statement_timeout）

接続プールの上限と api ロール向けの statement timeout の 2 つで、詳細は [operations.md](../operations.md) §3「DB 運用」に決まっている。

`db.max_conns`: プロセスは常に 1 個のプールしか持たず、全ロールが共有する。未指定（0）なら起動時の roles からロール別 budget を合計して自動算出する。`db.api_statement_timeout` は api ロールを含むプロセスのプール全体に適用される（monolith では worker 側にもかかる）。既定は 30s である。

### ingest.stall_timeout

Work slice 内の転送で無進捗を検知する時間で、既定は 30 秒である。この時間バイトが進まなければ接続を切り、temp の offset から Range 再開する。

4 分の slice は有限な Work Timeout で snooze し、同じジョブを再開するため、この値は録画全体の時間を制限しない（[recording.md](../recording.md) §5.3）。遅い回線や大きな録画で接続断と判定される場合は延ばす。

### サーキットブレーカーと検出器の閾値

削除数の上限は、一度に消える予約の数を抑え、超えると削除を止めて手動で再開するまで止まり続ける。開始遅延の猶予は、録画が始まらない異常を検出してアラートするまでの待ち時間で、短すぎると正常でも誤検知する。

`ruler.max_deletes_per_pass` は、1 サイト・1 パスで実行してよい導出削除数の上限である。超えたら削除を一切実行せず、大量削除サーキットブレーカーとして発動する。数えるのは「ルールが base を供給しているのに desired から外れた」削除だけである。

**ユーザーが投資を手放す書き込み（intent skip / intent クリア / 最後の investment だった overrides の削除）をしない限り起きない削除は、数にも入らず、発動中でも実行される**。境界も同節にある（[recording.md](../recording.md) §3.2）。

発動すると、手動で `POST /api/sites/{site}/breakers/ruler_deletes/resume` するまで止まり続けるラッチになる。件数が閾値以下に戻っても自動では解けない（[recording.md](../recording.md) §3.2「大量削除サーキットブレーカー」）。下げると EPG の一時的な欠損やルールの一括編集で発動しやすくなる代わりに、誤判定で一気に失う予約の上限が下がる。既定値 50 は、通常運用で 1 サイトが同時に抱えるアクティブ予約数を明確に超える値として選んである。

`reconciler.start_delay_grace` は、開始遅延検出器（[recording.md](../recording.md) §3.3）の猶予である。開始時刻にこの時間を足した時刻を過ぎても録画の開始が観測されない予約を検出して、アラートする。mirakc 側の未知の不具合への保険で、EPGStation#724 のチューナー再接続ハングで開始が 10 分遅延した実例がある。開始直後は mirakc の SSE 到達と watcher の処理に遅れがあるため、短すぎると正常な録画開始でも誤検知してアラートが常時鳴る。

<details><summary>根拠</summary>

- 録画の開始の観測: `recordings.started_at`

</details>

### worker.periodic_jobs と worker.queues

定期ジョブの設定は、プロセス内で定期ジョブを投入するかを決め、k8s では無効にして外部の CronJob から投入する。キューの設定は、worker が引くキューを絞る。空なら全部で、引数でも指定できるが両方指定は起動エラーになる。

`worker.periodic_jobs` の対象は `rokuban enqueue` で投入できるジョブと同じ集合である（`rokuban enqueue --help`）。`cm_detect_reconcile` だけは `cm_detect.enabled` が条件になる。k8s では false にし、CronJob から `rokuban enqueue` で投入する。true だと ScaledJob の 1 件消化モード（`--once`）が起動を拒み、投入元も CronJob に一本化できないためである（[data.md](../data.md) §2）。

`worker.queues` は、空なら全キューを引く。ロールを増やさずに「ruler / reconciler だけ別 Pod」を実現するための knob である。同じ指定を `--queues` で argv からもできるが、**両方指定は起動エラー**になる。k8s では ConfigMap 1 個を全 Pod で共有し、Pod ごとの差分を argv に寄せるためである（[operations.md](../operations.md) §5）。

書くのは論理名で、物理名は書かない。使えるのは `ingest` / `epg` / `ruler` / `reconciler` / `watcher` / `encode` である。`thumbnail` / `cleanup` / `storage` / `cm_detect` / `default` も使える。site 単位のキューの物理名への展開と、ロールとの関係は [operations.md](../operations.md) §5 を参照する。worker ロールが無いプロセスは、この設定に関わらずキューを引かない。

### ffmpeg の存在検査

ffmpeg の存在検査は、その ffmpeg を使うロールが起動するときだけ行う。api ロールは検査しない（不変条件 4）。

`encode.ffmpeg` / `encode.ffprobe` は、worker ロールが encode/thumbnail キューを購読するときだけ LookPath で存在検査する。購読するのは、絞り込みが無い（`worker.queues` が空で `--queues` も無い）か、encode/thumbnail を含むときである。`live.ffmpeg` は `live.enabled: true` の streamer 起動時だけ検査する。

### CM 区間の検出

CM 区間の検出は既定で無効で、有効にすると取り込み時にこの設定が録画ごとに凍結される。無効のデプロイでは、API が録画ごとの有効化と再検出の要求を 409 で断る。

`cm_detect.enabled` の既定は `false` である。有効にすると、ingest がこの値を録画単位の `recording_encode_policy.cm_detect` に凍結し、worker が 15 分ごとに必要な検出を投入する。録画詳細の PATCH で個別に有効化できるが、`enabled: false` のデプロイでは API が `cmDetect: true` と再検出を 409 で断る。誰もジョブを積まず、`until_encoded` の原本が結果を待って残り続けるためである。

worker 起動時に、`cm_detect.binary_dir` の 3 コマンドと ffprobe（原本の実尺を読む）と固定ルール `/usr/local/share/rokuban/cm_detect/JL_標準.txt` を検査する。これらは `Dockerfile.full` にだけ含まれ、公式イメージには含まれないため、JLSE 対応には full イメージを使う。

`cm_detect` キューは同時実行数 1 で動く。検出が完了するか 3 回失敗するまで、`until_encoded` の原本は削除対象にしない。

