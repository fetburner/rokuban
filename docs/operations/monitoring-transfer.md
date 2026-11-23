# FS 置き場間の転送の監視

FS 置き場間の転送は、転送待ちの件数、物理コピーの成功時刻、試行が上限に達した件数と失敗の累計で見る。

名前・型・意味は `internal/metrics` の定義（`Name` と `Help`）を引く。

`rokuban_storage_transfer_waiting_assets` と `rokuban_storage_transfer_waiting_bytes` は、転送 reconcile が見た差分を表す。最後の成功が古いまま転送待ちが残るときは、worker とマウントを調べる。

`rokuban_storage_transfer_exhausted_assets` が 0 以外なら、対象の転送元、宛先、ログの失敗理由を確認し、原因を直してから runbook の手順で試行記録を消す。マウントの不一致は、マウントと `.rokuban-store` の値を確認する。

`rokuban_storage_transfer_deferred_failures_total{reason}` は、試行に数えず snooze した転送失敗の累計である。`rate()` で増加を見て、失敗が続いているときは転送元・宛先の置き場と資格情報を確認する。`reason` は数えなかった理由で、取りうる値は計器の Help にある。

`rokuban_storage_transfer_policy_mismatch_total` は、設定変更前の規則の指紋を持つ job を捨てた累計である。通常の規則の切り替えで増えることがある。増加だけで障害とは判定しない。

`rokuban_storage_transfer_corrupt_sources_retired_total` は、転送の照合で壊れていると分かった派生コピーを退役させた累計である。増えたときは Error ログで転送元の置き場とパスを調べ、置き場の障害を疑う。退役の手順と、再生成について確かめた範囲は [置き場間の転送](../storage/transfer.md) にある。

## 置き場ごとの転送待ちのゲージ

転送待ちが上限までどれだけ余っているかと、encode を止めているかを、置き場ごとに見る。3 本とも、encode reconcile が DB に保存した値を scrape のたびに読み直す。ScaledJob 構成でも、どのロールの監視でも同じ値が見える。

見る順は、鮮度、件数、止まっているか、の順である。

鮮度は `rokuban_storage_transfer_backlog_counted_timestamp_seconds{store}` で見る。`time() - この値` が全件パスの周期（15 分）を大きく超えたら、件数と背圧のゲージは古い。encode reconcile が止まっていると、最後の件数を出し続けるためである。

`storage.transfer.backlog_limit` と比べて、上限までの余裕を `rokuban_storage_transfer_backlog_assets{store}` で見る。`rokuban_encode_backpressure{store}` は、同じ件数が上限に達したかを 0 か 1 で出す。止まっているかは、保存した件数と設定の上限から導く。上限を持たない置き場と、まだ数えていない置き場は系列を出さない。

encode reconcile のプロセスは ScaledJob の `--once` で 1 件ごとに終わる。保存した件数は DB に残るので、scrape も次のプロセスも同じ件数を読む。scrape が件数を数え直す集計は走らせない。

## 物理コピーの成功時刻

物理コピーの成功時刻は、コピーが active になった時刻を DB に保存した値である。原本と派生物のどちらの公開でも進み、置き場を問わない 1 本の系列として読む。

系列は `rokuban_storage_transfer_last_success_timestamp_seconds` である。書き込みは公開の確定と同じトランザクションで行うので、確定した公開だけが時刻を進める。転送 reconcile の完走、転送の対象が無い no-op、公開の拒否では進まない。

値の読み方は次の 3 通りに分ける。

- 0: 導入後にまだ 1 件も公開が確定していない。導入後も待ち件数が正のまま 0 なら、一度も公開できていない。
- 系列が出ない: その scrape で DB を読めなかった。`rokuban_sync_marker_scrape_errors_total{source="storage_transfer"}` が増えていれば、読み取りの失敗である。0 とは読まない。
- 時刻が古い: キューが空なら転送は起きないので、時刻は進まない。時刻が古いことは停滞を意味しない。待ち件数が正で時刻が古いときだけ、停滞とする。

待ち件数は、`storage.transfer.backlog_limit` を設定した置き場の `rokuban_storage_transfer_backlog_assets{store}` で読む。これは DB に保存された値で、どの Pod からも読める。`rokuban_storage_transfer_waiting_assets` は転送 reconcile のプロセス内の値なので、reconcile を走らせた Pod でしか読めない。停滞の式は、次のように待ち件数と組む。

```
max by (store) (rokuban_storage_transfer_backlog_assets) > 0
  and on() time() - max(rokuban_storage_transfer_last_success_timestamp_seconds) > 閾値
```

`and on()` は、左辺の `store` ラベルを残したまま、右辺が 1 本だけでも一致させるための書き方である。左辺を `max by (store)` で集約するのは、どの Pod からも同じ系列が出るためである。

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
- 数え始めた時刻を DB の時計から取ること: `TestEncodeReconcile_CountedAtComesFromTheDatabaseClock`
- 古い時刻の件数が新しい件数を上書きしないこと: `TestRecordTransferBacklog_StaleCountDoesNotOverwrite`
- 古い件数が捨てられたとき、残った新しい件数で止めること: `TestEncodeReconcile_StaleCountDecidesByTheSavedCount`
- 待ち件数が止まったら、最後の成功の古さを見てから worker とマウントを調べる: `rokuban_storage_transfer_waiting_assets`
- 試行が上限に達した件数が 0 以外なら、原因を直してから試行記録を消す: `rokuban_storage_transfer_exhausted_assets`
- rate が増えていれば、試行に数えない転送失敗が続いている: `rokuban_storage_transfer_deferred_failures_total{reason}`
- 設定変更で増えることがあり、増加だけでは障害と判定しない: `rokuban_storage_transfer_policy_mismatch_total`
- 増えたら転送元の置き場の障害を疑う: `rokuban_storage_transfer_corrupt_sources_retired_total`
- 件数の鮮度は全件パスの周期と比べて見る: `rokuban_storage_transfer_backlog_counted_timestamp_seconds`
- 上限までの余裕は backlog_limit の設定と比べて見る: `rokuban_storage_transfer_backlog_assets`
- 背圧が 1 のままなら、鮮度、件数、転送の順に調べる: `rokuban_encode_backpressure`
- 0 は未公開、系列が無いのは読み取りの失敗、古い時刻は待ち件数が正のときだけ停滞: `rokuban_storage_transfer_last_success_timestamp_seconds`

</details>
