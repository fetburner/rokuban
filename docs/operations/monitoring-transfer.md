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
| `rokuban_encode_backpressure{store}` | Gauge | 転送待ちが `storage.transfer.backlog_limit` に達し、その置き場の原本を読む encode の投入を止めているとき 1 |
| `rokuban_storage_transfer_backlog_assets{store}` | Gauge | `storage.transfer.backlog_limit` を設定した置き場ごとの転送待ちの論理資産数（転送元の置き場別）。複数の置き場にコピーがある資産は置き場ごとに数える |

転送待ちの 2 つのゲージは、転送 reconcile が見た差分を表す。失敗が上限に達した宛先と、削除してよい until_encoded の原本は数えない。最後の成功が古いまま転送待ちが残るときは、worker とマウントを調べる。

置き場ごとの件数は `rokuban_storage_transfer_backlog_assets{store}` で、上限に対してどこにあるかを見る。件数は encode reconcile が数えたパスでだけ更新される。数えるのは、全件パスの先頭と、候補の原本が上限付きの置き場にあるパスと、止まっている置き場があるパスである。

止まっている間は次のパスで数え直すので、`rokuban_encode_backpressure{store}` は解けたときに次のパスで 0 に戻る。止まっていない置き場の件数は全件パスの周期で更新される。Warn は置き場が止まり始めたときに 1 回、Info は解けたときに 1 回出し、止まっている間は毎パスでは出さない。`rokuban_encode_backpressure{store}` が 1 のまま下がらないなら、件数が減っているかを見て、減らないなら転送元の置き場の帯域の上限と網の状態を見る。

`rokuban_storage_transfer_exhausted_assets` が 0 以外なら、対象の転送元、宛先、ログの失敗理由を確認し、原因を直してから runbook の手順で試行記録を消す。マウントの不一致は、マウントと `.rokuban-store` の値を確認する。

`rokuban_storage_transfer_policy_mismatch_total` は、設定変更前の規則の指紋を持つ job を捨てた累計である。通常の規則の切り替えで増えることがある。増加だけで障害とは判定しない。

`rokuban_storage_transfer_corrupt_sources_retired_total` は、転送の照合で壊れていると分かった派生コピーを退役させた累計である。増えたときは Error ログで転送元の置き場とパスを調べ、置き場の障害を疑う。退役の手順と、再生成について確かめた範囲は [FS 置き場間の転送](../storage/transfer.md) にある。
