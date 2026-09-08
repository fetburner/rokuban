> [operations.md](../operations.md) §2「アラート設計」の一部。索引から辿る。

## 2. アラート設計

### scrambled > 0（B-CAS / 復号障害）

復号が正常なら `scrambling_control` は常にゼロのはず。**scrambled > 0 は放送品質ではなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味するので、ドロップ数とは別枠のアラート対象とする。EPGStation ドロップログの scramble 列と同じ役割。

### エッジディスク残量（未 ingest 滞留）

未 ingest record 総量メトリクスとエッジディスク残量を突き合わせてアラートする。ingest が詰まって未 ingest の record が溜まり続けるシナリオへの備え（[ストレージ](../storage.md) のサイジング指針と対）。

**このアラートは回線断を検知できない**。未 ingest 総量が数えるのは `record_sync` に `status='finished'` として観測済みの record だけである。その行は watcher が mirakc を観測して初めて作られる。**エッジ↔クラウドの回線が切れている間に始まった録画はここに現れないので、断のあいだこの値は平らなまま**（録画中に断が始まったぶんも、`finished` への更新が復帰後になるので同じ）。回線断は `rokuban_sweep_last_pass_timestamp_seconds` / `rokuban_epg_sync_last_success_timestamp_seconds` の**停滞**で別に見張る。断が `epg.retention_grace` を超えると encode 意図が落ちる（[ストレージ運用](storage.md)「N 日は容量だけでは決まらない」）。

### 大量削除サーキットブレーカー発動

EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定する。reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう（EPGStation#692 の障害クラス）。

- 1 回の ruler パスでの削除数に閾値（`ruler.max_deletes_per_pass`）を設け、超えたら削除せず停止してアラート
- 削除エンジンの物理 unlink についても、ソースを問わず 1 パスの物理削除が閾値（件数 / ライブラリ比率 / 総バイト数、例: 5% or 100 GB）を超えたら停止してアラート

**アラートは `rokuban_circuit_breaker_tripped{site,breaker}` を見る**。既存の
`*_circuit_breaker_trips_total` は「何回発動したか」しか答えられないが、ブレーカーは
**手動で再開するまで止まり続けるラッチ**なので「いま止まっているか」が知りたい情報である。

| ブレーカー | 何を守るか | 発動したら |
|---|---|---|
| `ruler_deletes` | ルール x EPG の評価から導出された予約削除 | `GET /api/breakers` の `detail` で消されようとしていた番組を確認 → 正当なら `POST /api/sites/{site}/breakers/ruler_deletes/resume`（`site` は一覧のレスポンスにある値） |
| `reconcile_total_loss` | 「desired が空なのに自分の schedule が観測されている」という全損シグネチャ | DB 接続・`reservations` の中身を確認。**件数の閾値ではない**ので、発動したら本当に異常である |
| `delete_reconcile` | 削除 reconcile（ごみ箱の猶予超過 / `until_encoded` の派生物完備 / 孤児回収の 3 ソースをまとめた 1 パス分の物理 unlink） | **`ruler_deletes` と異なり `detail` に対象の抜粋は載らない**（`internal/worker/delete_reconcile.go` は `breaker.Sample{Total: total}` しか渡さず、`breaker.Sample`/`SampleProgram` にファイルを表す欄も無い。未検証で「ファイルを確認」とは書けない）。`GET /api/breakers` の `pending`/`threshold` で規模を確認し、対象の内訳が要るなら DB を直接クエリする — **ごみ箱・`until_encoded` 待ちの 2 ソースは `media_assets`、孤児回収の候補は `orphan_files`（`rel_path` と `first_seen`。`first_seen` はエイジング判定の根拠なので内訳確認にも使える。孤児は定義上 `media_assets` に無いファイルなので `media_assets` では引けない）**（3 ソースの判定条件は [storage/retention.md](../storage/retention.md)）→ 正当なら `POST /api/breakers/delete_reconcile/resume`（site を持たないブレーカーなので site をパスに含めない） |

**1 が続く間、導出削除は一切実行されない**。これは「reconcile が収束できていない」ではなく
「人間の確認を待っている」を意味する。放置すると mirakc 側に不要な schedule が残り続けるため、
`for` の待ち時間を長く取らず即座に通知する。

発動中でも**削除以外は動く**（予約の作成・base の更新・schedule の作成・番組終了後の GC）。
「録画されない」ではなく「消えないものが残る」障害なので、慌てて resume せず `detail` を
確認してからにする。

### 開始時刻超過で recording.started 未観測

`rokuban_reconcile_start_delayed > 0` でアラートする。**`for` を長く取らないこと** ---
検出窓が「開始 + 猶予 〜 終了時刻」に限定されているので、番組が終わればゲージは自然にゼロへ戻る。
待ち時間を番組長より長くすると、短い番組の遅延を一度も通知しないまま取りこぼす。

`rokuban_recordings_failed_total{reason}` が同時に増えているなら mirakc が理由を返しているので
そちらが一次情報になる。**増えていないのにこのゲージが立つのが最も危険**な状態で、mirakc が
失敗を報告せずに録画を始めていないことを意味する（EPGStation#724 のクラス）。

### 開始前の未同期または観測不能

予約の受付が DB に成功したことは、mirakc に実効 schedule が反映されたことを意味しない。
次の主系列をサイトごとに使う。

- `rokuban_presync_pending{site,reason="missing"}` — desired に対する observed schedule が無い
- `rokuban_presync_pending{site,reason="options"}` — `scheduled` state の observed schedule はあるが、
  priority / `program:{programId}` tag / 明示 `contentPath` が desired と一致せず、reconciler が
  再作成を試みられる
- `rokuban_presync_pending{site,reason="options_deferred"}` — options の不一致はあるが、state が
  `scheduled` ではないため再作成を見送っている。放送終了を待つか、必要なら手動介入する
- `rokuban_presync_pending_earliest_start_timestamp_seconds{site,reason}` — 同じ reason で
  pending な予約のうち最も開始が近い番組の start_at。pending が 0 の reason には
  この系列が出ない（0 を出すと `earliest - time() < lead` が常に真になり、健全な状態で鳴る）
- `rokuban_schedule_snapshot_last_success_timestamp_seconds{site}` — schedule 全量 snapshot が DB に
  整合した形で最後に確定した時刻。0 は未確立

判定は **観測不能 → 未同期（missing） → 未同期（options） → 未同期（options_deferred） → 同期済み**
の順に読む。`options` は reconciler / mirakc / DB を確認して開始前に直す対象、
`options_deferred` は現在の schedule state では原理的に再作成できない対象で、担当者のアクションが異なる。
snapshot が 0 または stale なら、pending の値が 0 でも「同期済み」とは扱わない。collector
の DB 読み取りに失敗したときは pending / snapshot 自体が出ず、
`rokuban_presync_scrape_errors_total{site}` が増えるので、0 への置換でアラートを消さない。

件数の gauge だけでは、8 日先の予約 1 件と 2 分後開始の予約 1 件が同値になり
区別できない（issue #680）。開始が近いかどうかは件数ではなく
`rokuban_presync_pending_earliest_start_timestamp_seconds` で判定する。

アラート式の閾値は、現時点ではリポジトリに固定しない。Prometheus 側で運用値を設定する
（`<snapshot_stale_seconds>` と `<lead_seconds>` は環境ごとの recording rule / alert rule
の値に置き換える）。論理形は次のとおり。

```promql
# 先に観測不能を通知する。
time() - rokuban_schedule_snapshot_last_success_timestamp_seconds
  > <snapshot_stale_seconds>

# fresh な snapshot に対してだけ、開始が近い「直せる未同期」を通知する。
# 十分先の予約（earliest - time() >= <lead_seconds>）は鳴らさない。
(
  rokuban_presync_pending_earliest_start_timestamp_seconds{reason=~"missing|options"}
    - time() < <lead_seconds>
)
and on (site)
(
  time() - rokuban_schedule_snapshot_last_success_timestamp_seconds
    <= <snapshot_stale_seconds>
)

# options_deferred は別の Alertmanager ルートへ送る。
# 放送中は earliest が過去なので継続して成立するが、直ちに再作成せず、
# 放送終了待ちまたは手動介入が必要な状態である。
(
  rokuban_presync_pending_earliest_start_timestamp_seconds{reason="options_deferred"}
    - time() < <lead_seconds>
)
and on (site)
(
  time() - rokuban_schedule_snapshot_last_success_timestamp_seconds
    <= <snapshot_stale_seconds>
)
```

`for:` はここでは「pending が続いた時間」ではなく 1 scrape 分の揺らぎ吸収だけに
使う。開始までの残り時間は上式が `earliest - time()` で直接見ているので、
`for` を長く取る必要はない。

通知は Prometheus の alert rule から Alertmanager へ送り、`site` と `reason` を
ルーティングに残す。`missing` / `options` は開始前に reconciler・mirakc・DB を
確認する通常の未同期ルート、`options_deferred` は放送終了待ちまたは手動介入を案内する
別ルートにする。観測不能は同期状態の断定より優先して、担当者が reconciler の投入元・
worker / ScaledJob の起動状態・DB 接続を確認する入口にする。

`<lead_seconds>` を決める式は次のとおり（p95/p99 の実測値を代入する）。

```text
lead >= (
  定期投入間隔 + KEDA poll + Pod 起動 + reconcile パス時間
  + 作成した schedule の再観測 + 通知到達
) の p95/p99 + 運用者の介入余裕
```

将来「開始までに同期できた割合」を測る場合の母集団は、サイトと放送イベントの
`(site, program_id)` 単位にする。測定 lead より前に desired が確定し、明示 skip ではなく、
開始前に options 一致を確認できた候補を分母にする。直前に作られた手動予約、開始後の
操作、EPG の開始時刻変更で旧イベントと新イベントを跨いだ候補は別の母集団に分ける。
skip は分母から除外する。現スコープではこの履歴を保存せず、collector は現在の desired /
observed と snapshot 鮮度だけを出す。

常駐 `PeriodicJobs` では reconciler が full snapshot を確定し、常駐プロセスの `/metrics`
が DB の marker を読む。ScaledJob `--once` ではジョブ Pod 自身の process gauge を
scrape しようとせず、常駐 Pod の collector が同じ marker を読む。ジョブ未投入・Pod
未起動・パス失敗では marker が進まない。復旧して full snapshot がコミットされ、desired
と observed の差分が解消すれば、marker は新しくなり pending は 0 に戻る。予約の削除・
再実体化を跨いでも、collector はその時点の `(site, program_id)` と DB の desired /
observed を再計算するため、`synced` 成功状態を永続化して古い成功を残さない。

未測定の範囲は、定期投入から通知到達までの p95/p99、mirakc の POST 後に schedule が
再観測されるまでの遅延、サイトごとの EPG 時刻変更頻度である。実測後に alert rule の
閾値を変更し、アプリケーションを再ビルドしない。

### 経緯と失敗事例

- サーキットブレーカーのラッチ化と `rokuban_circuit_breaker_tripped` ゲージは M2-5、開始遅延検出器（`rokuban_reconcile_start_delayed`）は M2-7。
