# FS 置き場間の転送の監視

FS 置き場間の転送の進み具合と、失敗の止まり方を、次のメトリクスで見る。

| メトリクス | 型 | 意味 |
|---|---|---|
| `rokuban_storage_transfer_waiting_assets` | Gauge | desired の置き場へのコピーが必要な論理資産数 |
| `rokuban_storage_transfer_waiting_bytes` | Gauge | 転送待ちの論理資産の合計バイト数 |
| `rokuban_storage_transfer_exhausted_assets` | Gauge | 失敗が上限に達し、自動再投入を止めている論理資産数 |
| `rokuban_storage_transfer_last_success_timestamp_seconds` | Gauge | 最後に成功した物理コピーの転送の時刻 |
| `rokuban_storage_transfer_policy_mismatch_total` | Counter | 設定変更後に、古い規則の指紋を持つため破棄した job 数 |
| `rokuban_storage_transfer_corrupt_sources_retired_total` | Counter | 壊れた派生コピーを、転送 worker が転送元から退役させた回数 |
| `rokuban_storage_transfer_mount_mismatch_total{store}` | Counter | root が未設定、または目印が一致せず転送の開始を見送った回数 |
| `rokuban_encode_backpressure{store}` | Gauge（DB） | 直近に数えた転送待ちが `storage.transfer.backlog_limit` に達し、その置き場の原本を読む encode の投入を止めているとき 1 |
| `rokuban_storage_transfer_backlog_assets{store}` | Gauge（DB） | `storage.transfer.backlog_limit` を設定した転送元の置き場ごとに、直近に数えた転送待ちの論理資産数。複数の置き場にコピーがある資産は置き場ごとに数える |
| `rokuban_transfer_backlog_scrape_errors_total` | Counter（DB） | 保存した件数を scrape で読めなかった回数 |

`rokuban_storage_transfer_waiting_assets` と `rokuban_storage_transfer_waiting_bytes` は、転送 reconcile が見た差分を表す。失敗が上限に達した宛先と、削除してよい until_encoded の原本は数えない。最後の成功が古いまま転送待ちが残るときは、worker とマウントを調べる。

## 置き場ごとの転送待ちのゲージ

転送待ちが上限までどれだけ余っているかと、encode を止めているかを、置き場ごとに見る。2 本とも、encode reconcile が DB に保存した件数を scrape のたびに読み直す。ScaledJob 構成でも、どのロールの監視でも同じ値が見える。

`storage.transfer.backlog_limit` と比べて、上限までの余裕を `rokuban_storage_transfer_backlog_assets{store}` で見る。`rokuban_encode_backpressure{store}` は、同じ件数が上限に達したかを 0 か 1 で出す。止まっているかは、保存した件数と設定の上限から導く。上限を持たない置き場と、まだ数えていない置き場は系列を出さない。

encode reconcile のプロセスは ScaledJob の `--once` で 1 件ごとに終わる。保存した件数は DB に残るので、scrape も次のプロセスも同じ件数を読む。scrape が件数を数え直す集計は走らせない。

## 件数を数えるパス

件数を数えるのは、ゲージの更新と encode を止めるかの判断が要るパスと、止まっている置き場があるパスである。どちらでもないパスは数えず、ゲージも更新しない。

前者は、全件パスの先頭と、候補の原本が上限付きの置き場にあるパスである。後者は、直近の件数が上限に達している置き場があるパスで、解けたことを次のパスで 0 に戻すために数え直す。止まっているかは、DB に保存した直近の件数と今の上限から決める。

## ログ

Warn は置き場が止まり始めたときに 1 回、Info は解けたときに 1 回出す。止まっている間は毎パスでは出さない。前の状態は DB の件数から読むので、プロセスが毎回新しくなっても変わらない。並行する複数のパスでも Warn は 1 回である。

## 対処

止まりが続くときは、件数が減っているかで原因を切り分ける。件数が減らないなら、転送元の置き場の帯域の上限と網の状態を見る。

ゲージ `rokuban_encode_backpressure{store}` が 1 のまま下がらず、件数も減らないときが対象である。

<details>
<summary>根拠</summary>

- ScaledJob 構成でも保存した件数からゲージが出ること: `TestTransferBacklogCollector_ReadsSavedCounts`
- 数えないパスと数えるパスの区別: `TestEncodeReconcile_PassWithoutDecisionSkipsBacklogCount`
- 解けた次のパスでゲージが 0 に戻ること: `TestEncodeReconcile_BacklogGaugeClearsOnTargetedPass`
- パスごとにプロセスが新しくなっても Warn / Info が変化のときだけ出ること: `TestEncodeReconcile_BacklogLogsOnlyOnTransitions`
- 並行するパスで Warn が 1 回であること: `TestEncodeReconcile_BacklogWarnOnceAcrossConcurrentPasses`
- 並行する保存で前の行が無いと見るのが 1 つだけであること: `TestRecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce`

</details>

`rokuban_storage_transfer_exhausted_assets` が 0 以外なら、対象の転送元、宛先、ログの失敗理由を確認し、原因を直してから runbook の手順で試行記録を消す。マウントの不一致は、マウントと `.rokuban-store` の値を確認する。

`rokuban_storage_transfer_policy_mismatch_total` は、設定変更前の規則の指紋を持つ job を捨てた累計である。通常の規則の切り替えで増えることがある。増加だけで障害とは判定しない。

`rokuban_storage_transfer_corrupt_sources_retired_total` は、転送の照合で壊れていると分かった派生コピーを退役させた累計である。増えたときは Error ログで転送元の置き場とパスを調べ、置き場の障害を疑う。退役の手順と、再生成について確かめた範囲は [FS 置き場間の転送](../storage/transfer.md) にある。
