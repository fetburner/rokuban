# 運用

Rokuban の監視・アラート・DB 運用・ストレージ運用・k8s 運用に関する設計指針をまとめる。

## 1. 監視メトリクス

### エンドポイント（M1-9）

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

| 実装済みメトリクス | 型 | 対応する下記の項目 |
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
| `rokuban_reconcile_pending_diff{action}` | Gauge | reconcile 差分数（**収束すればゼロ**。アラートはこちら） |
| `rokuban_reconcile_schedules_total{action}` | Counter | 実際に差分を消した量 |
| `rokuban_reconcile_circuit_breaker_trips_total` | Counter | 全損シグネチャでの発動（M2-5 で意味が変わった。件数の閾値ではない） |
| `rokuban_circuit_breaker_tripped{site,breaker}` | Gauge | **いま止まっているか**（1 = 発動中）。ラッチなのでアラートはこちら |
| `rokuban_reconcile_last_pass_timestamp_seconds` | Gauge | 最後に完走したパスの時刻 |
| `rokuban_reconcile_start_delayed{site}` | Gauge | **開始時刻を過ぎたのに録画が始まっていない予約数**（M2-7）。収束すればゼロに戻る |
| `rokuban_epg_sync_duration_seconds` | Histogram | EPG 全量同期の所要 |
| `rokuban_epg_programs_projected` | Gauge | 直近パスの投影件数 |
| `rokuban_epg_channels_without_programs` | Gauge | 番組を返さなかったチャンネル数 |
| `rokuban_epg_sync_last_success_timestamp_seconds` | Gauge | 最後に成功した同期の時刻 |

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

未実装: 開始遅延検出器（下記）、エッジディスク残量（mirakc 側の値であり
Rokuban からは観測できない。node_exporter 等で別に取る）。

### 録画品質

mirakc の追従品質は EDCB ほどの長期実績がないため、品質メトリクスを継続計測する（[録画エンジン](recording.md) 参照）。

| メトリクス | ソース | 用途 |
|---|---|---|
| `recording.failed` 理由別カウンタ | watcher が mirakc SSE から受信。理由は構造化されている: `start-recording-failed` / `io-error` / `pipeline-error` / `need-rescheduling` / `schedule-expired` / `removed-from-epg` | 録画失敗の傾向分析、mirakc 品質の実測 |
| `recording.record-broken` | watcher が mirakc SSE から受信（理由付き、複数回あり） | 録画中の異常検出 |
| ドロップ統計（PID 別 continuity counter 不連続 / TEI） | ingest のインラインスキャン。188 バイト境界の TS パケット統計を転送中に読み取り専用で採取（追加 I/O パスゼロ） | EPGStation のドロップログ相当。PID 別サマリを `drop_stats` テーブルに格納し UI で表示 |
| scrambled カウンタ | ingest のインラインスキャン。`scrambling_control` ビットのカウント | B-CAS/復号障害の検出（後述のアラート対象） |

### ジョブ化されたループの監視（M2）

ruler / reconciler / record_sweep（watcher の 3 段構えのうち (c) 定期全量突き合わせ。M2-18）は River のジョブになったので、**「ループが止まっている」の検出方法が変わった**。advisory lock が取れているかではなく、**ジョブが投入され完走しているか**を見る。

| 見るもの | 意味 |
|---|---|
| `rokuban_*_last_pass_timestamp_seconds`（`reconcile` / `ruler` / `sweep`） | `time() - この値` が周期を大きく超えたら止まっている |
| `river_job` の `state='available'` が滞留 | 投入はされているが誰も引いていない（worker が 0 か、キューを引いていない） |
| `river_job` が増えない | **投入自体が止まっている**。`worker.periodic_jobs: false` なのに CronJob が動いていない、あるいはリーダーが不在 |

3 番目が k8s 特有の落とし穴。`PeriodicJobs` はリーダーだけが投入するので、worker が 0 にスケールすると誰も投入しない（[データ層](data.md) §2）。`rokuban enqueue` を叩く CronJob が設定されているかを最初に疑う。

手動で走らせたいときは `rokuban enqueue <job>`（`epg-sync` / `ruler-pass` / `reconcile-pass` / `record-sweep`）。既に待機中なら投入せず終了コード 0 を返すので、cron から重ねて叩いても安全。

**record_sweep には ruler / reconciler と違ってヒント経路（前倒し投入）がない**。定期投入だけが契機で、間隔は既定 5 分（`worker.RecordSweepInterval`、旧 watcher の `ReconcileInterval` を継承）。SSE 再接続をヒントにする案は検討したが、`internal/mirakc.Client.Subscribe` が再接続を内部に隠していて呼び出し側に通知できないため見送った（[録画エンジン](recording.md) §3.3「record_sweep の起動契機」）。取りこぼしの実害は SSE の (a)(b) が大半を吸収し、record_sweep は定期パスとして収束させる保険という位置づけなので、5 分間隔で十分と判断している。

### ruler（M2）

| メトリクス | 説明 |
|---|---|
| `rokuban_ruler_pass_duration_seconds` | 1 パス（全ルール x 全射影番組）の所要時間。射影が有界なので伸び続けることはない |
| `rokuban_ruler_reservations_total{action}` | `created` / `updated` / `deleted` / `gc`。**`updated` が毎パス予約数と同じ値で増え続けるなら差分書き込みが効いていない**（[録画エンジン](recording.md) §3.1） |
| `rokuban_ruler_circuit_breaker_trips_total` | 大量削除で停止した回数。EPG の一時欠損を疑う入口 |
| `rokuban_circuit_breaker_tripped{breaker="ruler_deletes"}` | **1 の間は導出削除が一切走らない**（手動再開まで止まるラッチ。M2-5）。カウンタと違い「いま止まっているか」に答える |
| `rokuban_ruler_last_pass_timestamp_seconds` | 最終パス時刻。`time() - この値` でパスが止まっていることを検出する（gauge が凍る問題への対策） |

`deleted` と `gc` は区別する。`deleted` は「ルールがマッチしなくなった」導出削除で**サーキットブレーカーの対象**、`gc` は「番組終了 + 猶予経過」の時間駆動で**対象外**（停止後の再開で大量に消えるのが正常）。

### ingest

| メトリクス | 説明 |
|---|---|
| ingest バイト数・所要時間 | 転送パフォーマンスの監視 |
| 未 ingest record 総量 | エッジのリングバッファ滞留量。エッジディスク残量と突き合わせてアラートの基礎とする |

ジョブは諦めず再試行し続ける（max attempts で dead-letter にすると record が宙に浮く）。長時間の転送失敗でエッジのリングバッファが溜まり続けるのが唯一の運用リスクであり、このメトリクスで可視化する。

#### River のジョブ一意性の注意

`UniqueOpts.ByState` は**必ず明示する**。River の既定（`UniqueOptsByStateDefault`）は
`completed` と `discarded` を含むため、既定のままだと「一度終わった引数のジョブは
二度と投入できない」になる。定期ジョブ（`epg_sync`）は実質ワンショットになり、
破棄された ingest も再投入できない。Rokuban は
`available / pending / retryable / running / scheduled` に限定している
（同時実行を防ぐのが目的で、処理済みかどうかは DB の状態が真実）。

`unique_states` は**行ごとに保存される** — 一意索引は
`river_job_state_in_bitmask(unique_states, state)` を条件に持つ。つまり
**古い設定で投入された行は設定を直しても鍵を占有し続ける**。この設定を変更した際は
既存の該当ジョブ行を一度削除する必要がある:

```sql
DELETE FROM river_job WHERE kind = 'epg_sync' AND state = 'completed';
```

### reconcile

| メトリクス | 説明 |
|---|---|
| `rokuban_reconcile_pending_diff{action="create"\|"delete"}` | desired（reservations）と observed（schedule_sync）の**存在**の差分。通常はゼロ付近に収束する |
| `rokuban_reconcile_pending_diff{action="update"}` | 予約オプション（priority / tag）の差分のうち、このパスで反映しようとした件数（M2-4）。収束する。**持続する非ゼロはアラート対象** --- mirakc が POST を拒否し続けている、または `MaxRecreatesPerPass` が低すぎる |
| `rokuban_reconcile_pending_diff{action="update_deferred"}` | 差分はあるが schedule の state が `scheduled` でないため**意図的に触らなかった**件数。**アラートしない** --- 録画中の番組の priority を変えると録画が終わるまで（数時間）非ゼロが続くのが正常。`update` と分けてあるのはこのため（[録画エンジン](recording.md) §3.2「再作成のガード」） |
| `rokuban_reconcile_schedules_total{action="recreated"}` | 予約オプションの差分反映で DELETE → POST の再作成が成功した件数 |
| `rokuban_reconcile_schedule_lost_total` | 再作成で **DELETE には成功したが POST が失敗**し、schedule が消えたまま残った回数。**0 以外はアラート対象**。レベルトリガーで次パスが再作成するが、その間に開始時刻を越えると取りこぼす（[録画エンジン](recording.md) §3.2「DELETE 成功 → POST 失敗」） |

`pending_diff` の `update` と `update_deferred` を分けているのは、このゲージの読み方が
「ゼロに戻らないまま続く = 収束できていない」だからである。**意図的に見送った差分を混ぜると
正常なユーザー操作でアラートが鳴り、ゲージがアラート不能になる。**

### 開始遅延検出器

録画開始は mirakc に委譲済みで Rokuban 側から防ぐ手段はないが、mirakc 側の未知の不具合への保険として、**「開始時刻を過ぎたのに `recording.started` が観測されない予約」を reconcile ループで検出する**。EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）のような事例に対応する。レベルトリガーの枠内で安価に実装できる。

## 2. アラート設計

### scrambled > 0（B-CAS / 復号障害）

復号が正常なら `scrambling_control` は常にゼロのはず。**scrambled > 0 は放送品質ではなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味するので、ドロップ数とは別枠のアラート対象とする。EPGStation ドロップログの scramble 列と同じ役割。

### エッジディスク残量（未 ingest 滞留）

未 ingest record 総量メトリクスとエッジディスク残量を突き合わせてアラートする。回線断・クラウド側障害時に未 ingest の record が溜まり続けるシナリオへの備え（[ストレージ](storage.md) のサイジング指針と対）。

### 大量削除サーキットブレーカー発動

EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう（EPGStation#692 の障害クラス）。

- 1 回の ruler パスでの削除数に閾値（`ruler.max_deletes_per_pass`）を設け、超えたら削除を実行せず停止してアラート
- 削除エンジンの物理 unlink についても、ソースを問わず 1 パスの物理削除が閾値（件数 / ライブラリ比率 / 総バイト数、例: 5% or 100 GB）を超えたら停止してアラート

**アラートは `rokuban_circuit_breaker_tripped{site,breaker}` を見る**（M2-5）。既存の
`*_circuit_breaker_trips_total` は「何回発動したか」しか答えられないが、ブレーカーは
**手動で再開するまで止まり続けるラッチ**なので「いま止まっているか」が知りたい情報である。

| ブレーカー | 何を守るか | 発動したら |
|---|---|---|
| `ruler_deletes` | ルール x EPG の評価から導出された予約削除 | `GET /api/breakers` の `detail` で消されようとしていた番組を確認 → 正当なら `POST /api/sites/{site}/breakers/ruler_deletes/resume`（`site` は一覧のレスポンスにある値） |
| `reconcile_total_loss` | 「desired が空なのに自分の schedule が観測されている」という全損シグネチャ | DB 接続・`reservations` の中身を確認。**件数の閾値ではない**ので、発動したら本当に異常である |

**1 が続く間、導出削除は一切実行されない。** これは「reconcile が収束できていない」ではなく
「人間の確認を待っている」を意味する。放置すると mirakc 側に不要な schedule が残り続けるため、
`for` の待ち時間を長く取らず即座に通知する。

発動中でも**削除以外は動く**（予約の作成・base の更新・schedule の作成・番組終了後の GC）。
「録画されない」ではなく「消えないものが残る」障害なので、慌てて resume せず `detail` を
確認してからにする。

### 開始時刻超過で recording.started 未観測

`rokuban_reconcile_start_delayed > 0` でアラートする（M2-7）。**`for` を長く取らないこと** ---
検出窓が「開始 + 猶予 〜 終了時刻」に限定されているので、番組が終わればゲージは自然にゼロへ戻る。
待ち時間を番組長より長くすると、短い番組の遅延を一度も通知しないまま取りこぼす。

`rokuban_recordings_failed_total{reason}` が同時に増えているなら mirakc が理由を返しているので
そちらが一次情報になる。**増えていないのにこのゲージが立つのが最も危険**な状態で、mirakc が
失敗を報告せずに録画を始めていないことを意味する（EPGStation#724 のクラス）。

## 3. DB 運用

### 輻輳時の隔離

「ユーザー操作で DB が詰まったら録画やエンコードに影響しないか」という懸念への対策。故障モードは常に「収束の遅れ」であり、仕事の喪失ではない:

- **録画は影響を受けない（DB 全停止でも走る）**。スケジュールは mirakc 側の `schedules.json` に永続化済みで、録画実行は mirakc が自律的に行う。DB 輻輳の影響は「新規・変更予約の同期遅延」（reconciler のラグ）のみ
- **実行中のエンコード・ingest も影響を受けない**。ジョブは claim / complete 時に小さなトランザクション（SKIP LOCKED）で DB を触るだけで、実行中（ffmpeg・転送）は DB に依存しない
- **ルール評価は UI と同期しない**。ルール編集 API は編集を書いて再評価ジョブを投入するだけで即応答

実装規律（issue #90 で実装済み）:

- **ロール別コネクションプール上限を分ける**。api が全コネクションを食い潰して worker / reconciler が待つ事態を防ぐ。
  ただしプロセスは常に 1 個のコネクションプールしか持たない（`cmd/rokuban/server.go` が起動時に 1 回だけ作り、
  そのプロセスが担う全ロールが共有する）。したがって「ロール別」とは複数プールを作ることではなく、
  **そのプロセスが担う roles 集合から、そのプロセスが持つ唯一のプールの `MaxConns` を決める**ことを指す
  （`internal/db.NewPool`）。`db.max_conns` を明示すればそれを使い、未指定ならロールごとの
  budget（api: 10 / worker: 8 / watcher: 3 / notifier: 3 / streamer: 4。根拠は `internal/db.roleConnBudget` の
  doc コメント）を roles の分だけ合計する。monolith（`--all`）は全ロール分の合計になる
- **API 系クエリに `statement_timeout`** を設定する。クエリ単位の context timeout だと「付け忘れた 1 本」が
  必ず生まれるため、接続の `RuntimeParams`（起動パケットの session default）で一括適用する
  （`db.api_statement_timeout`、未指定なら 30s）。**api ロールを含むプロセスのプール全体に適用される**
  ため、monolith で api と worker/watcher を同居させると worker 側のクエリにも同じ上限がかかる —
  世帯スケールの通常クエリを十分に上回る値（既定 30s）にしてあるので実害は想定していないが、
  ロールを分離すれば worker 単独プロセスには一切適用されなくなる

### pooler 越しに置けるのは api ロールだけ

`db.pooler_compat: true` は PgBouncer / Neon pooler の **transaction pooling** 越しの接続を想定したモードで、
pgx の prepared statement キャッシュを無効化する（`DefaultQueryExecMode` を `QueryExecModeExec` にする）。

これはデプロイの契約であり、**pooler を通せるのは api ロールだけ**である。worker（River 内部の
elector / notifier が使う LISTEN）・watcher（advisory lock によるリーダー選出）・notifier（ブラウザへの
SSE 配送のための LISTEN）はいずれもセッション状態に依存するため、transaction pooling で物理コネクションが
要求ごとに入れ替わると構造的に壊れる（[data.md](data.md) §2 / §3）。`internal/db.NewPool` は
`pooler_compat: true` と worker/watcher/notifier のいずれかのロールの組み合わせを起動時エラーにする
（fail-fast。streamer は LISTEN も advisory lock も使わないため pooler と組み合わせてよい）。

### EPG churn / autovacuum

EPG テーブルは 1 日に何度も大量 upsert されるため、遅くなるとしたら検索ではなく書き込みと autovacuum の追従。対策:

- バッチ upsert
- GIN fastupdate
- **テーブル別 autovacuum チューニング**（EPG テーブルに対して `autovacuum_vacuum_scale_factor` を小さく設定する等）

EPG テーブルは 8 日分 + 猶予のローリングウィンドウであり、永遠に太らない。検索性能自体は世帯スケール（数万〜20 万行）では問題にならない。

### Postgres datadir とエンコード scratch の分離

monolith モードでは Postgres のデータディレクトリとエンコードの scratch が同じディスクに載りやすい。実際に競合しやすいのはロックよりディスク I/O であり、**両者を同じディスクに置かない構成を推奨する**。

### バックアップ

保護対象は「ルール・録画履歴・media_assets・ドロップ統計・tombstone・手動オーバーライド」のみ（数 MB）。EPG プロジェクションは mirakc から再構築可能、ジョブキューは一時的。

- **catalog エクスポート**: worker の定期ジョブが、コアデータを JSON でメディアストレージ自身の `catalog/` 配下に書き出す（日次 + 世代保持）。メディアが生き残る障害では catalog も一緒に生き残る。pg_dump に依存しない（distroless イメージに postgres クライアント不要）アプリレベルのエクスポート
- **pg_dump（推奨・非必須）**: フル忠実度が欲しい場合の日次 pg_dump 構成例をドキュメントに記載する
- 世帯スケールでは catalog + 任意の pg_dump で十分。WAL アーカイビングは過剰

## 4. ストレージ運用

### 録画バッファのサイジング

録画バッファ（mirakc `recording.basedir`、エッジのローカルディスク）のサイジング指針:

- **容量の支配項は同時録画数ではなく「ingest が詰まったときの滞留分」**。回線断・クラウド側障害時は未 ingest の record が溜まり続ける。推奨値は**「N 日分の全録画を保持できる容量」**（地デジ約 7 GB/時で見積り）
- **速度要件は絶対帯域ではなくレイテンシ**。書き込みは 1 録画あたり約 2 MB/s（地デジ 17 Mbps）で、同時 8 本でも 16 MB/s に過ぎない。怖いのは他 I/O との競合によるレイテンシスパイクで、ingest pull のサイト単位 1〜2 本キャップはこのための決定でもある

録画バッファの容量アラート（前述の「未 ingest record 総量」メトリクスとエッジディスク残量アラート）とセットで運用する。

### アーカイブの速度要件

アーカイブ（Rokuban のメディアストレージ。ローカル FS / NAS / CSI の S3）は低速で良い:

- **平均スループット >= 1 日の録画総量 / 24 時間**。瞬間的な変動は録画バッファが吸収するので、リアルタイム性は一切要求されない。エンコードの読み出しもバッチなので遅くて良い
- 唯一レイテンシが人間に見えるのは**再生時のシーク**（S3 + FUSE の range read）。原本削除ポリシーと組み合わせた「視聴は H.265 派生物、原本は消すか S3 の奥」という運用が前提なら実用上問題にならない見込み

### disaster recovery（catalog + rescue）

`rokuban rescue`: ストレージを走査し、

- `catalog/` があれば照合してフルメタデータ（番組情報・ドロップ統計・保持ポリシー）ごと復元
- catalog にないファイルはディレクトリ規約・ファイル名から推定できる範囲で「素の asset」として登録（UI から見えて再生できる状態に戻す）

実装は `rokuban import epgstation` の in-place 登録機構と同型なので共有する。

既存不変条件の再確認:

- **「放送データのコピーが常に 1 つ以上」は DB 喪失時も維持される**: エッジ record の削除は ingest の DB コミット後 → コミット直後に DB を失ってもファイルはアーカイブに存在し、孤児回収の安全弁が守り、rescue が再登録する
- cleanup は mirakc の basedir に絶対に触らない（エッジ側削除は ingest の検証済み削除のみ）

## 5. k8s 運用

### ロールとキュー購読の関係（issue #113）

**worker ロールだけが River のキューを引く。** 他のロールは、そのプロセスが実際に
worker ロールを持つかどうかに関わらず、ジョブを実行しない。ロール分割デプロイ
（M4）で `--roles watcher` のような worker を含まない構成を組んだときに、
起動時検査が実態より広い安心を与える経路が過去にあったため（issue #113）、
現在は次の 2 点で構造的に保証している:

1. **watcher 単独プロセスは River クライアントを Start しない**（`--roles worker`
   を含む場合だけ `river.Client.Start` を呼ぶ）。加えて watcher 単独プロセスは
   `worker.NewWorkers` のフルのワーカー群（`EncodeWorker` / `ThumbnailWorker` を
   含む）を登録しない --- api ロールと同じ `worker.NewInsertOnlyClient`（ingest
   ジョブの `InsertTx` 専用、`Queues` / `Workers` を持たないため `Start` 自体が
   呼べない構成）を使う。watcher が必要とするのは ingest ジョブの投入だけであり、
   実行（キューの購読）ではないので、これで用は足りる
2. **`worker.queues` は worker ロールが引くキューの絞り込みであり、既定（空）は
   全キュー購読。** worker ロールが無いプロセスはこの設定に関わらずキューを
   一切引かない（`configuration.md` の `worker.queues`）

ffmpeg/ffprobe の起動時検査（不変条件 4）も、実際に encode/thumbnail キューを
購読するときだけ行う（`worker.RequiresEncodeTools`）。`worker.queues` で
encode/thumbnail を明示的に除外した worker Pod（例: ingest 専用 Pod）は ffmpeg
の存在を要求されない。逆に既定（全キュー購読）や encode/thumbnail を含む設定では
起動時に LookPath で検査し、無ければ即座に落ちる --- ffmpeg が無い環境で
encode/thumbnail ジョブが River の再試行を焼き続けてから気付く、という壊れ方を防ぐ。

### worker: KEDA ScaledJob（長時間ジョブ保護）

長時間バッチ（数時間のエンコード / ingest）には Deployment + HPA ではなく **KEDA ScaledJob** を使う。キューアイテムごとに k8s Job を起こす形にすると、**ジョブは完走するまで殺されない** --- スケールインは「新しい Job を起こさない」ことで実現され、実行中の犠牲者選定という問題自体が消える。

River の at-least-once / 冪等性は「殺されても正しい」を保証済みであり、この決定は「殺されても安い」を足すもの。

### 将来オプション: チャンク並列エンコード

preemption 対策は上記の ScaledJob で十分であり、チャンク化の価値はクラウドバーストによる高速化（数時間のエンコードを worker N 台で数分の壁時計時間に短縮）に限られる。自宅の HW エンコード（QSV/VAAPI）は実時間の数倍で足りるため価値が薄く、クラウド構成専用のオプション。初期実装には含めない。

実装方針の見立て: 実ファイルは分割せず、各ジョブに (開始時刻, 長さ) を渡して 2 段 seek（`-ss` を `-i` の前後で併用）でフレーム精度の境界を出す。映像はチャンクごとに独立ジョブ（境界は強制 IDR）。音声は音声フレーム境界のズレによる接合ノイズを避けるため分割せず、全体を 1 パスで別エンコードして最後に mux（ISDB の番組途中の音声レイアウト切替の正規化もここに集約）。全チャンク完了後に concat demuxer でロスレス結合 + 検証の fan-in ジョブ。構造化エンコードプロファイル（[storage.md](storage.md)）とは独立な executor の戦略なので、プロファイル定義に手を入れず後付けできる。

### Deployment 併用時: SIGTERM drain + pod-deletion-cost

Deployment 型で worker を運用する場合（またはその併用）の定石:

- SIGTERM で **drain**（実行中ジョブは完走、新規 claim 停止）+ 長い `terminationGracePeriodSeconds`
- busy な worker が `controller.kubernetes.io/pod-deletion-cost` を上げてスケールイン犠牲者から外れる

### シングルトンロール: pg_advisory_lock リーダー選出

watcher はシングルトンロール。`pg_try_advisory_lock` による監督ループでリーダー選出を行う（ruler / reconciler / record_sweep はジョブなので対象外。[データ層](data.md) §2）。ただし watcher の singleton 性はもはや「正しさ」の要件ではなく、「mirakc に N 本の SSE を張らない」という接続数の配慮に過ぎない（M2-16 で `processRecord` を冪等化済み。[データ層](data.md) §2、[録画エンジン](recording.md) §3.3）:

1. ロールごとに goroutine を立て、`pg_try_advisory_lock` を定期試行（15s + jitter）
2. 取得したら child context でロール本体を起動
3. リーダー中はロック専用コネクションに定期 heartbeat（`SELECT 1`、10s 間隔）。失敗 = リーダーシップ喪失とみなし、ロールを停止して取得ループに戻る
4. セッション断で PG 側ロックが自動解放されるため、待機プロセスが次の poll で取得しフェイルオーバー成立

k8s の Lease API に依存しないため monolithic mode でも同じコードが動く（[データ層](data.md) 参照）。フェイルオーバー遅延は最大 poll 間隔（〜15s）だが、いずれも定期 reconcile 前提のロールなので許容範囲。短時間の split-brain はシングルトンロールの仕事がすべて冪等（レベルトリガー + 冪等原則）であるため安全。

**`internal/role`（`RunSingleton`）は watcher 専用になったが畳まない**（issue #24 M2-20）。M2-17 / M2-18 で
ruler / reconciler / record_sweep がジョブになり、利用箇所は `cmd/rokuban/server.go` の 1 箇所だけになった。
それでも独立したパッケージとして残すのは、**「ソケットを connect し続ける」という形のロールが存在する限り
必要な機構**であり（[overview.md](overview.md) §ロール分類の基準）、リーダー選出の失敗モード
（heartbeat 喪失・split-brain・フェイルオーバー遅延）はテストで固定しておく価値が単独である。
呼び出し元が 1 つであることは、機構の複雑さが 1 つに減ったという成果であって、削除の根拠ではない。

### healthz: liveness のみ

`/healthz` は **liveness probe 専用**。依存サービス（DB・mirakc）の状態は一切チェックせず、プロセスが HTTP を返せる限り常に 200 を返す。

- 不変条件 1「api ロールは mirakc に問い合わせない」により、mirakc チェックは構造的に不可
- ハイブリッド構成（overview.md）ではクラウド側 api から mirakc に到達できないのが正常状態
- liveness に依存チェックを入れると「依存ダウン → 全プロセス再起動ループ」になる（liveness probe の定番アンチパターン）
- DB は起動時に fail-fast で検証済み。ランタイムの DB 断は各ロールがリトライ / クラッシュで対処する（crash-only 原則）

mirakc の健全性は watcher が `observed_at` として DB に記録し、UI / アラートで可視化する。ロードバランサ向けの readiness が必要になったら `/readyz`（DB ping）を別エンドポイントとして追加する。

### DB 接続失敗: fail-fast + 明示ログ

DB 接続失敗はエラーを握り潰さず fail-fast + 明示ログとする（EPGStation#628 の教訓、crash-only 方針と整合）。

### ネットワーク FS ハング: ジョブストール検知 + 外部 liveness

ネットワーク FS のハング（EPGStation#721）は worker ジョブがストールしうる。対策:

- **ジョブのストール検知**: ingest のタイムアウトは総時間ではなく**ストール検知**（N 秒間無進捗で切断扱い）。総時間タイムアウトは遅い回線の正常な転送を殺す
- **外部 liveness**: k8s の liveness probe / systemd watchdog を推奨構成に含める
