> [recording.md](../recording.md) §3.2「reconciler」の一部（大量削除サーキットブレーカー）。索引から辿る。

#### 大量削除サーキットブレーカー

**大量削除ブレーカーは、ruler の 1 パスあたりの削除数に `ruler.max_deletes_per_pass` の上限を設ける。** 超過時は削除を止めてアラートし、手動確認後に再開する。

EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）があると、素朴な ruler は予約を大量に「不要」と判定する。reconciler はそれを mirakc へ忠実に反映して一斉 DELETE する。EPGStation#692（予約と録画が勝手に消える）はこの障害クラスの実例。

対策:

- **対象は、ルールが base を供給していた予約が desired から外れた削除である**。desired は「(ルール勝者 − intent skip) ∪ investment（record 意図 ∪ overrides）」から決まる。
- `toDelete` には EPG の unmatch とユーザー操作の両方が現れる。ユーザーが投資を手放さないと起きない削除は対象外とし、ラッチ中でも実行する。
- 対象外は `NOT EXISTS program_investments` に加えて、`reservations.rule_id IS NULL` または `program_intents.action='skip'` を満たす場合である。
- **`rule_id IS NULL` だけではユーザー操作の証明にならない**。EPG の変化だけでも NULL になる。投資のある予約は desired に残り、`base` と重複根拠 2 列は凍結されるが、`rule_id` は NULL に更新される。
- intent skip はユーザーが「録るな」と明示した状態である。intent のクリアと最後の investment だった overrides の削除は、前項の対象外に当たる。
- ルール編集で勝者が変わった予約は `rule_id` が残るため対象のままにする。ルール削除は `toDelete` を通らない。
- `rule_id` が NULL になる経路でも、ルール削除は投資なしの予約を先に消す。そのため生き残る予約は投資を持っていた行であり、投資を消す別の書き込みが必要になる。
- 投資を消す per-program API は `{programId}` 単位で、バルク操作はない。catalog rescue は運用者が明示実行する。番組 snapshot の GC は予約行も同時に消す。
- EPG 同期・ruler・reconciler は intent と override を書かない。したがって EPG 欠損だけでブレーカー外の削除が増えることはない。
- **intent クリアをラッチ中に止めると、予約と録画が残り続け、ディスクとチューナーを消費する**。クリアは「意見なし」であり、`effective.skip` を立てない。
- intent skip は予約一覧に行を残す場合があるが、`effective.skip` により録画は防がれる。明示操作を同じ側に置き、skip だけに例外を設けない。
- 境界 (a): EPG unmatch と明示操作が重なり、`rule_id` が残る場合は区別できず、ブレーカー対象のままである。
- 境界 (b): 削除候補には EPG 射影に番組が残っている必要がある。射影から消えた番組は別の防御が凍結し、復旧後に次のパスで解ける。
- 境界 (c): EPG 欠損中は投資のある予約も `rule_id` が NULL になる。その間に投資を消すと予約はブレーカー外で削除されるが、EPG 復旧後に ruler が再作成する。
- 録画済みデータ（`media_assets`）へ至る自動削除は retention reconcile だけにする。EPG や予約の変化から録画物を消す経路は作らない。
- `programId` が EPG から消えた予約は猶予を置いて削除する。`orphaned` は番組終了後に schedule が観測されなかった欠測を表し、この用途には使わない。
- 放送開始直前の unmatch は `ruler.retract_grace` が先に受け止める。猶予で保留した予約はブレーカーの分子にも分母にも入れない。

<details><summary>根拠</summary>

- EPG unmatch と投資中の削除防止: `TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease`
- EPG unmatch 時の base と重複根拠 2 列の凍結: `internal/ruler/sql.go` の `resolved` CTE
- ラッチ中の intent クリア: `TestRunPass_LatchDoesNotWithholdIntentClearDelete`
- intent クリアを件数に含めないこと: `TestRunPass_IntentClearDeletesDoNotCountTowardBreaker`
- ラッチ中の intent skip: `TestRunPass_LatchDoesNotWithholdSkipIntentDelete`
- skip 予約を mirakc に作らないこと: `TestReconciler_SkippedReservationNotScheduled`
- 明示操作の分類: `internal/db/queries/ruler.sql` の `DeleteReleasedReservationsBySiteAndProgramIDs`
- per-program の操作: `DELETE .../intent`、`DELETE .../overrides`、`PUT .../intent{skip}`
- API のパス: `openapi.yaml`
- skip 判定: `intent.action='skip'`
- intent clear 中に reservation が desired に残ること: `reservation.EvaluateSyncCandidates` の `Skipped`
- EPG unmatch による NULL 書き込み: `rule_id = EXCLUDED.rule_id`
- catalog rescue: `internal/catalog/rescue.go`

</details>

##### 止められる場所は ruler だけ

削除件数の閾値を持つのは ruler 側だけで、**reconciler 側には置かない**（reconciler 側に置いても誤発火するだけで、守れるものも無い。理由は下記のとおり）。reconciler が「消すべき schedule」と判断する経路は、desired（reservations）を減らす操作の数だけある。だが reconciler からは、どれも「desired に無い schedule がある」以上には区別できない。ruler のブレーカーの対象かどうかで束ねると次の 5 通りに分かれる:

| 経路 | ruler の `MaxDeletesPerPass` の対象か |
|---|---|
| ruler が EPG の変化から導出削除した | 対象。ruler のブレーカーが既に通している |
| ルール**編集**（無効化・条件変更）で勝者が変わった | 対象。`rule_id` が残るので EPG 由来の unmatch と区別できない |
| ユーザーの明示操作で desired から外れた（intent skip、intent クリア、最後の investment だった overrides の削除） | **対象外**。削除文の `WHERE` が適用の瞬間に明示操作と判定し、カウントにも入れずラッチ中でも実行する（上記「大量削除サーキットブレーカー」） |
| ユーザーの明示操作のうちルール**削除**（`DeleteRule`） | **対象外**。API ハンドラ（`internal/api/rules.go`）が同一トランザクションで `reservations` を直接 DELETE し、ruler の `toDelete` も `MaxDeletesPerPass` も経由しない |
| 番組終了後の GC が予約行を刈った | **対象外**。GC は `runGC` という別経路で `runPassForSite` の `toDelete` を通らない（[ruler.md](ruler.md)「番組終了後の GC」と下記「GC は対象にしない」） |

ruler のブレーカーが通しているのは EPG 由来の導出削除（ルール編集で勝者が変わる経路を含む）だけで、**明示操作・ルール削除・GC はブレーカーの外にある**。reconciler から見ると、どの経路も「ruler あるいは API ハンドラが既に処理して DB にコミット済み」の状態でしか観測されない。reconciler にもう一段ブレーカーを置いても、desired に無い schedule があるという観測だけではこの区別ができない。その結果、ユーザーが取消した予約・ルール削除の一括処理（内訳を提示する確認 UI で安全性を担保済み）に誤発火する。GC の正常な一括削除（「長時間停止していた場合、再開後に溜まった期限切れ行を一括で消す」）にも誤発火するだけだった。

守る価値もない。**reconciler が DELETE する時点で「録画しない」決定は DB にコミット済み**である（不変条件「コミット = DB 行」）。誤りなら止めるべき場所は ruler で、reconciler で止めるのは「DB に合わせることを拒否する」ことにしかならず、mirakc に不要な schedule を残し続ける。

**ただし全損だけは別のシグネチャで守る。** 件数の閾値を外すと `listDesired` がバグや障害で空を返したときに自分が作った全 schedule を削除する経路が無防備になる。これは件数ではなく形で捕まえる:

```
desired が空 かつ 自分の tag が付いた schedule が 1 つ以上観測されている → 削除せず発動
```

GC・ユーザー操作では他の予約が残るので誤発火しない。全損だけを捕まえる（`breaker.ReconcileTotalLoss`）。

##### 発動はラッチ

「手動確認後に再開」には**人間が確認するまで止まり続けるラッチ**が必要で、それはプロセスをまたぐ永続状態である（`circuit_breakers` 表。[schema.md](../schema.md) §3.6）。**レベルトリガー設計の中で数少ない導出できない状態** — 誰かが確認したという事実は再取得できない。

- **行の存在そのものが「発動中」**。停止していない状態を表す行は無い。再開は行の DELETE
- 件数が閾値以下に戻っても**自動では解けない**。EPG が回復して候補がゼロになれば実害はないが、自動復帰させると「一瞬止まって復帰した」がアラートに残らず、EPG が繰り返し欠損する状況を見逃す
- **止めるのは削除だけ。** 発動中でも予約の作成・base の更新・schedule の作成は続く（レベルトリガーで収束させたい他の差分は止めない）
- **GC は発動中でも動く**（下記「GC は対象にしない」の理由がそのまま効く）
- `detail` に「何が消されようとしていたか」の抜粋（最大 20 件の programId と題名）を焼く。**手動確認には対象が見える必要がある**
- 再開は `POST /api/sites/{site}/breakers/{name}/resume`（資源の PK が `(site, name)` であることに合わせる）。`DELETE /api/sites/{site}/breakers/{name}` にしないのは、運用者から見た操作が「行を削除する」ではなく「確認したので再開する」だから（行が消えるのは実装詳細）
- **site を持たないブレーカー（`delete_reconcile`。`internal/breaker.IsSiteless`）だけは `POST /api/breakers/{name}/resume` で再開する**。理由は `internal/worker/delete_reconcile.go` の `DeleteReconcileWorker` doc コメント参照
- **ブレーカー名を足すときは `internal/breaker.All` と `openapi.yaml` の enum の両方に足す。** resume は `All` で検証するので、`All` を落とすと 400 になる。const↔`All` は `all_test.go`、`All`→enum は `breakers_test.go` が検出する（enum 側だけの余分はどちらも検出しない）

##### GC は対象にしない

**番組終了後の GC（[ruler.md](ruler.md)「番組終了後の GC」）は `MaxDeletesPerPass` の対象にしない**。ブレーカーが守るのは「ルール x EPG」の評価結果から導出される削除だけである。EPG の一時的な欠損・フリッカーに引きずられて予約を大量に消してしまう事故（上記 EPGStation#692 のクラス）を防ぐためのもの。GC の削除対象は時刻の比較だけで決定的に定まり、EPG の状態には一切左右されない。むしろ reconciler/ruler が長時間停止していた場合、再開後に溜まった期限切れ行を一括で消すのは正常な挙動であり、ここをブレーカーで止めると実害のない削除が積み上がり続けるだけになる。

