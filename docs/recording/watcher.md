> [recording.md](../recording.md) §3「Rokuban 側のコンポーネント」の一部。索引から辿る。

### 3.3 watcher（SSE 購読・状態反映）

`/events` SSE を購読し、`recording.record-saved` で `recordingStatus: finished` になったら ingest ジョブを投入する。

#### 3 段構えの信頼性設計

| 段 | 内容 | 形 |
|---|---|---|
| (a) | `record-saved` は同一 record に複数回・順序保証なしで飛ぶ → **record id で冪等投入**（River unique job） | **常駐**（`Watcher.Run` の SSE 購読 + `handleEvent`） |
| (b) | watcher ダウン中の取りこぼし → **SSE の接続時全 record 再送**で回復 | **常駐**（同上。mirakc 側が接続時に再送する挙動そのもの） |
| (c) | SSE はあくまでヒント → **定期的な `GET /api/recording/records` 全量取得と DB の突き合わせ**（レベルトリガー）が真実 | **ジョブ**（`internal/worker.RecordSweepWorker`、`record_sweep`。ロジックは `Watcher.Sweep` を呼ぶだけで移植しない） |

この 3 つでエンコード漏れは構造的に起きない。

**真実（レベルトリガー）がジョブで、ヒント源が常駐**という配置になった。ruler / reconciler が「定期パスが真実、作成/更新イベントはヒント」という形をジョブとして持つのと対称で、watcher の (c) も同じ形にはまる。(a)(b) は SSE という長寿命コネクションでしか実現できないヒント経路なので常駐に残る。

#### record 処理は並行実行しても壊れない

`processRecord` は `record_sync` の `(site, record_id)` 行を**先に確保して行ロックを取ってから** `recordings` を作る。同一 record を 2 つの経路（SSE 由来の (a) と record_sweep ジョブの (c)、あるいは 2 プロセス）が同時に処理しても、2 つ目は 1 つ目のコミットを待ってから `recording_id` が埋まっているのを見る。

これがないと両方が「行なし」を見て両方が `createRecording` し、`recordings_unique_active_event`（`00003` の部分ユニークインデックス）違反で片方が失敗する。既にある PK を使うだけなので、`pg_advisory_xact_lock` のような追加の機構は要らない。

この性質があるので **watcher のシングルトン性は「正しさ」の要件ではない**。残っている理由は「mirakc に N 本の SSE を張らない」という接続数の配慮で、壊れるわけではない（ingest ジョブは record id で冪等）。3 段構えの (c) を record_sweep ジョブとして切り出せたのはこの前提による（(a) と (c) が並行に走っても `recordings` が重複しないことをテストで固定してある）。

#### record_sweep の起動契機

ruler / reconciler と違い、**起動契機は定期のみ**（ヒントで前倒しする経路を持たない）。

| 契機 | 種別 |
|---|---|
| 定期（既定 5 分、旧 watcher の `ReconcileInterval` を継承） | **真実**。デプロイ形態に応じて River `PeriodicJobs` か k8s CronJob（`rokuban enqueue record-sweep`）が投入する（[データ層](../data.md) §2） |

ruler / reconciler は「作成・更新イベント」というヒントを同一トランザクションで投入できたが、record_sweep には対応する自然なヒントがない。**最も自然な候補は SSE の再接続**（切れて再接続した = 取りこぼした可能性がある区間ができた合図）だが、`internal/mirakc.Client.Subscribe` は再接続を内部で処理して自動リトライするだけで、呼び出し側（watcher）に再接続を通知する仕組み（コールバック等）を持たない。追加するなら `mirakc.SSEConfig` に `OnReconnect` のようなフックを生やす設計判断が要るため見送り、定期投入のみとしている。

#### 品質メタデータ記録

`recording.record-broken` / `recording.failed` イベントは構造化された品質シグナルとして record に紐づけて DB に記録する（「録画品質の実測」計画の入力）。

#### 開始遅延検出器

録画開始は mirakc に委譲済みで Rokuban 側から防ぐ手段はないが、EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）のような mirakc 側の未知の不具合への保険として、**「開始時刻を過ぎたのに recording.started が観測されない予約」を reconcile ループで検出してアラート**する。既存の品質メトリクス（recording.failed / record-broken / ドロップ統計）に加える。レベルトリガーの枠内で安価に実装できる。

実装（`reconciler.detectStartDelays`）:

- 観測の有無は **`recordings.started_at`** で見る（watcher が mirakc の record から書く）。`recordings` 行そのものが無い場合も「観測なし」
- **検出窓は `開始時刻 + 猶予 < now() < 終了時刻`。** 終了時刻を過ぎた予約は `recordNeverScheduled` の領分で、ここで拾い続けると**終わった番組についてアラートが鳴り止まなくなる**。開始遅延は「まだ間に合う可能性がある」時間帯の話である
- 猶予（`reconciler.start_delay_grace`、既定 3 分）は開始直後の SSE 到達と watcher 処理の遅れを誤検知しないためのもの。ゼロにすると毎回誤検知する
- `effective.skip` の予約と `orphaned` は対象外（前者は始まらないのが正常、後者は既に「録れなかった」とマークされている）
- **DB に新しい状態を持たせない。** 毎パス再計算できる導出値なので `rokuban_reconcile_start_delayed{site}` ゲージ 1 つで表す（不変条件 5）。`quality_events` には書かない --- それは `recordings` の列で、録画が始まっていない番組には行が無いことがある（§3.2「DELETE 成功 → POST 失敗」と同じ制約）

---

