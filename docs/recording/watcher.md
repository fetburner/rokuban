> [recording.md](../recording.md) §3「Rokuban 側のコンポーネント」の一部。索引から辿る。

### 3.3 watcher（SSE 購読・状態反映）

`/events` SSE を購読し、`recording.record-saved` で `recordingStatus: finished` になったら ingest ジョブを投入する。

#### 3 段構えの信頼性設計

| 段 | 内容 | 形 |
|---|---|---|
| (a) | `record-saved` は同一 record に複数回・順序保証なしで飛ぶ → **record id で冪等投入**（River unique job） | **常駐**（`Watcher.Run` の SSE 購読 + `handleEvent`） |
| (b) | watcher ダウン中の取りこぼし → **SSE の接続時全 record 再送**で回復 | **常駐**（同上。mirakc 側が接続時に再送する挙動そのもの） |
| (c) | SSE はあくまでヒント → **定期的な schedules / records API の全量取得と DB の突き合わせ**（レベルトリガー）が真実 | **ジョブ**（`internal/worker.RecordSweepWorker`、`record_sweep`。ロジックは `Watcher.Sweep` を呼ぶだけで移植しない） |

この 3 つで record の反映と finished record の ingest 漏れは構造的に起きない。

ただし、この 3 段構えが保証する範囲は record と ingest に限る。`recording.failed` と
`recording.record-broken` は record の状態反映とは別の品質イベントで、mirakc が接続時に
再送するのは `record-saved` だけである。したがって、失敗イベント全般を SSE の取りこぼし
なしで回収できる、という意味ではない。

`Watcher.Sweep` が failed を再構成できる範囲は次の通りである。

- `GET /api/recording/schedules` に `state=failed` と `failedReason` が残っている、かつ
  Rokuban の予約として特定できる schedule は、record が無くても `recordings.status=failed`
  と `quality_events` を作る。schedule が mirakc から削除された後はこの経路では回収できない。
- `GET /api/recording/records` に残る `recording.status=failed` と `recording.failedReason` を
  持つ record は、通常の record と同じく `record_sync` / `recordings` に反映し、失敗理由も
  `quality_events` に残す。これは mirakc が失敗 record のメタデータを保持している場合に限る。
- record が無く、failed schedule も既に削除済み、または `failedReason` が返らない
  `recording.failed` は API から再構成できないため、SSE 専用で sweep では回収されない。
  `recording.record-broken` も同様に、イベントの record ID と理由を API が履歴として返さない
  ため SSE 専用である。records API に「コンテンツが無い」record が見えても、それだけから
  どの record-broken が何回発生したかを推測して品質イベントを捏造しない。

failed 行は active-event の一意制約で SSE と sweep のどちらが先でも一行に収束する。
sweep が同じ失敗を次回以降も観測しても、同じ event と reason の品質イベントは一度だけ
追記する。一方、SSE で届く `recording.record-broken` や繰り返しの `recording.failed` は
観測されたイベント履歴として既存の追記経路に残す。

**真実（レベルトリガー）がジョブで、ヒント源が常駐**という配置になった。ruler / reconciler が「定期パスが真実、作成/更新イベントはヒント」という形をジョブとして持つのと対称で、watcher の (c) も同じ形にはまる。(a)(b) は SSE という長寿命コネクションでしか実現できないヒント経路なので常駐に残る。

#### record 処理は並行実行しても壊れない

`processRecord` は `record_sync` の `(site, record_id)` 行を**先に確保して行ロックを取ってから** `recordings` を作る。同一 record を 2 つの経路（SSE 由来の (a) と record_sweep ジョブの (c)、あるいは 2 プロセス）が同時に処理しても、2 つ目は 1 つ目のコミットを待ってから `recording_id` が埋まっているのを見る。

これがないと両方が「行なし」を見て両方が `createRecording` し、部分ユニークインデックス `recordings_unique_active_event` 違反で片方が失敗する。既にある PK を使うだけなので、`pg_advisory_xact_lock` のような追加の機構は要らない。

この性質があるので **watcher のシングルトン性は「正しさ」の要件ではない**。残っている理由は「mirakc に N 本の SSE を張らない」という接続数の配慮で、壊れるわけではない（ingest ジョブは record id で冪等）。3 段構えの (c) を record_sweep ジョブとして切り出せたのはこの前提による（(a) と (c) が並行に走っても `recordings` が重複しないことをテストで固定してある）。

#### record_sweep の起動契機

ruler / reconciler と違い、**起動契機は定期のみ**（ヒントで前倒しする経路を持たない）。

`record_sweep` は全量取得を始める前に、プロセス死で `running` のまま残った ingest の回収も
行う。最後の進捗時刻が古い行を候補にするが、ジョブ ID の advisory lock を取得できた場合だけ
死亡と確定し、旧行を終端化して新しい ingest を投入する。生きている転送は lock を保持して
いるため、進捗時刻が古くても回収しない。

| 契機 | 種別 |
|---|---|
| 定期（既定 5 分、旧 watcher の `ReconcileInterval` を継承） | **真実**。デプロイ形態に応じて River `PeriodicJobs` か k8s CronJob（`rokuban enqueue record-sweep`）が投入する（[データ層](../data.md) §2） |

ruler / reconciler は「作成・更新イベント」というヒントを同一トランザクションで投入できたが、record_sweep には対応する自然なヒントがない。**最も自然な候補は SSE の再接続**（切れて再接続した = 取りこぼした可能性がある区間ができた合図）だが、`internal/mirakc.Client.Subscribe` は再接続を内部で処理して自動リトライするだけで、呼び出し側（watcher）に再接続を通知する仕組み（コールバック等）を持たない。追加するなら `mirakc.SSEConfig` に `OnReconnect` のようなフックを生やす設計判断が要るため見送り、定期投入のみとしている。

#### 品質メタデータ記録

`recording.record-broken` / `recording.failed` イベントは構造化された品質シグナルとして
record に紐づけて DB に記録する（「録画品質の実測」計画の入力）。`recording.failed` は
上記の範囲で schedules / records sweep からも補完するが、`record-broken` と API に残らない
recordless failed は SSE 専用である。

#### 開始遅延検出器

録画開始は mirakc に委譲済みで Rokuban 側から防ぐ手段はないが、EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）のような mirakc 側の未知の不具合への保険として、**「開始時刻を過ぎたのに recording.started が観測されない予約」を reconcile ループで検出してアラート**する。既存の品質メトリクス（recording.failed / record-broken / ドロップ統計）に加える。レベルトリガーの枠内で安価に実装できる。

実装（`reconciler.detectStartDelays`）:

- 観測の有無は **`recordings.started_at`** で見る（watcher が mirakc の record から書く）。`recordings` 行そのものが無い場合も「観測なし」
- **検出窓は `開始時刻 + 猶予 < now() < 終了時刻`。** 終了時刻を過ぎた予約は `recordNeverScheduled` の領分で、ここで拾い続けると**終わった番組についてアラートが鳴り止まなくなる**。開始遅延は「まだ間に合う可能性がある」時間帯の話である
- 放送イベントキーの照合も `started_at` の時間窓に閉じ、原則は直近 24 時間、24 時間を超える候補があれば開始時刻 − 24 時間まで広げる。`started_at` は予定ちょうどではなく予定 − 15 秒になるため、開始時刻ちょうどは下界にしない
- 猶予（`reconciler.start_delay_grace`、既定 3 分）は開始直後の SSE 到達と watcher 処理の遅れを誤検知しないためのもの。ゼロにすると毎回誤検知する
- `effective.skip` の予約と `orphaned` は対象外（前者は始まらないのが正常、後者は既に「録れなかった」とマークされている）
- **DB に新しい状態を持たせない。** 毎パス再計算できる導出値なので `rokuban_reconcile_start_delayed{site}` ゲージ 1 つで表す（不変条件 5）。`quality_events` には書かない --- それは `recordings` の列で、録画が始まっていない番組には行が無いことがある（§3.2「DELETE 成功 → POST 失敗」と同じ制約）

---
