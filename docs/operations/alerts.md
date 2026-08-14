> [operations.md](../operations.md) §2「アラート設計」の一部。索引から辿る。

## 2. アラート設計

### scrambled > 0（B-CAS / 復号障害）

復号が正常なら `scrambling_control` は常にゼロのはず。**scrambled > 0 は放送品質ではなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味するので、ドロップ数とは別枠のアラート対象とする。EPGStation ドロップログの scramble 列と同じ役割。

### エッジディスク残量（未 ingest 滞留）

未 ingest record 総量メトリクスとエッジディスク残量を突き合わせてアラートする。ingest が詰まって未 ingest の record が溜まり続けるシナリオへの備え（[ストレージ](../storage.md) のサイジング指針と対）。

**このアラートは回線断を検知できない。** 未 ingest 総量が数えるのは `record_sync` に `status='finished'` として観測済みの record だけで、その行は watcher が mirakc を観測して初めて作られる。**エッジ↔クラウドの回線が切れている間に始まった録画はここに現れないので、断のあいだこの値は平らなまま**（録画中に断が始まったぶんも、`finished` への更新が復帰後になるので同じ）。回線断は `rokuban_sweep_last_pass_timestamp_seconds` / `rokuban_epg_sync_last_success_timestamp_seconds` の**停滞**で別に見張る —— 断が `epg.retention_grace` を超えると encode 意図が落ちる（[ストレージ運用](storage.md)「N 日は容量だけでは決まらない」）。

### 大量削除サーキットブレーカー発動

EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう（EPGStation#692 の障害クラス）。

- 1 回の ruler パスでの削除数に閾値（`ruler.max_deletes_per_pass`）を設け、超えたら削除を実行せず停止してアラート
- 削除エンジンの物理 unlink についても、ソースを問わず 1 パスの物理削除が閾値（件数 / ライブラリ比率 / 総バイト数、例: 5% or 100 GB）を超えたら停止してアラート

**アラートは `rokuban_circuit_breaker_tripped{site,breaker}` を見る**。既存の
`*_circuit_breaker_trips_total` は「何回発動したか」しか答えられないが、ブレーカーは
**手動で再開するまで止まり続けるラッチ**なので「いま止まっているか」が知りたい情報である。

| ブレーカー | 何を守るか | 発動したら |
|---|---|---|
| `ruler_deletes` | ルール x EPG の評価から導出された予約削除 | `GET /api/breakers` の `detail` で消されようとしていた番組を確認 → 正当なら `POST /api/sites/{site}/breakers/ruler_deletes/resume`（`site` は一覧のレスポンスにある値） |
| `reconcile_total_loss` | 「desired が空なのに自分の schedule が観測されている」という全損シグネチャ | DB 接続・`reservations` の中身を確認。**件数の閾値ではない**ので、発動したら本当に異常である |
| `delete_reconcile` | 削除 reconcile（ごみ箱の猶予超過 / `until_encoded` の派生物完備 / 孤児回収の 3 ソースをまとめた 1 パス分の物理 unlink） | **`ruler_deletes` と異なり `detail` に対象の抜粋は載らない**（`internal/worker/delete_reconcile.go` は `breaker.Sample{Total: total}` しか渡さず、`breaker.Sample`/`SampleProgram` にファイルを表す欄も無い。未検証で「ファイルを確認」とは書けない）。`GET /api/breakers` の `pending`/`threshold` で規模を確認し、対象の内訳が要るなら DB を直接クエリする — **ごみ箱・`until_encoded` 待ちの 2 ソースは `media_assets`、孤児回収の候補は `orphan_files`（`rel_path` と `first_seen`。`first_seen` はエイジング判定の根拠なので内訳確認にも使える。孤児は定義上 `media_assets` に無いファイルなので `media_assets` では引けない）**（3 ソースの判定条件は [storage/retention.md](../storage/retention.md)）→ 正当なら `POST /api/sites/{site}/breakers/delete_reconcile/resume`（`site` は一覧のレスポンスにある値） |

**1 が続く間、導出削除は一切実行されない。** これは「reconcile が収束できていない」ではなく
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

### 経緯と失敗事例

- サーキットブレーカーのラッチ化と `rokuban_circuit_breaker_tripped` ゲージは M2-5、開始遅延検出器（`rokuban_reconcile_start_delayed`）は M2-7。
