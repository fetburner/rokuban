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
| `storage.stores` | —（必須になる条件は下記「置き場の規則」） | 名前付き置き場の定義（FS は任意で `root`） |
| `storage.originals` | —（`stores` / `derived` と一緒に書く） | 上から評価する原本の置き場の規則 |
| `storage.derived` | —（`stores` / `originals` と一緒に書く） | 派生物の共通の置き場 |
| `storage.transfer.bandwidth` | なし | 転送元の置き場ごとの帯域の上限 |
| `storage.transfer.backlog_limit` | なし | 転送元の置き場ごとの転送待ちの上限。達したらその置き場の原本を読む encode の投入を止める |
| `storage.transfer.overlap` | `48h` | 新しいコピーができてから旧コピーを消すまでの猶予 |
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
| `live.max_sessions` | `4` | プロセスローカルの同時ライブセッション上限 |
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
| `cleanup.missing_asset_age` | `0`（= 24 時間） | 実体無しの猶予。実体ファイルが無い候補を報告するまでの期間 |
| `log.level` | `info` | debug / info / warn / error |
| `log.format` | `json` | json / text |

`live.captions` には `libaribcaption` 入りの FFmpeg が必要である。`live.profiles` の形（`scaler` / `crf` / `qp` を含む）はサンプル設定ファイルにあり、`segment_seconds` の既定は 2、`playlist_size` の既定は 6 である。判断は [live.md](live.md)「live」と「encode/live の HW エンコード」にある。

`cleanup.trash_retention` はごみ箱の猶予である（[storage.md](../storage.md) §7）。`cleanup.max_deletes_per_pass` はソースを問わず 1 パス全体の合計に対して働く（ruler と同じラッチ式）。`cleanup.missing_asset_age` は、「active なのに実体ファイルが無い」候補の報告までの期間である。自動削除はしない（[storage.md](../storage.md) §7「孤児回収の逆」）。

`log.*` は config を読む全サブコマンドに共通の入口で、`slog` の既定ロガーへ適用される。対象は server / migrate / rescue / enqueue / catalog / config validate である。config の読み込みより前に出るログだけは既定（text 形式・Info 以上）のままになる（`--config` のパース失敗など）。

### 置き場の規則

置き場の規則は、置き場の定義・原本の置き場の規則・派生物の置き場・転送の設定からなる。形はサンプル設定と検証が持つので、ここには判断だけを書く。

`storage.stores` は、`recording_hls.enabled` を有効にしたとき、または `originals` / `derived` / `transfer` のどれかを書いたときに必須である。

置き場の名前の長さは、River のキュー名 `{queue}_{store}` が 64 文字に収まる式で決まる。

`storage.originals` は上から評価して最初に一致した規則を使う。時刻の条件を `older_than`（放送終了時刻からの経過時間）だけにするのは、`newer_than` や時刻の窓を許すと desired の置き場が行き来し、転送と削除が往復するからである。最後の規則を無条件にするのは、どの資産にも desired の置き場が決まるようにするためである。原本の置き場は `fsync` と atomic rename と hard link を信頼できる POSIX FS に限る（[ストレージ契約](../storage/contract.md) §1）。`storage.derived` は、全 site 共通の派生物の置き場を 1 つ選ぶ。派生物には FS と S3 API の置き場を使える。

`storage.transfer.overlap` の下限は、実体無しの猶予（`cleanup.missing_asset_age`）に削除 reconcile の周期の 2 倍を足した値である。新しいコピーの実体無しの判定が確定する前に、旧コピーを消さないためである。省略すると `48h` になり、既定の猶予ならこの下限を満たす。猶予を延ばして下限を割るときは、検証が overlap の明示を求める。

`storage.transfer.bandwidth` は転送元ごとの正の帯域値である。`bps` / `Bps` は 1 秒あたりの bit / byte、`Kbps` / `Mbps` / `Gbps` と `KBps` / `MBps` / `GBps` は 10 進、`KiBps` / `MiBps` / `GiBps` は 2 進で読む。たとえば `50Mbps` は毎秒 6.25 MB である。

`storage.transfer.backlog_limit` は FS 置き場ごとの正の整数である。転送待ちとは、その置き場にコピーがあり、desired の置き場へまだ転送されていない資産の件数である。件数は転送の reconcile と同じ述語で数える。転送の失敗が試行上限に達した資産は数えない。件数が上限に達すると、encode reconcile はその置き場の原本を読む encode（キュー `encode_{store}`）を投入しない。原本の ingest は止めない。ingest しなかった原本は後から取り戻せないが、encode は転送待ちが減った後に走れるからである。

件数の集計は資産数に比例して重いので、件数が要るパスだけで数える（[監視](../operations/monitoring-transfer.md)）。

`storage.transfer` は規則の指紋に含めない。帯域・転送待ちの上限・overlap は転送の宛先の判断に使わないので、変えても積み済みの転送ジョブを捨てさせない。

`storage.originals` の参照先は FS 置き場に限る。FS 置き場は何個でも定義でき、規則ごとに別の置き場を指せる。`storage.derived` は、FS 置き場か S3 置き場を指定できる。S3 置き場の定義は、どの規則も参照しなければ受け付ける。

FS 置き場の root は `storage.stores.<名前>.root` で指定する。省略した置き場は `storage.media_dir` を root にするので、1 つの置き場だけのプロセスは `media_dir` のままでよい。省略した置き場は 1 つのプロセスに 1 つまでしかマウントできない。2 つ目以降の置き場には `root` を書く。`root` は規則の指紋に含めない。マウント先の違いで積み済みの転送ジョブを捨てさせないためである。

同じ root を指す 2 つの置き場を 1 つのプロセスでマウントすると、目印と書き込みを取り違えるので起動時に拒否する。一方の root がもう一方の root の下にある組み合わせも拒否する。外側の置き場の監査が、内側の置き場のファイルまで歩いてしまうからである。

どの置き場をマウントするかは config ではなく argv の `--stores` で渡す（`--sites` と同じ扱い）。`--stores` に `storage.stores` に無い名前や S3 置き場を渡すと起動エラーになる。

次の起動エラーもある。

- 置き場で修飾するキューを引くのに置き場が 0 件の worker
- FS 置き場が 0 件で ingest を引く worker
- `storage.derived` が FS 置き場なのにマウントしていないまま、encode か thumbnail（seek_tiles を含む）を引く worker
- 置き場で修飾するキューが 2 本以上になる `--once`

派生物の FS 置き場を別の置き場と並べて使うときは、派生物を S3 置き場にするか、その置き場を `--stores` でマウントする。

worker が ingest か置き場で修飾するキューを引くときは、FS 置き場を 1 つ以上マウントできなければならない。ruler など置き場を使わないキューだけを引く worker には、置き場が無くてもよい。

`storage.stores.<名前>.internal_url` は、その置き場の原本を別のプロセスが転送元として読むための URL である。読むのは転送ワーカーで、転送元の置き場をそのプロセスがマウントしていないときに使う。

`internal_url` が指すのは、その置き場をマウントした streamer のベース URL である。転送ワーカーは `<internal_url>/api/media/stores/<置き場>/recordings/<id>/file` へ Range 付きで要求する。応答が `X-Accel-Redirect` で本体を前段に任せる streamer を指すと、転送は失敗する。原本を直接返す streamer を指す。

`server.allowed_hosts` が空でないとき、`internal_url` のホストはその一覧に含める。streamer の Host 検証は転送の要求にもかかるので、一覧に無いホストは 400 になる。この構成は起動時に拒否する。

`internal_url` の無い置き場は、マウントしているときだけ転送元になる。

転送元をマウントしているときは、`internal_url` に新しいコピーがあってもローカルを読む。

`internal_url` からの読みの途中で本体が切れたら、転送ワーカーは切れた位置から Range 付きで残りを要求し直す。読み進めないまま張り直すのが 3 回続いたら諦める。その後は転送ジョブの次の試行が、書いた checkpoint から再開する。

<details><summary>根拠</summary>

- 名前の上限の式は `config.MaxStoreNameLen` にあり、`TestStorageQueueNameBudget` が全キュー名から検査する。
- 起動エラーの組み合わせ: `TestValidateStoreBinding`（`cmd/rokuban/stores_test.go`）
- 根の重なりの拒否: `TestStoreRootsRejectsNestedAndIdenticalRoots`
- 応答に X-Accel-Redirect が付いた場合の拒否: `TestStorageTransferWorkerRejectsAccelRedirectAnswer`
- `internal_url` のホストが `server.allowed_hosts` に無い構成の拒否: `TestLoadRejectsInternalURLHostOutsideAllowedHosts`
- マウントした置き場のローカル優先: `TestStorageTransferWorkerPrefersMountedCopyOverNewerRemoteCopy`
- 下限の周期は `config.DeleteReconcileInterval`、指紋は `StorageConfig.PolicyFingerprint` が持つ。
- `backlog_limit` の検証（未知の置き場・s3 置き場・0 以下）: `TestStoragePolicyValidation`
- 読みの途中で切れたときの Range での張り直し: `TestResumingRemoteReaderContinuesAfterDisconnect`
- 読み進まない張り直しが上限に達したら諦めること: `TestResumingRemoteReaderGivesUpAfterReconnectLimit`
- checkpoint からの再開: `TestStorageTransferWorkerResumesRemoteOriginalWithRange`
- 帯域の上限が転送元の置き場ごとに掛かり、上限の無い置き場は絞られないこと: `TestTransferSourceReaderLimitsOnlyTheConfiguredSourceStore`
- 転送待ちが上限に達した置き場へ encode を積まないこと: `TestEncodeReconcile_TransferBacklogStopsEncodesIntoTheSourceStore`
- 止まっている置き場の解除が次のパスで投入とゲージに出ること: `TestEncodeReconcile_BacklogGaugeClearsOnTargetedPass`
- 件数を数えないパスと数えるパスの区別: `TestEncodeReconcile_PassWithoutDecisionSkipsBacklogCount`
- Warn / Info が状態の変化だけで出ること: `TestEncodeReconcile_BacklogLogsOnlyOnTransitions`

</details>

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

