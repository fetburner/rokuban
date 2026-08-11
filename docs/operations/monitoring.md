> [operations.md](../operations.md) §1「監視メトリクス」の一部。索引から辿る。

## 1. 監視メトリクス

### エンドポイント

`GET /metrics` で Prometheus の text exposition format を返す。**ロールに関わらず
すべてのプロセスが公開する** — worker だけの Pod でも滞留メトリクスを scrape したいため、
HTTP リスナーは常に 1 本立てる。OpenAPI には載せない（text format であり
生成クライアントの対象外）。

**`/metrics` と `/healthz` は Host allowlist を免除する。** 監視基盤は Pod IP や
サービス名で叩くため allowlist に載せようがない（IP は動的）。allowlist の内側に
置くと k8s の liveness probe と Prometheus の scrape が 400 で落ちる。DNS rebinding が
守ろうとしているのはブラウザ経由でデータを読み書きされることなので、機密を含まない
インフラ用エンドポイントを免除しても防壁は薄くならない。

実装は `internal/metrics`。2 種類を使い分けている。

- **プロセス内カウンタ / ヒストグラム**: そのプロセスで起きた事象を数える。
  再起動でリセットされるが Prometheus はカウンタのリセットを扱える
- **DB を引くゲージ**: scrape のたびに真実を DB から取り直す。プロセス内に
  溜めないので、どのロールが scrape されても同じ値になる。「イベントはヒント、
  真実はテーブル再読」と同じ考え方

| 実装済みメトリクス | 型 | 意味 |
|---|---|---|
| `rokuban_recordings_failed_total{reason}` | Counter | `recording.failed` 理由別 |
| `rokuban_records_broken_total{reason}` | Counter | `recording.record-broken` 理由別 |
| `rokuban_ingest_dropped_packets_total` | Counter | ドロップ統計（全体の趨勢） |
| `rokuban_ingest_error_packets_total` | Counter | TEI |
| `rokuban_ingest_scrambled_packets_total` | Counter | scrambled カウンタ |
| `rokuban_ingest_bytes_total` | Counter | ingest バイト数 |
| `rokuban_ingest_duration_seconds` | Histogram | ingest 所要時間 |
| `rokuban_ingest_jobs_total{result}` | Counter | ingest の成功/失敗件数 |
| `rokuban_uningested_records{site}` | Gauge（DB） | 未 ingest record 総量（件数） |
| `rokuban_uningested_record_bytes{site}` | Gauge（DB） | 未 ingest record 総量（バイト） |
| `rokuban_uningested_backlog_scrape_errors_total{site}` | Counter | 上記の取得失敗 |
| `rokuban_encode_duration_seconds` | Histogram | encode 1 件の所要時間 |
| `rokuban_encode_jobs_total{result}` | Counter | encode の成功/失敗件数 |
| `rokuban_thumbnail_duration_seconds` | Histogram | thumbnail 1 件の所要時間 |
| `rokuban_thumbnail_jobs_total{result}` | Counter | thumbnail の成功/失敗件数 |
| `rokuban_reconcile_pending_diff{action}` | Gauge | reconcile 差分数（**収束すればゼロ**。アラートはこちら） |
| `rokuban_reconcile_schedules_total{action}` | Counter | 実際に差分を消した量 |
| `rokuban_reconcile_schedule_lost_total` | Counter | 再作成で DELETE 成功 → POST 失敗（下記 reconcile。**0 以外はアラート対象**） |
| `rokuban_reconcile_circuit_breaker_trips_total` | Counter | 全損シグネチャでの発動（件数の閾値ではない。下記「経緯と失敗事例」） |
| `rokuban_circuit_breaker_tripped{site,breaker}` | Gauge | **いま止まっているか**（1 = 発動中）。ラッチなのでアラートはこちら |
| `rokuban_reconcile_last_pass_timestamp_seconds` | Gauge | 最後に完走したパスの時刻 |
| `rokuban_reconcile_start_delayed{site}` | Gauge | **開始時刻を過ぎたのに録画が始まっていない予約数**。収束すればゼロに戻る |
| `rokuban_ruler_pass_duration_seconds` | Histogram | ruler 1 パスの所要時間（下記 ruler） |
| `rokuban_ruler_reservations_total{action}` | Counter | ruler が作成/更新/削除した予約数（下記 ruler） |
| `rokuban_ruler_circuit_breaker_trips_total` | Counter | 大量削除ブレーカーの発動遷移回数（下記 ruler） |
| `rokuban_ruler_last_pass_timestamp_seconds` | Gauge | 最後に成功した ruler パスの時刻 |
| `rokuban_sweep_last_pass_timestamp_seconds` | Gauge | 最後に成功した record_sweep パスの時刻 |
| `rokuban_epg_sync_duration_seconds` | Histogram | EPG 全量同期の所要 |
| `rokuban_epg_programs_projected` | Gauge | 直近パスの投影件数 |
| `rokuban_epg_channels_without_programs` | Gauge | 番組を返さなかったチャンネル数 |
| `rokuban_epg_sync_last_success_timestamp_seconds` | Gauge | 最後に成功した同期の時刻 |
| `rokuban_tuners_projected{site}` | Gauge | 直近のチューナー同期で投影した本数（射影が空になっていないか） |
| `rokuban_tuner_sync_last_success_timestamp_seconds{site}` | Gauge | 最後に成功したチューナー全量同期の時刻 |
| `rokuban_capacity_overages{site}` | Gauge | チューナー不足区間の数（**非ゼロは信頼できるが、ゼロは保証ではない**。下記「沈黙は保証ではない」） |
| `rokuban_delete_reconcile_deleted_total{source}` | Counter | 物理削除したアセット件数（source 別） |
| `rokuban_delete_reconcile_bytes_total{source}` | Counter | 物理削除で解放したバイト数 |
| `rokuban_delete_reconcile_last_pass_timestamp_seconds` | Gauge | 最後に成功した削除 reconcile パスの時刻 |
| `rokuban_live_active_sessions` | Gauge | ライブセッション数（**per-process**。全体は Prometheus 側で sum。[k8s 運用](k8s.md) §5） |
| `rokuban_live_session_start_failures_total{reason}` | Counter | ライブセッション開始失敗（`session_limit` / `upstream_error` / `ffmpeg_error`） |
| `rokuban_live_idle_gc_reclaimed_total` | Counter | idle GC が回収したライブセッション数 |
| `rokuban_live_idle_gc_last_pass_timestamp_seconds` | Gauge | 最後に完走した idle GC パスの時刻 |

**録画失敗は観測した時点で数える。** 予約の照会や mirakc への問い合わせより後に
置くと、それらが失敗したときに取りこぼす（物事がうまくいっていないときこそ数えたい）。

**`reconcile 差分数` はゲージ。** カウンタ（`..._schedules_total`）は単調増加なので
「収束しているか」を表せない。ゼロに戻らないまま続くのは reconcile が収束できて
いないということで（mirakc が作成を拒否し続ける、サーキットブレーカーが削除を
止めている等）、アラートすべきはゲージ側。

**ゲージには「最後に成功した時刻」を必ず対で持つ。** ゲージは値が凍結するので、
`pending_diff` や `epg_programs_projected` だけでは「収束した」と「ループが動いて
いない」を区別できない。シングルトンがロックを取れていない・定期ジョブが投入されなく
なった場合を `time() - <last_*_timestamp> > 閾値` で検出する。実際に `UniqueOpts` の
設定ミスで EPG の定期同期がワンショット化していた事故があり、この指標があれば
気づけた。

**滞留メトリクスは取得失敗時に 0 を報告しない。** 0 を出すと「滞留なし」と区別できず、
滞留アラートを黙って無効化してしまう。代わりに専用のエラーカウンタを進める。

PID 別のドロップ内訳はメトリクスにしない（PID × 録画数でカーディナリティが爆発する）。
`drop_stats` テーブルと `/api/recordings/{id}/drop-stats` で見る。

未実装: エッジディスク残量（mirakc 側の値であり
Rokuban からは観測できない。node_exporter 等で別に取る）。

### 録画品質

mirakc の追従品質は EDCB ほどの長期実績がないため、品質メトリクスを継続計測する（[録画エンジン](../recording.md) 参照）。

| メトリクス | ソース | 用途 |
|---|---|---|
| `recording.failed` 理由別カウンタ | watcher が mirakc SSE から受信。理由は構造化されている: `start-recording-failed` / `io-error` / `pipeline-error` / `need-rescheduling` / `schedule-expired` / `removed-from-epg` | 録画失敗の傾向分析、mirakc 品質の実測 |
| `recording.record-broken` | watcher が mirakc SSE から受信（理由付き、複数回あり） | 録画中の異常検出 |
| ドロップ統計（PID 別 continuity counter 不連続 / TEI） | ingest のインラインスキャン。188 バイト境界の TS パケット統計を転送中に読み取り専用で採取（追加 I/O パスゼロ） | EPGStation のドロップログ相当。PID 別サマリを `drop_stats` テーブルに格納し UI で表示 |
| scrambled カウンタ | ingest のインラインスキャン。`scrambling_control` ビットのカウント | B-CAS/復号障害の検出（[アラート設計](alerts.md) の対象） |

### ジョブ化されたループの監視

ruler / reconciler / record_sweep（watcher の 3 段構えのうち (c) 定期全量突き合わせ）は River のジョブなので、**「ループが止まっている」の検出は** advisory lock が取れているかではなく、**ジョブが投入され完走しているか**を見る。

| 見るもの | 意味 |
|---|---|
| `rokuban_*_last_pass_timestamp_seconds`（`reconcile` / `ruler` / `sweep`） | `time() - この値` が周期を大きく超えたら止まっている |
| `river_job` の `state='available'` が滞留 | 投入はされているが誰も引いていない（worker が 0 か、キューを引いていない） |
| `river_job` が増えない | **投入自体が止まっている**。`worker.periodic_jobs: false` なのに CronJob が動いていない、あるいはリーダーが不在 |

3 番目が k8s 特有の落とし穴。`PeriodicJobs` はリーダーだけが投入するので、worker が 0 にスケールすると誰も投入しない（[データ層](../data.md) §2）。`rokuban enqueue` を叩く CronJob が設定されているかを最初に疑う。

手動で走らせたいときは `rokuban enqueue <job>`。既に待機中なら投入せず終了コード 0 を返すので、cron から重ねて叩いても安全。

**site 束縛ジョブと site 非依存ジョブで `--site` の要否が違う**:

| 種別 | ジョブ | `--site` | CronJob の立て方 |
|---|---|---|---|
| site 束縛 | `epg-sync` / `tuner-sync` / `ruler-pass` / `reconcile-pass` / `record-sweep` | 多サイトでは必須（1 サイトなら省略可） | **サイトごとに 1 本**（`--site tokyo` 等） |
| site 非依存 | `catalog-export` | **付けない**（付けるとエラー） | **全体で 1 本**（サイトごとに立てない） |

`catalog-export` はアーカイブが単一なので site の属性を持たない。サイトごとの CronJob から叩くと N 回投入される（River の一意制約で 1 本に合流はするが意図が読めない）。

**record_sweep には ruler / reconciler と違ってヒント経路（前倒し投入）がない**。定期投入だけが契機で、間隔は既定 5 分（`worker.RecordSweepInterval`、旧 watcher の `ReconcileInterval` を継承）。SSE 再接続をヒントにする案は検討したが、`internal/mirakc.Client.Subscribe` が再接続を内部に隠していて呼び出し側に通知できないため見送った（[録画エンジン](../recording.md) §3.3「record_sweep の起動契機」）。取りこぼしの実害は SSE の (a)(b) が大半を吸収し、record_sweep は定期パスとして収束させる保険という位置づけなので、5 分間隔で十分と判断している。

### ruler

| メトリクス | 説明 |
|---|---|
| `rokuban_ruler_pass_duration_seconds` | 1 パス（全ルール x 全射影番組）の所要時間。射影が有界なので伸び続けることはない |
| `rokuban_ruler_reservations_total{action}` | `created` / `updated` / `deleted` / `gc`。**`updated` が毎パス予約数と同じ値で増え続けるなら差分書き込みが効いていない**（[録画エンジン](../recording.md) §3.1） |
| `rokuban_ruler_circuit_breaker_trips_total` | 大量削除で停止した回数。EPG の一時欠損を疑う入口 |
| `rokuban_circuit_breaker_tripped{breaker="ruler_deletes"}` | **1 の間は導出削除が一切走らない**（手動再開まで止まるラッチ）。カウンタと違い「いま止まっているか」に答える |
| `rokuban_ruler_last_pass_timestamp_seconds` | 最終パス時刻。`time() - この値` でパスが止まっていることを検出する（gauge が凍る問題への対策） |

`deleted` と `gc` は区別する。`deleted` は「ルールがマッチしなくなった」導出削除で**サーキットブレーカーの対象**、`gc` は「番組終了 + 猶予経過」の時間駆動で**対象外**（停止後の再開で大量に消えるのが正常）。

### ingest

| メトリクス | 説明 |
|---|---|
| ingest バイト数・所要時間 | 転送パフォーマンスの監視 |
| 未 ingest record 総量 | エッジのリングバッファ滞留量。エッジディスク残量と突き合わせてアラートの基礎とする |

ジョブは諦めず再試行し続ける（max attempts で dead-letter にすると record が宙に浮く）。長時間の転送失敗でエッジのリングバッファが溜まり続けるのが唯一の運用リスクであり、このメトリクスで可視化する。

### reconcile

| メトリクス | 説明 |
|---|---|
| `rokuban_reconcile_pending_diff{action="create"\|"delete"}` | desired（reservations）と observed（schedule_sync）の**存在**の差分。通常はゼロ付近に収束する |
| `rokuban_reconcile_pending_diff{action="update"}` | 予約オプション（priority / tag）の差分のうち、このパスで反映しようとした件数。収束する。**持続する非ゼロはアラート対象** --- mirakc が POST を拒否し続けている、または `MaxRecreatesPerPass` が低すぎる |
| `rokuban_reconcile_pending_diff{action="update_deferred"}` | 差分はあるが schedule の state が `scheduled` でないため**意図的に触らなかった**件数。**アラートしない** --- 録画中の番組の priority を変えると録画が終わるまで（数時間）非ゼロが続くのが正常。`update` と分けてあるのはこのため（[録画エンジン](../recording.md) §3.2「再作成のガード」） |
| `rokuban_reconcile_schedules_total{action="recreated"}` | 予約オプションの差分反映で DELETE → POST の再作成が成功した件数 |
| `rokuban_reconcile_schedule_lost_total` | 再作成で **DELETE には成功したが POST が失敗**し、schedule が消えたまま残った回数。**0 以外はアラート対象**。レベルトリガーで次パスが再作成するが、その間に開始時刻を越えると取りこぼす（[録画エンジン](../recording.md) §3.2「DELETE 成功 → POST 失敗」） |

`pending_diff` の `update` と `update_deferred` を分けているのは、このゲージの読み方が
「ゼロに戻らないまま続く = 収束できていない」だからである。**意図的に見送った差分を混ぜると
正常なユーザー操作でアラートが鳴り、ゲージがアラート不能になる。**

### 開始遅延検出器

録画開始は mirakc に委譲済みで Rokuban 側から防ぐ手段はないが、mirakc 側の未知の不具合への保険として、**「開始時刻を過ぎたのに `recording.started` が観測されない予約」を reconcile ループで検出する**。EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）のような事例に対応する。レベルトリガーの枠内で安価に実装できる。検出値は `rokuban_reconcile_start_delayed{site}` に出る（アラートの取り方は [アラート設計](alerts.md)）。

### 沈黙は保証ではない

観測のうち、**「出ていない = 大丈夫」と読んではいけない**ものを 1 箇所に
まとめる。設計上ここは全部「警告を見逃す」側に偏らせてある（過剰に警告しない代わりに
沈黙が保証にならない、という取引）。

| 沈黙 | 何を意味しないか | 何を見るか |
|---|---|---|
| `/api/capacity/overages` が空 | 収まるとは限らない。並走 EPGStation・ライブ視聴・EPG 収集は見えず、mirakc の `excluded_channels` は `/api/tuners` に載らないので**知る術がない** | `rokuban_tuners_projected` が 0 でないこと |
| 同上（射影が空） | 射影が 1 行も無いサイトは**何も主張しない**ので、同期が壊れると警告が黙って消える | `tuner_sync` の行と `tuner_sync_last_success` の鮮度 |
| `drop-stats` の `pidType` が無い | 分類できなかっただけで、ドロップ統計そのものは正しい | `packets` / `drops` は種別と独立に信頼できる |
| `pidType` が `other` | 音声でないとは限らない（LATM AAC は `other` に落ちる） | 4K/8K を録ったなら疑う |
| `/api/sites/{site}/programs/{programId}/overlaps` の `count = 0` | 録れるとは限らない（他サイトや mirakc の他の消費者は数えていない） | 重なりの手動確認（[docs/runbook/](../runbook.md) 側） |
| `/api/breakers` が空 | 削除が正しかったとは限らない。**閾値を下回る削除は素通りする** | `rokuban_ruler_reservations_total{action="deleted"}` の増え方 |
| `rokuban_reconcile_start_delayed` が 0 | 録画が始まったことの確認ではない（猶予 3 分の内側は検出しない） | `recordings.started_at` |

### 経緯と失敗事例

- `/metrics` エンドポイントは M1-9、開始遅延検出器は M2-7、record_sweep のジョブ化は M2-18、チューナー射影と `rokuban_capacity_overages` は M2-10、`catalog-export` が `--site` を取らない決定は issue #200。
- **`rokuban_reconcile_circuit_breaker_trips_total` は M2-5 で意味が変わった**（メトリクス名は既存のダッシュボード・アラートを壊さないため据え置き）。以前は「1 パスの削除数が閾値を超えた」を数えていたが、その件数ベースの判定は誤発火しかしないので撤去し、今は「desired が空なのに自分の schedule が観測される」という全損シグネチャの発動を数える。`rokuban_circuit_breaker_tripped` ゲージと、ブレーカーのラッチ化（発動遷移だけを数える）も同じ M2-5。
- `pending_diff` の `update` / `update_deferred` の分離は M2-4。
- 「沈黙は保証ではない」の表は M2 の手動検証 runbook から移設した（`pidType` が `other` の音声 PID は `gots` の `IsAudioContent()` の値域に従っているだけで、自前の `stream_type` 表は作らない方針。観測したら 1 行で足せる）。
