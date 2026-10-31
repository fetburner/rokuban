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
| `rokuban_storage_transfer_backlog_counted_timestamp_seconds{store}` | Gauge（DB） | 上の件数の、数え始めた時刻。件数の鮮度 |
| `rokuban_storage_transfer_backlog_assets{store}` | Gauge（DB） | `storage.transfer.backlog_limit` を設定した転送元の置き場ごとに、直近に数えた転送待ちの論理資産数。複数の置き場にコピーがある資産は置き場ごとに数える |
| `rokuban_transfer_backlog_scrape_errors_total` | Counter（DB） | 保存した件数を scrape で読めなかった回数 |

`rokuban_storage_transfer_waiting_assets` と `rokuban_storage_transfer_waiting_bytes` は、転送 reconcile が見た差分を表す。失敗が上限に達した宛先と、削除してよい until_encoded の原本は数えない。最後の成功が古いまま転送待ちが残るときは、worker とマウントを調べる。

`rokuban_storage_transfer_exhausted_assets` が 0 以外なら、対象の転送元、宛先、ログの失敗理由を確認し、原因を直してから runbook の手順で試行記録を消す。マウントの不一致は、マウントと `.rokuban-store` の値を確認する。

`rokuban_storage_transfer_policy_mismatch_total` は、設定変更前の規則の指紋を持つ job を捨てた累計である。通常の規則の切り替えで増えることがある。増加だけで障害とは判定しない。

`rokuban_storage_transfer_corrupt_sources_retired_total` は、転送の照合で壊れていると分かった派生コピーを退役させた累計である。増えたときは Error ログで転送元の置き場とパスを調べ、置き場の障害を疑う。退役の手順と、再生成について確かめた範囲は [FS 置き場間の転送](../storage/transfer.md) にある。

## 置き場ごとの転送待ちのゲージ

転送待ちが上限までどれだけ余っているかと、encode を止めているかを、置き場ごとに見る。3 本とも、encode reconcile が DB に保存した値を scrape のたびに読み直す。ScaledJob 構成でも、どのロールの監視でも同じ値が見える。

見る順は、鮮度、件数、止まっているか、の順である。

`rokuban_storage_transfer_backlog_counted_timestamp_seconds{store}` が数え始めた時刻である。`time() - この値` が全件パスの周期（15 分）を大きく超えたら、件数と背圧のゲージは古い。encode reconcile が止まっていると、最後の件数を出し続けるためである。

`storage.transfer.backlog_limit` と比べて、上限までの余裕を `rokuban_storage_transfer_backlog_assets{store}` で見る。`rokuban_encode_backpressure{store}` は、同じ件数が上限に達したかを 0 か 1 で出す。止まっているかは、保存した件数と設定の上限から導く。上限を持たない置き場と、まだ数えていない置き場は系列を出さない。

encode reconcile のプロセスは ScaledJob の `--once` で 1 件ごとに終わる。保存した件数は DB に残るので、scrape も次のプロセスも同じ件数を読む。scrape が件数を数え直す集計は走らせない。

## 件数を数えるパス

件数を数えるのは、次の 3 つのパスである。どれでもないパスは数えず、ゲージも更新しない。

- 全件パスの先頭。ゲージの更新に使う。
- 候補の原本が上限付きの置き場にあるパス。encode を止めるかの判断に使う。
- 直近の件数が上限に達している置き場があるパス。解けたことを次のパスで 0 に戻すために数え直す。

上限を設定していない構成では、全件パスの先頭だけが、残った行を消すために DB に触れる。それ以外のパスは DB に触れない。

## ログ

Warn は置き場が止まり始めたときに 1 回、Info は解けたときに 1 回出す。止まっている間は毎パスでは出さない。前の状態は DB の件数から読むので、プロセスが毎回新しくなっても変わらない。

並行する複数のパスは、前の件数の読みと保存を直列にするので、変化を見るのは 1 つだけである。より古い時刻に数えた件数は保存せず、その置き場の Warn / Info も出さない。encode を止める判断は、DB に残った新しい件数で行うので、ゲージと食い違わない。

## 対処

止まりが続くときは、鮮度、件数、転送の順に調べる。

`rokuban_encode_backpressure{store}` が 1 のままのときは、まず鮮度を見る。鮮度のゲージは `rokuban_storage_transfer_backlog_counted_timestamp_seconds{store}` である。古ければ、encode reconcile のジョブが動いているかを調べる。新しければ、`rokuban_storage_transfer_backlog_assets{store}` が減っているかを見る。減っていなければ、転送元の置き場の帯域の上限と網の状態を調べる。

<details>
<summary>根拠</summary>

- ScaledJob 構成でも保存した件数からゲージが出ること、鮮度の値: `TestTransferBacklogCollector_ReadsSavedCounts`
- 数えないパスと数えるパスの区別: `TestEncodeReconcile_PassWithoutDecisionSkipsBacklogCount`
- 上限の無い構成の targeted pass が DB に触らないこと: `TestEncodeReconcile_NoLimitPassDoesNotTouchBacklogState`
- 解けた次のパスでゲージが 0 に戻ること: `TestEncodeReconcile_BacklogGaugeClearsOnTargetedPass`
- パスごとにプロセスが新しくなっても Warn / Info が変化のときだけ出ること: `TestEncodeReconcile_BacklogLogsOnlyOnTransitions`
- 並行する保存で変化を見るのが 1 つだけであること: `TestRecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce`
- 古い時刻の件数が新しい件数を上書きしないこと: `TestRecordTransferBacklog_StaleCountDoesNotOverwrite`
- 古い件数が捨てられたとき、残った新しい件数で止めること: `TestEncodeReconcile_StaleCountDecidesByTheSavedCount`

</details>
