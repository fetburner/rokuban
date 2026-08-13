> [recording.md](../recording.md) §3.2「reconciler」の一部（大量削除サーキットブレーカー）。索引から辿る。

#### 大量削除サーキットブレーカー

予約は「ルール x EPG」から導出されるため、EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう。EPGStation#692（予約と録画が勝手に消える）はこの障害クラスの実例。

対策:

- **1 回の ruler パスでの削除数に閾値**（`ruler.max_deletes_per_pass`）を設け、超えたら削除を実行せず停止してアラート。手動確認後に再開
- **数えて止めるのは「ルールが base を供給しているのに desired から外れた」削除だけ。** desired は「(ルール勝者 − intent skip) ∪ investment（record 意図 ∪ overrides）」から導出されるので、`toDelete`（既存予約のうち desired から外れた行）には EPG 由来の unmatch とユーザーの明示操作が混ざる。このうち**ユーザー（運用者）が投資を手放す書き込みをしない限り起きない削除はブレーカーの外**に置き、カウントにも入れずラッチ中でも実行する。判定は削除文の `WHERE` が適用の瞬間に行い、`program_investments` が空であることに加えて次のどちらかが立てば対象:
  - `reservations.rule_id IS NULL` — いまルールが base を供給していない行。**この列は EPG の変化だけでも NULL になる**（投資を持つ行はルールが外れても desired に残るのでそのパスで upsert され、`internal/ruler/sql.go` の `resolved` CTE が凍結するのは `base` と dedup 根拠 2 列だけ。`rule_id = EXCLUDED.rule_id` がそのまま NULL を書く。`TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease` が実測で固定）ので、**これ単体はユーザー由来の証明にならない**
  - `program_intents.action='skip'` — ユーザーが「録るな」と書いた

  intent クリア（`DELETE .../intent`）と「最後の investment だった overrides の削除」は前者、intent skip は後者に落ちる。**ルールの編集**で勝者が変わる経路は `rule_id` が残るので EPG 由来の unmatch と区別できず、ブレーカー対象のまま。**ルールの削除**はそもそも `toDelete` を経由しない（下記「止められる場所は ruler だけ」の表）
- **守備範囲を保っているのは `NOT EXISTS program_investments` のほうである。** `rule_id IS NULL` の行は、その NULL が書かれた時点で必ず投資を持っていた（ruler の upsert が NULL を書くのは「勝者なしで desired」= 投資ありの行だけ。もう 1 つの経路である `rules` の FK `ON DELETE SET NULL` も、`DeleteRule` が同一 tx で投資なしの行を**先に**消すので、生き残るのは投資ありの行だけ）。したがってブレーカーの外に出るには**投資を消す書き込みが別途必要**で、それができるのは per-program の api 3 本（`DELETE .../intent` / `DELETE .../overrides` / `PUT .../intent{skip}`。`openapi.yaml` のパスは `{programId}` を含みバルクが無い）、運用者が明示的に走らせる catalog rescue の復元（`internal/catalog/rescue.go`。`program_intents` を upsert するので record を skip で上書きしうる。バルクだが EPG に駆動されない）、`program_snapshots` の GC CASCADE（このとき予約行も一緒に落ちるので `toDelete` に現れない）だけ。**EPG 同期・ruler・reconciler はこの 2 表に一切書かない**ので、EPG が壊れてもブレーカー外の削除件数は「人が書いた回数」で頭打ちになる
- **混ぜたままにしたときの実害はラッチが無期限であるぶん無期限に続く。** intent クリアは `effective.skip` を立てない（クリアは「意見なし」であって「録るな」ではない）ので、ラッチ中に残る予約行は `listDesired`（`db.EvaluateSyncCandidates` の `Skipped`）からも除外されず、reconciler は schedule を消さず、番組は録画され続ける。失うのは導出で作り直せる予約行ではなくディスクとチューナーで、ブレーカーが守ろうとしているものより重い（`TestRunPass_LatchDoesNotWithholdIntentClearDelete` / `TestRunPass_IntentClearDeletesDoNotCountTowardBreaker` / `TestRunPass_LatchDoesNotWithholdSkipIntentDelete` が固定）。intent skip 側の実害はもともと予約一覧の表示上の残留だけだった（`intent.action='skip'` が `effective.skip` を立てるので録画は防がれる。`TestReconciler_SkippedReservationNotScheduled`）が、「明示操作は対象外、ただし skip を除く」という例外を増やさないため同じ側に置く
- **境界が 3 つある**（「明示操作は必ず即座に効く」「EPG では絶対に起きない」とは書かない）:
  - (a) ルール由来の unmatch と明示操作が同じ番組で重なった場合（`rule_id` が残ったままの行の意図を消した）は前者と区別できないのでブレーカー対象のまま。ブレーカーが発動しているのはまさにルール x EPG が大きく動いたときなので保守側に倒す
  - (b) 削除候補になる前提として EPG 射影に番組が残っている必要がある（射影から消えた番組の凍結は別の防御。EPG が復旧すれば次パスで自動的に解け、ラッチと違い人間の再開を待たない）
  - (c) **EPG 欠損中は投資を持つ行の `rule_id` が一斉に NULL に落ちる**ので、その最中にユーザーが投資を消すと、健全な EPG ならルール由来で残ったはずの予約がブレーカーの外で消える。EPG 復旧後の次パスでルールが作り直すので自己修復するが、この削除は「明示操作**からしか**説明できない」ものではない（`TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease` が (c) の前半 = `rule_id` の NULL 化と、投資がある間は消えないことの両方を測っている）
- **不変条件: 録画済みデータ（media_assets）に至る自動削除経路は retention reconcile のみ**。EPG・予約側の状態変化から録画物の削除に到達するパスを作らない
- programId が EPG から消えた予約は即削除せず猶予を置く（mirakc 自身も removed-from-epg を理由付き failed として通知してくる）。なお導出値 `orphaned` はこの用途ではなく「番組終了後に schedule が観測されなかった」を意味し、`recordings` に never-scheduled 行が存在するかどうかから読むたびに導出する（[schema.md](../schema.md) §3）

##### 止められる場所は ruler だけ

削除件数の閾値を持つのは ruler 側だけで、**reconciler 側には置かない**（両方に置いていた時期があるが、reconciler 側は誤発火しかしないので撤去した。末尾「経緯と失敗事例」）。reconciler が「消すべき schedule」と判断する経路は、desired（reservations）を減らす操作の数だけあるが、reconciler からはどれも「desired に無い schedule がある」以上には区別できない。ruler のブレーカーの対象かどうかで束ねると次の 5 通りに分かれる:

| 経路 | ruler の `MaxDeletesPerPass` の対象か |
|---|---|
| ruler が EPG の変化から導出削除した | 対象。ruler のブレーカーが既に通している |
| ルール**編集**（無効化・条件変更）で勝者が変わった | 対象。`rule_id` が残るので EPG 由来の unmatch と区別できない |
| ユーザーの明示操作で desired から外れた（intent skip、intent クリア、最後の investment だった overrides の削除） | **対象外**。削除文の `WHERE` が適用の瞬間に明示操作と判定し、カウントにも入れずラッチ中でも実行する（上記「大量削除サーキットブレーカー」） |
| ユーザーの明示操作のうちルール**削除**（`DeleteRule`） | **対象外**。API ハンドラ（`internal/api/rules.go`）が同一トランザクションで `reservations` を直接 DELETE し、ruler の `toDelete` も `MaxDeletesPerPass` も経由しない |
| 番組終了後の GC が予約行を刈った | **対象外**。GC は `runGC` という別経路で `runPassForSite` の `toDelete` を通らない（[ruler.md](ruler.md)「番組終了後の GC」と下記「GC は対象にしない」） |

ruler のブレーカーが通しているのは EPG 由来の導出削除（ルール編集で勝者が変わる経路を含む）だけで、**明示操作・ルール削除・GC はブレーカーの外にある**。reconciler から見ると、どの経路も「ruler あるいは API ハンドラが既に処理して DB にコミット済み」の状態でしか観測されない。reconciler にもう一段ブレーカーを置いても、desired に無い schedule があるという観測だけではこの区別ができず、ユーザーが取消した予約・ルール削除の一括処理（内訳を提示する確認 UI で安全性を担保済み）・GC の正常な一括削除（「長時間停止していた場合、再開後に溜まった期限切れ行を一括で消す」）に誤発火するだけだった。

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

##### GC は対象にしない

**番組終了後の GC（[ruler.md](ruler.md)「番組終了後の GC」）は `MaxDeletesPerPass` の対象にしない。** ブレーカーが守るのは「ルール x EPG」の評価結果から導出される削除だけで、EPG の一時的な欠損・フリッカーに引きずられて予約を大量に消してしまう事故（上記 EPGStation#692 のクラス）を防ぐためのもの。GC の削除対象は時刻の比較だけで決定的に定まり、EPG の状態には一切左右されない。むしろ reconciler/ruler が長時間停止していた場合、再開後に溜まった期限切れ行を一括で消すのは正常な挙動であり、ここをブレーカーで止めると実害のない削除が積み上がり続けるだけになる。

---

#### 経緯と失敗事例

- **reconciler 側の閾値の撤去**: M1-4 では ruler と reconciler の両方に削除件数の閾値を置いていたが、reconciler 側は誤発火しかしないので M2-5 で撤去した（issue #2 のコメント）。理由は上記「止められる場所は ruler だけ」のとおり
- **ラッチ化**: M1-4 の骨格はパス内で完結していて、次のパスでは何も覚えていなかった。「手動確認後に再開」を実現するために M2-5 で `circuit_breakers` 表による永続ラッチにした
- 再開 API の資源同定（`(site, name)` を PK とする `/breakers/{name}/resume`）は issue #102 で決めた
- **明示操作をブレーカーの外に出した**: M2-5 のラッチ化から issue #171 まで、`toDelete` は「desired から外れた理由」を区別せず数え・保留していた。intent skip は `effective.skip` が録画を止めるので実害が予約一覧の表示上の残留に留まったが、**intent クリア（`DELETE .../intent`）は `effective.skip` を立てない**ため、ラッチ中は「クリアしたのに人間が再開するまで録り続ける」になっていた（#154 の実装レビューで発見）。3 択（録画も止める／導出削除の対象から外す／UI 説明で足りるとする）のうち「対象から外す」を採った。「録画も止める」（reconciler 側で現在の desired を再評価する）は、根拠のない予約行が一覧に残り続ける上に desired の判定器が 2 つになる（reconciler が ruler と同じ材料を読み直す）ので却下。「UI 説明」は、ラッチが人間の再開を待つ無期限の状態である以上「クリアしたのに録れる」を真にできないので却下。判定を削除文の `WHERE` に置いたのは #29 型の窓（読み取りと適用の間に着地した意図を踏み潰す）を作らないため —— 呼び出し側は `toDelete` 全体を渡し、`RETURNING` で「実際に明示操作由来として消えた集合」を受け取って、残りをブレーカーに掛ける。分類がトランザクション外の古い読み取りで決まる余地がない。境界 (a)(b)(c) を上記「大量削除サーキットブレーカー」に明記してある —— 「明示操作は必ず即座に効く」とは書かない。**最初の版は「`rule_id` は EPG が動いても据え置かれるので EPG 由来の unmatch は `rule_id IS NULL` を作れない」という論証を書いたが、これは実測で偽だった**（投資を持つ行は desired に残るので upsert され、`resolved` CTE は `rule_id` を凍結しない）。結論は変わらないが、支えているのは `rule_id` の不変性ではなく「投資を消せるのは人だけ」のほうである（PR #273 のレビュー）
