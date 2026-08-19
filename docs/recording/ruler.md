> [recording.md](../recording.md) §3「Rokuban 側のコンポーネント」の一部。索引から辿る。

## 3. Rokuban 側のコンポーネント

### 3.1 ruler（ルール評価 → 予約生成）

EPG プロジェクションの番組をルールと突き合わせ、`reservations`（desired state）の base を生成・更新する。手動予約もルール由来の予約も同じテーブルに入り、区別は `program_intents.action`（`record` の有無）から導出する。**列に保存してはならない** --- 導出器が不可逆に書き換え、永続資産（録画履歴の `source`）に漏れる（[reservation-model.md](reservation-model.md) §4.4）。

**ルールエンジンは Rokuban 側に置く**。mirakc はルールベース自動予約をスコープ外と明言しており（contrib にサンプルがある）、まさに「ルール = サーバー、録画エンジン = エッジ」という分割を想定した作りになっている。

#### 評価は全量、書き込みは差分

**1 パスで全ルール x 全射影番組を評価する。差分駆動にしない。**

desired 予約が変わる契機は 3 つあり、EPG の変更はそのうち 1 つでしかない。

| 契機 | 差分駆動で拾えるか |
|---|---|
| EPG の番組が変わった | 拾える（変更行の検出が必要） |
| ユーザーがルールを編集した | 拾えない。EPG は変わっていない |
| 時間が経過して番組がウィンドウに入った | 拾えない |

差分駆動にすると契機ごとに経路が分かれ、取りこぼしが独立したバグになる。全量評価なら経路は 1 本で、どの契機でも次のパスで収束する（レベルトリガー）。

加えて全量評価でないと成立しないものが 2 つある。

- **大量削除サーキットブレーカー**（[breaker.md](breaker.md)）の「削除数が総数の N% 超過」は、desired 集合の全体が 1 パスで手に入らないと分母が定義できない
- **競合判定は集合の性質**。1 ルールの予約が増減すると他の予約の競合状態が変わるので、部分評価と相性が悪い（EPGStation の `updateRule` が `findTimeRanges` で影響範囲の予約を引き直しているのは同じ理由）

規模は問題にならない。EPG プロジェクションはローリングウィンドウで永久に有界（[データ層](../data.md) 参照）で、実測で 19 サービス x 8 日 = 2680 行。数百ルールとの突き合わせは pg_trgm GIN 込みで秒未満。評価は Postgres の集合演算で行うので、**予約 1 件ずつのループにはしない**。

**ただし書き込みは差分にする。** `reservations` には SSE 用の行トリガーがあるため、毎パス全予約の base を書き直すと NOTIFY が全行 x 毎パス飛び、クライアントの invalidate が鳴り続ける。base が実際に変わった行だけ UPDATE する（`WHERE base IS DISTINCT FROM ...`）。churn と bloat も避けられ、`updated_at` が「base が実際に変わった時刻」という意味を保つ。

起動契機は 3 つあるが、**定期パスが真実**で残り 2 つは投入を早めるヒントに過ぎない。ヒントを落としても定期パスが拾う。

| 契機 | 種別 |
|---|---|
| 定期（既定 10 分） | **真実**。デプロイ形態に応じて River `PeriodicJobs` か k8s CronJob が投入する（[データ層](../data.md) §2） |
| ルールの作成 / 更新 / 削除 | ヒント。api がルール書き込みと**同一トランザクションで** `InsertTx` する（dual-write にならない） |
| EPG 同期の完了 | ヒント。epg_sync ワーカーがパス完了時に投入する |

ヒントは `UniqueOpts{ByArgs, ByState}` で合流する。ルールを連続で編集してもパスは 1 回で足りる。

##### EPGStation の実績

EPGStation も全量評価であり、**差分駆動を実装した上で無効化している**（`EPGUpdater.ts`）。

```ts
this.updateManage.on(EPGUpdateEvent.PROGRAM_UPDATED, () => {
    // NOTE this.config.epgUpdateIntervalTime の周期で予約情報を更新させるため無効化
    // this.notify();
});
```

EPG 更新完了で `reservationManage.updateAll()` を呼び、全手動予約と全ルールを評価する。ルール編集時のみ `updateRule(ruleId)` の部分評価を持つが、正しさを担保しているのは周期的な全量パスの側。

真似しない点が 3 つある。いずれも「予約 1 件ずつ TypeScript で処理する」ことから来ている。

- ループの各要素間の `Util.sleep(10)`（ルール 100 個 + 手動予約 100 件で sleep だけで 2 秒）
- 全量更新のログが多すぎて必要になった `isSuppressReservesUpdateAllLog` 設定
- 手動予約の追加が全量更新に割り込むための優先度付き実行権

#### 複数ルール解決

- **desired 予約は programId につき最大 1 つ**。複数ルールがマッチした場合、予約オプション（priority、エンコードプロファイル、保持ポリシー）は最高 priority のルールから採る。記録するのは勝者だけ（`reservations.rule_id`）。負けたルールは `base` に何も供給しないので保存しない —— 必要になれば enabled ルールを `rulequery.MatchProgramIDsForRule` で回して同じ集合が出る
- **勝者決定は全順序**: `ORDER BY priority DESC, id ASC`（同率なら先に作られたルールが勝つ）。同率タイを不定のままにすると、全量パスごとに勝者が入れ替わって base の差分書き込みが発火し続け、mirakc に更新 API がないため reconciler が schedule を DELETE + POST で作り直し続けるフラッピングになる。差分書き込みは勝者決定の決定性を前提として要求する。新しい方でなく古い方を勝たせるのは、同率の新ルール追加が既存予約の base を動かさないため（勝たせたければ priority を上げる — 暗黙の新旧より明示的な優先度操作）
- **除外はルール単位ではなく番組単位のオーバーライド**（reservation の skip フラグ）。どのルール経由でマッチしていても一貫して除外される
- EPGStation#538（複数ルールにマッチした番組を除外できない）は、予約がルール単位で管理されていたために起きた不整合。Rokuban は programId ベースなので構造的に防げる

#### 重複排除（再放送スキップ）

- EPGStation#704 の教訓: 囲み文字（:heavy_multiplication_x::heavy_multiplication_x:等）を一律除去する正規化は「前編/後編」の区別まで消して誤判定する。**記号除去 + 完全一致ではなく、pg_trgm の類似度ベース**で設計する（閾値はルール単位で調整可能に）
- EPGStation#473 の要望（この番組を重複扱いにする / しないを手動で上書きする）のうち、**予約側は実装済み**: `program_intents.action = 'record'` が dedup の `base.skip` に勝つ合成として `db.EffectiveOptions` が解く（§4.2）。**履歴（`recordings`）側の除外印は作らない** —— 誤って抑制された放送は予約側の `action = 'record'` で個別に勝たせればよく、**1 本録れた時点でその録画が新しい抑制元になって以降の再放送はまた弾かれる**（下記「ルールの削除は履歴のスコープを消す」と同じ一過性。`TestRunPass_DedupeRecordIntentThenNewRecordingSuppressesAgain`）ので、特定の録画を比較対象から外す印は同じ状態に恒久の構造を足すだけになる。抑制が 1 本外しても止まらないのは閾値がそのシリーズに対して低いときで、それを直すのは印ではなく `rules.dedupe_threshold` / `dedupe_window` である（外した次に録れた 1 本が同じ抑制元になる）。逆向き（録れていない番組を今後スキップさせる）は紐づける `recording_id` が無く、意味は予約側の `action = 'skip'` そのもの。境界: 予約側の印は射影に出ている放送にしか付けられないので、まだ EPG に無い先の放送を先回りして「重複扱いにしない」とは言えない。
- 判定に使った根拠（マッチした履歴、類似度）を予約に記録し、UI で「なぜスキップされたか」を説明可能にする

実装は `internal/ruler/dedupe.go`（候補の集合を jsonb で渡す集合演算 1 文）。判定規約:

| 項目 | 決めたこと |
|---|---|
| 比較対象 | **同じ `rule_id` の `recordings` だけ**。「同じルールが同じ番組シリーズを指している」前提に乗る（グローバルな突き合わせはしない）。**ルールを削除すると履歴は比較対象から外れる**（下記「ルールの削除は履歴のスコープを消す」） |
| 状態 | `status = 'finished'` のみ。`recording`（進行中）も `failed` も「録れた」とはみなさない |
| `deleted_at` | **絞らない。** ごみ箱に入れても物理削除しても行は tombstone として残り、重複排除は機能する契約（[スキーマ](../schema.md) §5）。`deleted_at IS NULL` を足すのは書き忘れの修正ではなく契約違反 |
| 時間窓 | `rules.dedupe_window` が NULL なら**無制限**（`rules` の CHECK は `dedupe_enabled` のとき `dedupe_threshold` だけを要求し window は任意） |
| 勝者 | `DISTINCT ON (program_id)` で類似度最大の 1 件。tie-break は `recordings.id ASC` |

**自分自身の録画は除外する**（`(network_id, service_id, event_id)` の不一致）。放送済み番組の予約は GC（終了 + `retention_grace`）まで残り、EPG 射影も同じ地平まで番組を保持するので、録画が `finished` になった次のパスで **similarity = 1.0 の自己一致が必ず起きる**。実装中に除外述語を外して再現済み。害は表示だけではない: `effective.skip = true` になると `reconciler.listDesired` から落ち、`recordNeverScheduled` / `detectStartDelays` の入力からも外れるため、**重複排除が無関係な状態機械の DB 状態を変えてしまう**。site は比較に入れない（同一放送は全サイトで同じ programId を持ち、マッチした全サイトで予約を作る N 予約が既定なので、サイト間の共食いも同時に防ぐ必要がある）。

tie-break を決定的にするのは必須で、任意ではない。同じ類似度の録画が複数あるときに勝者が毎パス入れ替わると、base の差分書き込みが発火し続けて NOTIFY が鳴り止まず、mirakc に更新 API がないため reconciler が schedule を DELETE + POST で作り直し続けるフラッピングになる（本節「複数ルール解決」の priority 同率タイと同じクラスの問題）。

**`base.skip` に skip を載せる唯一の経路が重複排除である。** ユーザーの「録るな」は `program_intents.action` が担い、`action = 'record'` が dedup の skip に勝つ合成は `db.EffectiveOptions` の 1 箇所で解く（§4.2）。このとき**根拠 2 列は消さない** — UI が「重複と判定したが録る」と説明できるようにするため。

根拠 2 列（`dedup_match_recording_id` / `dedup_similarity`）は base と同じ凍結規則に従う。ルールが base を供給している間は毎パス作り直し、マッチが無ければ NULL に戻す（前パスの根拠を残さない。不変条件 9）。`rule_id` が外れたら base と一緒に凍結する — base だけ凍結して根拠を消すと「なぜ skip なのか説明できない base」が残るため。FK を張っていないので、参照先の録画が消えた場合もこの毎パスの作り直しが孤立を解消する（[スキーマ](../schema.md) §3）。

##### ルールの削除は履歴のスコープを消す

比較対象を `rule_id` で絞るということは、**比較のスコープは生きている `rules` の行が定義している**ということである。ルールを削除すると 2 段階で履歴が効かなくなる。

1. `recordings.rule_id` は `rules` への FK が `ON DELETE SET NULL`（`00006_rules.sql`）なので、そのルールで録れた履歴の `rule_id` が NULL に落ちる。以後どのルールの比較対象にもならない
2. 同じ条件でルールを**作り直しても** id は新しくなるので、過去の録画は 1 件もマッチしない。直後のパスでは重複としてスキップされなくなる（実際に余分に録れる量は下記のとおり一過性）

**これは仕様である**（`internal/ruler/dedupe_test.go` の `TestRunPass_DedupeHistoryLeavesScopeOnRuleDelete` が 3 段階で固定している: ルールが生きていれば skip / 削除→作り直し直後は skip しない / 新ルールで 1 本録れるとまた skip する）。条件を大きく変えたいだけなら**削除して作り直すのではなく編集する** —— `PATCH /api/rules/{id}`（UI の「編集」「検索しながら編集」）は id を保つので履歴も保たれる。

`deleted_at` の tombstone 契約（上表）との非対称に見えるが、守っている主語が違う。tombstone が守るのは「録画したという不可逆な事実」で、ユーザーがファイルを消しても事実は残る。ルール削除で失われるのは事実ではなく**比較の枠**で、`recordings` の行は 1 行も減っていない。倒れる方向も「録り逃し」ではなく「余計に録る」側であり（[予約モデル](reservation-model.md) §4.3「迷ったら録る側に倒す」）、**新ルールの下で 1 本録れれば以降の再放送はまた弾かれる**（上と同じテストの段階 3 で測っている: `base.skip` が true に戻り、根拠 2 列は新しい録画を指す）—— 履歴が積み直るまでの一過性の過剰録画になる。この一文が受け入れ可能かどうかの分かれ目で、偽なら帰結は「窓の中の再放送を全部録り直す」に戻る。

`recordings.rule_id` の FK を外して値を残す案は採らない。作り直したルールが新しい id を持つ以上、上の 2 が残って**症状が消えない**（履歴に旧 id を保存しても新ルールの比較対象にはならない）。削除→作り直しをまたいで効かせるには「ルール名をキーにする」等の別の同定が要るが、名前キーは同名の別ルールの履歴を黙って混ぜるので、いま乗っている前提より弱い前提に置き換わる。加えて、恒久に解決しない `ruleId` を履歴に残すと「一覧が未解決だから解決できない」という**一時的な**状態の表示（[フロントエンド](../frontend/recordings.md)「ルール名の解決」）と区別が付かなくなる。

削除の確認ダイアログは、`dedupeEnabled` なルールに限りこの帰結を事前に伝える（`web/src/pages/rules.tsx` の `deleteRuleConfirmMessage`。文面は上の測定に合わせ「次の再放送を録り直す / 1 本録れれば以降はまた弾かれる」までを言う）。**重複排除の設定自体を編集する UI は現状無い**（`web/src` で `dedupe*` に触るのは `buildRuleInput` の `preserve` と skip 理由の表示だけ）。`dedupeEnabled` なルールは `POST` / `PATCH /api/rules` を直接叩いて作ったものに限られ、この確認文に到達する経路も今はそこだけになる。

**類似度検索に trgm GIN は効かない。** `gin_trgm_ops` が加速するのは `%` / `<%` / LIKE / 正規表現で、`similarity()` の関数呼び出しはインデックスに乗らない。`%` は閾値をルール単位ではなく GUC `pg_trgm.similarity_threshold` から読むため `rules.dedupe_threshold` と直接は噛み合わない（前段フィルタにする手順は `internal/ruler/dedupe.go` のコメントに残してある）。家庭用の履歴規模では素の走査で足りるので、隠れたセッション状態を持ち込む前に実測する。

#### サイトの扱い

**ルールはサイトに従属しない**グローバルな永続資産で、rules に site 列はない。サイトは条件の一次元であり、`rule_sites` 子テーブル（指定なし = 全サイト。他の条件テーブルと同じ規約）で絞り込む。FK は張らない（レジストリは設定ファイルにある — 外部に真実があるものは存在を制約できない）。**書き込み時にはレジストリと照合する**: `rule_sites` に載せる各 site 名は、その api プロセスが読んでいる `config.mirakc` / `mirakcs`（`GET /api/sites` と同じ一覧）に存在しなければ 400 になる。タイポを黙って保存し、どのサイトにも一致しない条件として無音で失敗させない。レジストリから site が消えたあとに残る既存行はこの照合の対象外（書き込み時照合は過去行を掃除しない）。

**実体化はマッチした全サイトで予約を作る（N 予約が既定）**。同一放送は全サイトで同一 programId を持つ（Mirakurun の ID 合成）ため `(site, program_id)` ごとに予約行ができ、全部録る。これは意図された既定値: 複数録画してドロップ統計で選別するワークフローを一級として扱い、サイト選好・自動フェイルオーバーという機構を持たない。チューナー競合は mirakc の調停に任せ、負けは `need-rescheduling` として観測される（競合の可視化と開始遅延検出器がこの運用を支える）。絞りたければ `rule_sites` が唯一の機構。

NID/SID は放送規格のスコープでサイトに依存しないため、地上波の条件は地域が違えば構造的にマッチしない（NID が違う）。全サイトマッチが実際に効くのは BS/CS と同一地域の複数サイトのみ。

サイト名は安定識別子として扱い、オンラインのリネームはサポートしない（SQL 付け替えを伴う運用作業）。無断リネームは旧サイトの射影が stale になり導出削除として現れるため、サーキットブレーカーが受け止める。

#### 番組終了後の GC

`reservations` / `program_intents` / `program_overrides` の物理削除（GC）は ruler の 1 パス内で、全サイト評価の後に 1 回だけ行う（`internal/ruler/ruler.go` の `runGC`）。実際に DELETE するのは `program_snapshots` の 1 表だけで、対象は `start_at + duration_ms < now() - 猶予` を満たす行（`reservations` の active/detached/orphaned を問わない）。`reservations` / `program_intents` / `program_overrides` はこの表への `(site, program_id)` FK が `ON DELETE CASCADE` なので、スナップショットが消えると 3 表とも一緒に落ちる。猶予には既存の `epg.retention_grace`（既定 24h、EPG プロジェクションのローリングウィンドウと同じ設定）をそのまま流用する。専用の設定項目を増やさず、「EPG から消える」と「予約・意図として GC される」の寿命を揃える。`recordings` と `never_scheduled_events` は `program_snapshots` への FK を持たないので、この削除で録画履歴や欠測の観測が失われることはない。

**GC は大量削除サーキットブレーカー（`MaxDeletesPerPass`）の対象にせず、ブレーカー発動中でも動く**。GC の削除対象は時刻の比較だけで決定的に定まり、EPG の状態には一切左右されないため（理由の全体は [breaker.md](breaker.md)「GC は対象にしない」）。

---

#### 経緯と失敗事例

- GC は当初 3 表それぞれに別々の DELETE 文があり、表ごとに違うスナップショット列を見てドリフトしていた（表ごとに違う時刻で GC していた）。issue #27 で `program_snapshots` への `ON DELETE CASCADE` FK による 1 本の DELETE（`DeleteEndedProgramSnapshots`）に集約した
- `recordings.reservation_id` 列（GC 当時は `ON DELETE SET NULL`）は issue #158 で列自体を削除した
- 重複排除（`internal/ruler/dedupe.go`）の実装は M2-6
- 「ルールの削除は履歴のスコープを消す」は issue #215 の決定。`recordings.rule_id` の FK を外して値を残す案（`dedup_match_recording_id` で FK を張らなかった議論と同型）を評価したうえで採らなかった —— 作り直したルールが新 id を持つ以上、値を残しても症状（`dedupe_window` 内の再放送を録り直す）が消えないため。判断の全文は同 issue のコメントにある

