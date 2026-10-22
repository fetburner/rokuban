> [recording.md](../recording.md) §3「Rokuban 側のコンポーネント」の一部。索引から辿る。

### 3.3 watcher（SSE 購読・状態反映）

watcher は mirakc のイベント配信を購読し、record が録画中か録画済みになったのを見たら取り込みジョブを投入する。録画中の追従を録画済み後の追い付きより優先し、生録画がバックログに埋もれないようにする。

<details><summary>根拠</summary>

- 購読するイベント配信: `/events` SSE
- 投入の契機: `recording.record-saved` で `recordingStatus: recording` または `finished` を観測したとき
- 優先度: 録画中の追従は River priority 1、finished 後の追い付きは priority 2

</details>

#### 3 段構えの信頼性設計

record の反映と、録画済み record の取り込み漏れは、3 つの仕組みを重ねて防ぐ。重複するイベントは record 単位で冪等に投入する。watcher の停止中の取りこぼしは、接続時に全 record が再送されることで回復する。最後は定期的な全量の突き合わせが正しい状態を決める。

| 段 | 防ぐもの | 仕組み | 形 |
|---|---|---|---|
| (a) | 同じ record への重複・順不同のイベント | record id で冪等投入 | 常駐 |
| (b) | watcher の停止中の取りこぼし | 接続時の全 record 再送 | 常駐 |
| (c) | (a)(b) が漏らした record の反映と取り込み | 全量取得と DB の突き合わせ | ジョブ |

この 3 つがあるので、record の反映漏れと録画済み record の取り込み漏れは構造的に起きない。(c) はレベルトリガーで、イベントはヒントにすぎない。(b) は mirakc が接続時に再送する挙動そのものである。

**権威（レベルトリガー）がジョブで、ヒント源が常駐**という配置になった。ruler / reconciler も「定期パスが状態の権威、作成・更新イベントはヒント」という形をジョブとして持ち、watcher の (c) も同じ形にはまる。(a)(b) は長寿命の接続でしか実現できないヒント経路なので、常駐に残る。(c) のジョブは watcher の突き合わせを呼ぶだけで、ロジックを移植しない。

##### 失敗イベントの回収範囲

3 段構えが保証するのは record の反映と取り込みだけで、失敗イベント全般の回収は保証しない。録画失敗は、API に失敗した予定か record が残っていれば、突き合わせで再構成できる。残っていない録画失敗と record 破損は、イベント配信でしか拾えない。

録画失敗と record 破損のイベントは、record の状態反映とは別の品質イベントである。mirakc が接続時に再送するのは、record の保存イベントだけである。

突き合わせが失敗を再構成できる範囲は、次の 3 通りである。

1. 失敗した予定（schedule）が失敗理由付きで残っていて、Rokuban の予約として特定できる場合は、record が無くても失敗の録画行と品質イベントを作る。予定が mirakc から削除された後は、この経路では回収できない
2. 失敗状態と失敗理由を持つ record が残っている場合は、通常の record と同じく反映し、失敗理由も品質イベントに残す。これは mirakc が失敗 record のメタデータを保持している場合に限る
3. record が無く、失敗した予定も削除済みか、失敗理由が返らない録画失敗は、API から再構成できない。イベント配信でしか拾えず、突き合わせでは回収しない。record 破損も、イベントの record ID と理由を API が履歴として返さないので、イベント配信でしか拾えない

「コンテンツが無い」record が API に見えても、そこから record 破損が何回起きたかを推測して品質イベントを作らない。

失敗の録画行は、削除・置換されていない録画行を放送イベントごとに 1 行とする一意制約により、イベント配信と突き合わせのどちらが先でも 1 行に収束する。突き合わせが同じ失敗を次回以降も観測しても、同じイベントと理由の品質イベントは 1 度だけ追記する。一方、イベント配信で届く record 破損や繰り返しの録画失敗は、観測したイベントの履歴として既存の追記経路に残す。

<details><summary>根拠</summary>

- 冪等投入: River unique job
- 常駐の購読と処理: `Watcher.Run` の SSE 購読 + `handleEvent`
- (c) のジョブ: `internal/worker.RecordSweepWorker`（`record_sweep`）が `Watcher.Sweep` を呼ぶ
- 品質イベント: `recording.failed` / `recording.record-broken`。接続時に再送されるのは `record-saved` だけ
- 経路 1 の取得元: `GET /api/recording/schedules` の `state=failed` と `failedReason`。作るのは `recordings.status=failed` と `quality_events`
- 経路 2 の取得元: `GET /api/recording/records` の `recording.status=failed` と `recording.failedReason`。反映先は `record_sync` / `recordings`
- 1 行への収束: active-event の一意制約

</details>

#### record 処理は並行実行しても問題が起きない

同じ record を 2 つの経路が同時に処理しても、録画行は 1 つしかできない。record の同期行を先に確保して行ロックを取り、その後で録画行を作るからである。2 つ目の処理は 1 つ目のコミットを待ち、録画行が既に紐づいているのを見る。

同時に処理する経路とは、イベント由来の (a) と突き合わせジョブの (c)、あるいは 2 つのプロセスである。行ロックが無いと、両方が「行なし」を見て両方が録画行を作り、放送イベントの部分一意索引の違反で片方が失敗する。既にある主キーを使うだけなので、勧告ロックのような追加の機構は要らない。

この性質があるので、**watcher のシングルトン性は「正しさ」の要件ではない**。シングルトンにしている理由は、mirakc に N 本の購読を張らないという接続数の配慮だけで、複数動いても問題は起きない（取り込みジョブは record id で冪等）。3 段構えの (c) をジョブとして切り出せたのは、この前提があるからである。(a) と (c) が並行に走っても録画行が重複しないことは、テストで固定してある。

<details><summary>根拠</summary>

- 処理の入口: `processRecord`
- 先に確保する行: `record_sync` の `(site, record_id)`。その後に `recordings` を作り、2 つ目は `recording_id` が埋まっているのを見る
- 行ロックが無い場合の重複作成: `createRecording`
- 違反する索引: `recordings_unique_active_event`
- 要らない機構の例: `pg_advisory_xact_lock`

</details>

#### record_sweep の起動契機

突き合わせジョブは定期的にだけ起動し、イベントで前倒しする経路を持たない。ruler / reconciler と違い、前倒しに使える自然なヒントが無いからである。

| 契機 | 種別 | 既定の間隔 |
|---|---|---|
| 定期 | 権威 | 5 分 |

ジョブは mirakc の全 record を取得し、DB に同期されていない録画済み record を補う。プロセスが死んで実行中のまま残った取り込みジョブは、ジョブキューの回収機構が拾う。突き合わせジョブがジョブキューの行を直接変えることはない。定期の投入は、デプロイ形態に応じてジョブキューの定期実行か k8s CronJob（`rokuban enqueue record-sweep`）が行う（[データ層](../data.md) §2）。

ruler / reconciler は、作成・更新を commit した後に、そのイベントをヒントとして投入できる。突き合わせジョブには、それに当たる自然なヒントが無い。最も自然な候補は購読の再接続である。切れて再接続したことは、取りこぼした区間がありうる合図になる。しかし mirakc クライアントは再接続を内部で自動リトライするだけで、呼び出し側の watcher に再接続を知らせる仕組みを持たない。知らせるには購読の設定に再接続のフックを足す設計判断が要るので、それは見送り、定期投入だけにしている。

<details><summary>根拠</summary>

- 突き合わせの処理: `processRecord` で補う
- 実行中のまま残ったジョブの回収: River の `JobRescuer`。ジョブ行は `running` のまま残る
- 定期投入: River `PeriodicJobs`
- 既定 5 分: 旧 watcher の `ReconcileInterval` を継承
- 再接続の処理: `internal/mirakc.Client.Subscribe`
- 足すなら要るフック: `mirakc.SSEConfig` に `OnReconnect` のようなもの

</details>

#### 品質メタデータ記録

録画失敗と record 破損のイベントは、構造化された品質シグナルとして record に紐づけて DB に記録する。これは「録画品質の実測」計画の入力になる。録画失敗は「失敗イベントの回収範囲」で述べた範囲で突き合わせからも補うが、record 破損と、API に残らない record の無い録画失敗は、イベント配信でしか拾えない。

<details><summary>根拠</summary>

- 記録するイベント: `recording.record-broken` / `recording.failed`
- イベント配信でしか拾えないもの: `record-broken` と、API に残らない record の無い failed

</details>

#### 開始遅延検出器

開始時刻を過ぎても録画開始が観測されない予約を、reconcile のループで検出してアラートを出す。録画開始は mirakc に委譲済みで、Rokuban 側から遅延を防ぐ手段は無い。mirakc 側の未知の不具合（EPGStation#724 では、チューナーの再接続ハングで開始が 10 分遅れた）への保険として置く。

既存の品質メトリクス（録画失敗・record 破損・ドロップ統計）に加える検出で、レベルトリガーの枠内で安価に作れる。

##### 観測と検出窓

開始遅延は、開始時刻に猶予を足した時刻から、終了時刻までの間だけ検出する。終了時刻を過ぎた予約まで拾い続けると、終わった番組のアラートが鳴り止まなくなるからである。開始遅延は、まだ間に合う可能性がある時間帯の話である。

終了時刻を過ぎた予約は、録画予定が作られなかった予約を拾う検出の担当になる。録画開始の観測は、watcher が mirakc の record から書く開始時刻で見る。録画行そのものが無い場合も「観測なし」とする。

放送イベントキーの照合も、記録された録画開始時刻（観測値）の時間窓に閉じる。原則は直近 24 時間で、24 時間を超える候補があれば、番組の開始時刻の 24 時間前まで広げる。記録される開始時刻は予定の 15 秒前になるので、予定の開始時刻ちょうどを下界にはしない。

猶予（`reconciler.start_delay_grace`、既定 3 分）は、開始直後のイベント到達と watcher の処理の遅れを誤検知しないためのものである。ゼロにすると毎回誤検知する。

録画しない意図の予約と、孤児になった予約は対象外である。前者は始まらないのが正常で、後者は既に「録れなかった」と記録されている。

##### 検出結果の表し方

開始遅延は DB に新しい状態として持たせず、ゲージ 1 つで表す。毎パス再計算できる導出値だからである（不変条件 5）。

ゲージは `rokuban_reconcile_start_delayed{site}` である。品質イベントの表には書かない。その表は録画行に紐づく列を持ち、録画が始まっていない番組には録画行が無いことがあるからである（§3.2「DELETE 成功 → POST 失敗」と同じ制約）。

<details><summary>根拠</summary>

- 実装: `reconciler.detectStartDelays`
- 観測の有無: `recordings.started_at`。`recordings` 行そのものが無い場合も「観測なし」
- 検出窓: `開始時刻 + 猶予 < now() < 終了時刻`
- 終了時刻を過ぎた予約の担当: `recordNeverScheduled`
- 記録される開始時刻: `started_at` は予定 − 15 秒
- 対象外の予約: `effective.skip` と `orphaned`
- 書かない表: `quality_events`

</details>

---
