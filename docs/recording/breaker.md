> [recording.md](../recording.md) §3.2「reconciler」の一部（大量削除サーキットブレーカー）。索引から辿る。

#### 大量削除サーキットブレーカー

予約は「ルール x EPG」から導出されるため、EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう。EPGStation#692（予約と録画が勝手に消える）はこの障害クラスの実例。

対策:

- **1 回の ruler パスでの削除数に閾値**（`ruler.max_deletes_per_pass`）を設け、超えたら削除を実行せず停止してアラート。手動確認後に再開
- **ブレーカーが数えるのは `toDelete`（既存予約のうち desired から外れた行）で、外れた理由が EPG の一時欠損かユーザーの明示操作かを区別しない。** desired は「(ルール勝者 − intent skip) ∪ investment（record 意図 ∪ overrides）」から導出されるため、ルール編集で勝者が変わる／intent skip を立てる／intent をクリアする（`DELETE .../intent`）／最後の investment だった overrides を消すといったユーザーの明示操作も同じ `toDelete` に混ざり、ラッチ中は他の導出削除と同様にカウント・保留される（非網羅）。**ルールの削除だけは例外で `toDelete` を経由しない**（下記「止められる場所は ruler だけ」の表）。区別しないのは単純さを優先した設計判断で、代わりに影響件数の内訳を提示する確認 UI が安全装置になる
- **実害は経路によって異なる。** intent skip は `intent.action='skip'` により `effective.skip` を立てるため、ラッチ中に予約行が残っても `listDesired`（`db.EvaluateSyncCandidates` の `Skipped`）が同期対象から除外し、録画そのものは防がれる（実害は予約一覧の表示上の残留のみ。`TestReconciler_SkippedReservationNotScheduled` が固定）。**intent クリア（`DELETE .../intent`）はこの限りではない** —— `program_intents` の行を消すだけで `effective.skip` を立てないため、ラッチ中に残る予約行は `listDesired` から除外されず、既存 schedule も消えず、番組は録画され続ける。この経路の扱いは未決（issue #171）
- **不変条件: 録画済みデータ（media_assets）に至る自動削除経路は retention reconcile のみ**。EPG・予約側の状態変化から録画物の削除に到達するパスを作らない
- programId が EPG から消えた予約は即削除せず猶予を置く（mirakc 自身も removed-from-epg を理由付き failed として通知してくる）。なお導出値 `orphaned` はこの用途ではなく「番組終了後に schedule が観測されなかった」を意味し、`recordings` に never-scheduled 行が存在するかどうかから読むたびに導出する（[schema.md](../schema.md) §3）

##### 止められる場所は ruler だけ

削除件数の閾値を持つのは ruler 側だけで、**reconciler 側には置かない**（両方に置いていた時期があるが、reconciler 側は誤発火しかしないので撤去した。末尾「経緯と失敗事例」）。reconciler が「消すべき schedule」と判断する経路は、desired（reservations）を減らす操作の数だけあるが、reconciler からはどれも「desired に無い schedule がある」以上には区別できない。ruler のブレーカーの対象かどうかで束ねると次の 4 通りに分かれる:

| 経路 | ruler の `MaxDeletesPerPass` の対象か |
|---|---|
| ruler が EPG の変化から導出削除した | 対象。ruler のブレーカーが既に通している |
| ユーザーの明示操作で desired から外れた（intent skip、intent クリア、最後の investment だった overrides の削除、ルール**編集**で勝者が変わるなど） | 対象。上記「大量削除サーキットブレーカー」のとおり区別せず同じ `toDelete` に混ざる |
| ユーザーの明示操作のうちルール**削除**（`DeleteRule`） | **対象外**。API ハンドラ（`internal/api/rules.go`）が同一トランザクションで `reservations` を直接 DELETE し、ruler の `toDelete` も `MaxDeletesPerPass` も経由しない |
| 番組終了後の GC が予約行を刈った | **対象外**。GC は `runGC` という別経路で `runPassForSite` の `toDelete` を通らない（[ruler.md](ruler.md)「番組終了後の GC」と下記「GC は対象にしない」） |

ruler のブレーカーが通しているのは EPG 由来の導出削除と、desired から外れる形の明示操作で、**ルール削除と GC はブレーカーの外にある**。reconciler から見ると、この 2 経路以外は「ruler が既に処理して DB にコミット済み」の状態でしか観測されない。reconciler にもう一段ブレーカーを置いても、desired に無い schedule があるという観測だけではこの区別ができず、ルール削除の一括処理（内訳を提示する確認 UI で安全性を担保済み）と GC の正常な一括削除（「長時間停止していた場合、再開後に溜まった期限切れ行を一括で消す」）に誤発火するだけだった。

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
