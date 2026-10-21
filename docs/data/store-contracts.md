> data.md §9 の本文。索引から辿る。

## 9. ドメインごとの store 契約

永続化はドメインごとの interface を通す。**各メソッドが自身のトランザクションを持ち、呼び出し側へ tx を渡さない。**

呼び出し側に tx を渡すと、呼び出し側が別々のメソッドの書き込みを後から 1 つの tx にまとめられるので、どの書き込みが原子的かが契約から読めなくなる。

### 契約の置き場

契約は 1 つの共通パッケージに置き、interface は利用側の用途ごとに割る。tx の束ね方（原子性）はメソッドが持つので、割っても変わらない。

pgstore はドメインごとの struct（pool を持つ）で、そのドメインの interface をまとめて実装する。利用側は使う interface だけを個別のフィールドで持ち（`jobq.Enqueuer` と同じ形）、全 interface を束ねた集約 struct は作らない。集約 struct は、利用側が使わない契約まで依存に含めてしまう。

置き場は `internal/store` である。1 パッケージにするのは、エラーとドメイン型を全ドメインで共有するためである。契約パッケージは pgx と `sqlcgen` を import せず、depguard がこれを検査する。

### メソッドの単位

読み取りは複数のドメインをまたいでもよい。複数ドメインの表を原子的に書く必要があるときだけ、その書き込みを一つのメソッドに束ねる。

表の数でメソッドを分けると、原子的であるべき書き込みが呼び出し側へ漏れる。

### ジョブ投入

ジョブ投入はデータ更新を commit した後のヒントとする。ヒントを失っても、定期 reconcile が永続状態から仕事を見つけ直す。

River の `InsertTx` のように同じ tx でジョブを投入すると、DB の外にあるキュー（Redis など）を使う実装が契約を満たせなくなる。

### 方言に依存しない前提

契約は DB 方言による挙動差を前提にしない。

文が失敗したら、tx をロールバックしてからエラーを返す。MySQL は文が失敗した後も tx が続くので、ロールバックせずに返すと途中までの書き込みが commit されうる。

一意違反・排他制約違反は `ErrConflict`、対象が無いときは `ErrNotFound` に写す。DB 固有のエラーコードや `pgx.ErrNoRows` を呼び出し側へ漏らすと、別の実装が同じ契約を満たせなくなる。

### 時刻

時刻は、比べる相手を書いた時計に合わせる。

比べる相手が store の記録した時刻（`updated_at`・`attempted_at`・`observed_at` など）なら、DB の時計で比べる。この場合の契約は、時刻ではなく幅（`time.Duration`）を受け取る。比べる相手が外から来た時刻（EPG の放送時刻など）なら、契約は呼び出し側の `time.Time` を受け取る。

store が記録する時刻は DB の時計で付け、呼び出し側の時計を使わない。api と worker は別ホストなので、時計がずれると、書き手をまたぐ時刻の大小比較が反転しうる。

ごみ箱の猶予だけが例外である。api が DB の時計で付けた `deleted_at` を、worker の時計の cutoff と比べている。猶予が日単位でずれが効かないので、今の形のままにする。

<details><summary>根拠</summary>

- DB の時計で比べる前例（GC）: `internal/db/queries/reservations.sql` の `DeleteStaleNeverScheduledEvents`。`observed_at < now() - horizon` で比べ、`PruneNeverScheduled` が呼ぶ
- DB の時計で比べる前例（孤児候補）: `internal/store/pgstore.Analysis.FailOrphanLogoCandidates`。`ListRunningCMLogoCandidates` が返す DB の `now()` を使う
- 例外: `internal/worker/delete_reconcile.go` の `trashCutoff` と `recordings_trash.sql` の `deleted_at = now()`

</details>

### 視聴状態

視聴状態の書き込みは 1 つの契約にまとめる。再生位置は録画ごとに保存する。視聴済みの印は、押した録画の行にだけ置く。

読み取り側は同じ放送イベントの印を束ねるので、`ClearWatchedForEvent` はそのイベントの全行から印を消す。

`MarkWatched` は印の upsert と再生位置の削除を 1 メソッドにする。2 つは「視聴済みにする」という同じ業務操作であり、呼び出し側が 2 回に分けて呼ぶと、印だけ付いて再生位置が残った状態で止まりうる。契約テストは、再生位置の削除が失敗したときに印の書き込みも巻き戻ることを固定している。

<details><summary>根拠</summary>

- 契約: `internal/store.PlaybackState`
- PostgreSQL 実装: `internal/store/pgstore.Playback`
- 視聴済みの書き込みと位置の削除: `internal/store/contracttest.RunPlayback`

</details>

### ごみ箱と完全削除

ごみ箱の操作は、利用側ごとに 2 つの契約に分ける。api が使う操作と、削除 reconcile が使う操作が違うからである。

api が使うのは論理削除・復元・即時削除要求をまとめた `RecordingTrash` で、削除 reconcile が使うのは完全削除の確定だけの `RecordingPurge` である。実装は録画のドメインの `pgstore.Recordings` が両方を持つ。

同じ録画の復元と即時削除要求は直列化される。要求が先に確定すれば、復元がそれを取り下げる。復元が先に確定すれば、後から来た要求は録画をごみ箱に戻して残る。どちらの順でも、復元された録画に要求だけが残ることはない。要求だけが残ると、次の普通の論理削除でごみ箱の猶予を飛ばして即時削除の対象になるが、ユーザーは即時削除を要求していないからである。

仕組みは [削除エンジン](../storage/retention.md) §7「復元と即時削除要求の競合」にある。

`RecordingPurge.MarkPurged` は呼び出し側が渡す cutoff を使い、猶予を過ぎて削除対象アセットが残っていない録画の `purged_at` を確定する。返すのは今回初めて完全削除を確定した行だけである。呼び出し側はこの結果を削除通知に使い、別の判定で発火対象を作り直さない。

<details><summary>根拠</summary>

- 契約: `internal/store.RecordingTrash` / `internal/store.RecordingPurge`
- PostgreSQL 実装: `internal/store/pgstore.Recordings`
- 復元と即時削除要求の直列化、完了記録の冪等性: `internal/store/contracttest.RunTrash`
- 削除述語と `purged_at` の意味: [削除エンジン](../storage/retention.md) §7

</details>

### チューナーと容量

チューナー同期は、sweep の印の取得、全量 upsert、古い行の削除を別々の store メソッドで行い、同期全体を 1 つの tx にまとめない。印の取得と upsert の間に mirakc への問い合わせが入るためである。upsert の全行は 1 つのメソッドの中で原子的に反映する。

削除の基準には、upsert より前に `SweepMark` が返した DB の時刻を渡す。呼び出し側の時計を使うと、時計のずれで射影全体を消しうる。

容量の読み取りは、予約の base・overrides と意図を解決せずに渡す。effective options の解決と Hall 条件の判定は `internal/capacity` が行う。

保存前プレビューは、対象ルールの状態の読み取りと候補の読み取りを別のメソッドにする。ルールはリクエストの時点で引けるが、候補は番組検索の後でしか引けないためである。1 つにまとめると、存在しないルールの 404 が検索の後ろへずれる。ルールの読み取りは、対象が無ければ `ErrNotFound` を返す。

<details><summary>根拠</summary>

- チューナー同期と一覧: `internal/store.TunerSync` / `TunerReader`
- 容量読み取り: `internal/store.Capacity`
- PostgreSQL 実装: `internal/store/pgstore.Tuners`
- バッチの原子性・需要の区間の終了端: `TestTunersContract`
- 容量需要と候補の抽出: `ListCapacityDemand` / `ListCapacityDemandAllSites` / `ListCapacityPreviewCandidates`

</details>

### 録画の観測

watcher は mirakc の record を観測するたびに、record の同期行・録画・品質イベントを 1 つのメソッドで原子的に書く。ingest の投入は、その commit の後に送るヒントにする。

契約は利用側ごとに 3 つに割る。観測を書く `RecordObservations`、ingest worker が転送先を読む `IngestTarget`、metrics が未取り込みの滞留を読む `RecordBacklog` である。実装は録画のドメインの `pgstore.Recordings` が全部持つ。

同じ `(site, record_id)` の観測は直列化される。直列化しないと、並行した観測が同じ record に録画を作ろうとし、後発が一意違反で失敗する。録画の由来は、同じ tx で読んだ予約と意図から導出する。

録画の title を書く操作は、分類ルールの当たりも保つ。これは録画ドメインの約束であり、PostgreSQL 実装ではトリガーが満たす。

品質イベントの時刻は DB の時計で付ける。呼び出し側は時刻を渡さない。

ingest をヒントにできるのは、取りこぼしを sweep が拾い直すからである。mirakc の record は ingest の DB commit 後にしか消えず、定期の record sweep は録画中・完了の record を再処理するたびに ingest を投入し直す。

<details><summary>根拠</summary>

- 契約: `internal/store.RecordObservations` / `IngestTarget` / `RecordBacklog`
- PostgreSQL 実装: `internal/store/pgstore.Recordings`
- 同じ record の直列化: `TestRecordObservationsContract/same record observations serialize`
- 契約テストは既存の同期行を置いて競合させる。行が無いと INSERT の一意制約だけで直列化されてしまい、ロックの有無を検出できないからである。確保の直後に同期点を差し込み、後発が先発と同時に先へ進めるかを見る
- 原子性（一意違反で同期行も残らない）: `TestRecordObservationsContract/record sync rolls back with recording conflict`
- 失敗の観測の収束と成功による置き換え: `TestRecordObservationsContract` の failed 系のケース
- 古い同期行の削除は ID の集合で判定する: `TestRecordObservationsContract/stale record syncs are decided by the live id set`
- ingest の投入の判定: `TestRecordObservationsContract/ingest hint follows the recording status`
- 品質イベントの時刻: `TestRecordObservationsContract/quality events carry a readable time`
- title の分類: `recordings_label_rule_sync_update` trigger / `TestRecordingsLabelRuleTrigger_FollowsTitleUpdates`
- commit 後のヒント: `TestProcessRecord_EnqueueFollowsCommit`

</details>

### 録画の検索

録画一覧とシリーズ棚では検索条件を共有し、API の状態は取得した事実から導出する。

録画一覧・単体取得・棚は `RecordingQuery` に置く。フィルタは OpenAPI の生成型を使わない `RecordingFilter` とし、HTTP パラメータの検証と変換は api が行う。

SQL の組み立ては pgstore に置く。ジャンルの配列の重なり、行値の比較（カーソルと service の組）、キーワードの述語は方言の機能なので、契約は意味だけを持ち、方言は実装に置く。

一覧と棚は、共通する検索条件の WHERE を同じ関数で組み立てる。複製すると片方だけ条件が増え、絞り込み結果がずれる。

録画の検索は EPG に結合しない。理由は [検索](search.md) §5「録画検索は rulequery を共有しない」にある。共有するのはキーワードの正規化方言だけである。述語 `keywordClause` は pgstore の番組検索と録画検索が呼び、条件のコンパイラと列名の対応は共有しない。

契約は、SQL の列を写した行を返す。API のレスポンス状態は `recordingFromListFields` で毎回導出する。導出済みの状態を SQL に焼くと、導出規則を API と DB の両方に持つことになる。

一覧と棚は、汎用プランへの切り替えを避ける実行方式で毎回計画させる。これは pgstore の実装上の制約で、契約には現れない。理由と測定値は [REST の判断](../api/rest.md) の「プリペアド文キャッシュと再計画」にある。

<details><summary>根拠</summary>

- 契約: `internal/store.RecordingQuery` / `RecordingFilter`
- PostgreSQL 実装: `internal/store/pgstore.Recordings`
- 一覧と棚の共通条件: `recordingsFilterWhere`
- キーワードの述語: `keywordClause`

</details>

### 予約の意図・上書き

利用者の録画意図（record / skip）とオプションの上書きは、ruler が作る予約とは別に、この契約が commit する。予約そのものはこの契約が書かない。

意図・上書きの書き込みは、番組スナップショットの確保と同じトランザクションで確定する。`PutIntent` は EPG 射影か既存の凍結スナップショットを必要とし、どちらも無ければ `ErrNotFound` を返す。`PatchOverrides` も同じ条件で `ErrNotFound` を返す。

同じ番組の書き込みは、射影にある番組でも凍結スナップショットだけの番組でも直列化する。`PatchOverrides` は、store が直列化した後の既存の上書きと予約の base にパッチをマージし、その実効値を検証する。違反は型付きエラーで返る。直列化の後に評価するのは、先に読むと別フィールドの同時 PATCH が互いの値を消すからである。マージ結果が空なら上書きの行を削除する。

予約の詳細・一覧・重なりに必要な結合結果は store が返す。`source` / `state` と実効オプションは、読み取り時に api が導出する。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.Reservations`
- 射影・凍結スナップショット・欠損の意図書き込み: `TestReservationsContract/intent_snapshots_projection_and_rejects_missing_program`
- 欠損番組への PATCH: `TestReservationsContract/patch_rejects_a_missing_program`
- 射影にある番組の同時 PATCH: `TestReservationsContract/concurrent_field_patches_preserve_both_values/projected_program`
- 凍結スナップショットだけの番組の同時 PATCH: `TestReservationsContract/concurrent_field_patches_preserve_both_values/frozen_snapshot`
- 空の上書き行の削除: `TestReservationsContract/empty_overrides_delete_their_row`
- 一覧・詳細・重なりの読み取り: `TestReservationsContract/reservation_reads_return_details_and_overlaps`
- マージの設定と reset: `TestMergeOverrides_SetsOnlyGivenFields` / `TestMergeOverrides_ResetClearsEachField`
- reset の対象が Options と一致すること: `TestOverrideFieldsMatchOptions`

</details>

### 録画ルール

ルールの作成・更新・削除と EPGStation からの取り込みは、それぞれ 1 つの tx で確定する。

`Rules` は API が使うルールの読み書きの契約である。作成と更新は、ルール本体と全ての子条件を同じ tx で書く。更新では子条件を全て置き換える。

ルールの削除だけは予約の表も書く。意図の無い予約の削除とルールの削除を別の tx にすると、間で切れたときにルールの無い予約が残るからである。意図の有無は削除の瞬間に再評価するので、削除と同時に入った意図の予約は消えない。意図のある予約と、そのルール由来の録画は残り、ルールへの参照だけが外れる。

`RuleImport` は、EPGStation のルール ID で既存のルールを引く取り込みの契約である。ID の保存方法は store の内側に閉じる。

正規表現の構文検査は `ProgramSearch.ValidateRegex` を使う。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.Rules` / `internal/store.RuleImport`
- PostgreSQL 実装: `internal/store/pgstore.Rules`
- 子条件の挿入失敗で create が巻き戻る: `TestRulesContract/create child failure rolls back`
- 子条件の全置換と失敗時の復元: `TestRulesContract/update replaces all children atomically`
- 削除と予約の保持: `TestRulesContract/delete preserves reservations with intent`
- ルールの削除が失敗したときの予約の保持: `TestRulesContract/delete failure keeps reservations without intent`
- EPGStation import の冪等性と原子性: `TestRulesContract/EPGStation import uses stable identity atomically`

</details>

### メディア読み取りとストレージ観測

メディアとストレージの読み取りは、利用側ごとの契約に分ける。DB の行型を利用側へ広げないためである。

streamer のハンドラは `MediaServing` を受け取る。worker のジョブは `MediaReconcile` と `StorageObservation` を、API のハンドラは `MediaReader` と `StorageObservation` を受け取る。api ロールと worker ロールの起動時の検査も `MediaReconcile` を使う。実装はどれも `pgstore.Media` が持つ。streamer が SQL の行型を知ると、配信してよい行の条件が streamer の中に散らばる。API が行型を知ると、読み取り条件が API の中に散らばる。

`MediaServing` は、配信元のアセットと、追っかけの対象と、原本 VOD の録画状態を返す。対象が無ければ `ErrNotFound` を返す。`deleting` と `deleted` のアセットは配信元にしない。ごみ箱の録画は、アセットも追っかけの対象も返さない。

worker のジョブは `MediaReconcile` から、再計算する候補と、エンコード計画の比較材料を読む。比較材料は、encoded の行と凍結された keep 区間を同じ時点の 1 回の読み取りで返す。

起動時の検査は 2 つある。1 つは profile の意味の検査で、過去の encoded と作り直し要求を証拠として読む。api ロールと worker ロールの両方が走らせる。もう 1 つは `rel_path` の名前空間の検査で、worker ロールだけが走らせる。原本と encoded のうち `deleted` でない行を見て、`rel_path` が `sites/{site}/` で始まらない行があれば、起動を拒む。site の成分が空の `sites//` も拒む。サムネイルとシークタイルは `thumbnails/` の名前空間にあるので、この検査の対象にしない。

worker は `StorageObservation` に `storage_sync` と、孤児・実体無しの候補を記録する。API は同じ interface から観測値を読む。孤児の判定に使う既知パスの一覧は、`media_assets` の状態に関係なく全ての `rel_path` を、`sites/{site}/` を含めたまま返す。走査で得るパスは `sites/{site}/` を含むので、接頭辞を落とすと一致せず、全ての原本が孤児候補になる。`thumbnails/` のように `sites/` の外にある予約名前空間の行も含める。含めないと、サムネイルが孤児として記録される。削除途中のファイルも一覧に残す。外すと、削除エンジンが消している途中のファイルが孤児候補として記録され、報告の対象になる。

候補の経過時間は DB の時計で比べる。契約は時刻ではなく `time.Duration` を受け取るので、worker の時計がずれても、公開前の名前の孤児を自動削除する時期（`agedUnpublishedOrphans`）と、孤児・実体無しを報告する時期は変わらない。一般則は [時刻](#時刻) にある。

<details><summary>根拠</summary>

- 契約: `internal/store.MediaServing` / `MediaReconcile` / `MediaReader` / `StorageObservation`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- 原本・encoded の配信元が active だけであること: `TestMediaStoreContract` の `serving returns only active assets`
- サムネイル・シークタイルとごみ箱の録画: `TestMediaStoreContract` の `serving covers thumbnails and seek tiles`
- 原本 VOD の site の絞り込み: `TestMediaStoreContract` の `original VOD target is scoped to the recording's site`
- 追っかけの対象: `TestMediaStoreContract` の `chase target requires a record and a recording outside the trash`
- エンコード計画の観測と作り直し要求の照合: `TestMediaStoreContract` の `encode planning reads asset and request state through its contract`
- サムネイル再選択の候補の型付き値: `TestMediaStoreContract` の `thumbnail reselection candidates carry typed timeline and inputs`
- 名前空間の検査の受理と空の site 成分: `TestMediaStoreContract` の `namespace guard accepts site paths and rejects malformed ones`
- 接頭辞の無い原本と encoded: `TestMediaStoreContract` の `namespace guard rejects unprefixed originals and encoded files`
- サムネイルとシークタイルの除外: `TestMediaStoreContract` の `namespace guard leaves thumbnails and seek tiles to their own namespace`
- 既知パス一覧: `TestMediaStoreContract` の `audit lists every media asset path including reserved namespaces`
- 孤児候補の経過時間: `TestMediaStoreContract` の `orphan ageing compares against the database clock`
- 実体無し候補の経過時間と active でない行の除外: `TestMediaStoreContract` の `missing asset ageing compares against the database clock`
- 孤児を報告し回収する仕組みと、実体無しを検出する仕組み: [storage contract](../storage/contract.md) §5「残量の観測」 / [retention contract](../storage/retention.md) §7「孤児の報告と回収」「孤児回収の逆」
- `rel_path` の名前空間: [storage contract](../storage/contract.md) §5「rel_path の名前空間」

</details>

### メディア削除

削除の候補選びと状態遷移は store に集め、worker はファイル操作だけを持つ。

worker は `MediaDelete` から候補を読み、unlink の後に遷移を確定する。削除可否の述語（ごみ箱・until_encoded・外した encoded）は名前のまま実装の中に閉じ、契約はメソッド名と doc コメントで意味だけを示す。

遷移の時点で削除条件を再確認するのは 2 つのメソッドだけである。`MarkUntilEncodedDeleting` は、同じ録画への削除方針と作り直し要求の書き込みと直列化してから再確認する。候補を列挙した後に作り直し要求が commit されうるからである。`RevertToActive` も適用時に再確認し、`deleted` の行は戻さない。ごみ箱の猶予の cutoff は呼び出し側から受け取る（[時刻](#時刻) の例外）。

<details><summary>根拠</summary>

- 契約: `internal/store.MediaDelete`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- 作り直し要求との直列化と再確認: `TestMediaDeleteStoreContract` の `until-encoded transition waits for and rechecks recording writes`
- deleted の行を戻さないこと: `TestMediaDeleteStoreContract` の `revert does not restore a deleted asset`
- 外した encoded の削除要求が deleted 確定と同時に消えること: `TestMediaDeleteStoreContract` の `mark deleted clears the removal request`

</details>

### メディア公開

原本・encoded・thumbnail・seek tiles の公開は、1 つの tx を開いたまま、DB の判定、ファイルの公開、commit をこの順に行う。tx は公開のメソッドごとに store が持つ。

ファイルの公開は、呼び出し側が関数 `publish` として store に渡す。store は検査と行の書き込みをすべて終えた後、commit の前に `publish` を 1 回だけ呼ぶ。`publish` は rename と fsync を行う。検査で公開しないと決めたときは呼ばない。

`publish` がエラーを返したら、store は rollback してそのエラーを返す。`publish` が成功した後に commit が失敗したら、store はエラーを返す。このときファイルは行を持たない孤児として残る。呼び出し側はそのファイルを消さず、孤児の監査に回収を任せる。

呼び出し側は、rel_path のファイルロックを持って store を呼ぶ。store はファイルシステムを知らない。

`publish` は副作用を持つので、[ロック後の判断](#ロック後の判断)の「関数は DB に触れず状態を変えない」規則の例外である。DB の判定とファイルの公開の順序を固定するために、store が `publish` を呼ぶ。

先に浮かぶ案は、commit してから rename する順序である。この順序は、rename の失敗が行だけを残し、コミット = DB 行なのに実体が無い状態を作る。今の順序なら、commit の失敗は孤児ファイルを残すだけである。

原本の commit は、行の作成・進捗の削除・保持ポリシーの凍結を 1 つの tx で行う。同じ rel_path の原本が既にあれば `ErrConflict` を返し、`publish` は呼ばない。凍結する方針は、store が同じ tx で読んだ予約・上書き・意図を、呼び出し側の純粋な関数に渡して導く。設定に依存するクランプは、この関数の中にある。

encoded の公開は、試行番号・desired・作り直し要求・cut の区間を tx の中で読み直してから `publish` を呼ぶ。版の削除・事後追加と直列化し、cut はさらにチャプター編集・作り直し要求と直列化する。古い計画なら `ErrStalePublishPlan` を返す。

thumbnail と seek tiles の公開は、同じ録画のごみ箱移動・完全削除と直列化する。ごみ箱か削除済みの録画には公開しない。初回の公開は、active な行が既にあれば公開しない。thumbnail の置き換えは、観測した行（id・rel_path・seek）が変わっておらず、入力の原本が active なときだけ行う。

in-place 登録は、録画とアセットの行を 1 つの tx で書く。ファイルの検査とファイルロックは、呼び出し側の `internal/inplace` が持つ。同じ rel_path の行が既にあれば、その録画を再利用する。同じパスに deleted の行と active な行が並ぶときは、active な行を優先する。

<details><summary>根拠</summary>

- 契約: `internal/store.MediaPublish`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- 古い試行・外した desired・publish の回数: `TestMediaStoreContract` の `encoded publication rechecks attempt and desired profiles`
- publish の失敗で行が残らないこと: `TestMediaStoreContract` の `original callback failure rolls back rows` ほか
- publish が成功した後の commit 失敗: `TestMediaStoreContract` の `commit failure after publish returns an error and leaves no rows`
- rel_path の衝突: `TestMediaStoreContract` の `original publication rejects an occupied rel_path without publishing`
- ごみ箱と既存の行: `TestMediaStoreContract` の `thumbnail and seek tiles skip trashed recordings and existing assets`
- 公開がチャプター編集を待たせること: `TestMediaStoreContract` の `cut publication serializes with chapter edits`
- 公開がチャプター編集を待つこと: `TestMediaStoreContract` の `cut publication waits for an open chapter edit and rechecks`
- 版の削除との直列化: `TestMediaStoreContract` の `encoded publication waits for an open removal and rechecks`
- in-place の冪等性と active な行の優先: `TestMediaStoreContract` の `in-place registration is idempotent and prefers published rows`
- ingest の commit 失敗で canonical が孤児として残ること: `TestIngestWorker_CanonicalOrphanSurvivesDBDisconnect`
- encode の commit 失敗で古い試行が後の公開を上書きしないこと: `TestEncodeWorker_DisconnectedAttemptCannotOverwriteLaterPublication`
- ingest のファイルロックと store の統合: `TestIngestWorker_CommitHoldsRelPathFileLockThroughRename`
- encode のファイルロックと store の統合: `TestEncodeWorker_PublishHoldsRelPathFileLockThroughCommit`
- thumbnail のファイルロックと store の統合: `TestThumbnailWorkerPublishesUnderRelPathLock`
- seek tiles のファイルロックと store の統合: `TestSeekTilesWorkerPublishesUnderRelPathLock`
- in-place のファイル検証とファイルロック: `internal/inplace.Register`

</details>

### TS scan

TS の計測結果は、対象の状態を再確認した上で一括して保存する。

`TSScan` は計測対象の読み取りと計測結果の保存を担う。`ReplaceScan` は、原本がまだ active でサイズが計測時と同じかを再確認する。そのうえで、ドロップ統計・ドロップ位置・計測済みサイズ（marker）を 1 tx で置き換える。サイズが違えば何も書かない。読んでいる間に in-place 登録がファイルを差し替えうるからで、古い計測結果が残っても marker のサイズがずれたままなので、次の reconcile が計測をやり直す。途中の書き込みが失敗したときも、以前の計測結果と marker が残る。

<details><summary>根拠</summary>

- 契約: `internal/store.TSScan`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- 置き換えの原子性: `TestTSScanStoreContract` の `replaces statistics and marker together`
- 状態変化を待ってからの再確認: `TestTSScanStoreContract` の `rechecks active state after waiting for the asset lock`
- 失敗時に以前の結果が残ること: `TestTSScanStoreContract` の `rolls back statistics and marker after a failed replacement`

</details>

### 更新通知

更新通知（NOTIFY）は、データを変えたドメインのメソッドの中で送る。汎用の発行 interface は作らず、payload の文字列も契約に出さない。

方言ごとのクエリで書けば足りるので、発行側に interface を作る利益がない。

### ロック後の判断

ロック後の状態で再評価する判断を純粋な関数として契約の引数に渡すのは、判断が呼び出し側の状態（設定など）に依存するときだけである。

store は tx の中で、その関数をロック後の値に対して 1 回だけ呼ぶ。関数は DB に触れない。store は `config` を import しない。

設定に依存しない純粋な判定（`internal/chapters`・`internal/reservation` の関数など）は、pgstore が直接呼ぶ。

関数を引数にしても、tx を呼び出し側へ渡さない規則とは矛盾しない。関数は DB に触れず store の状態を変えないので、呼び出し側は書き込みを束ねられない。

Dex の updater 関数（`UpdateClient(id, func(old) (new, error))`）も同じ形である。

契約テストは、関数が呼ばれた回数と渡された値を数えて固定する。

### 直列化

直列化は、保証だけを契約に書き、機構は契約に書かない。

メソッドの doc コメントには、何と何を直列化するかを書き、機構（`FOR UPDATE`・advisory lock）は書かない。機構を書くと、別の実装が同じ保証を別の手段で満たせなくなる。

録画の行ロックは pgstore の共通ヘルパ（`lockRecordingState`、またはその上で消去済み・ごみ箱を弾く `lockRecording`）だけで取り、録画の行に触れる最初の文にする。ロックを取る前に読んだ古い値で判断すると、書き手どうしが互いの書き込みを観測できなくなるからである。

それより先に取るロック（局の advisory lock など）があれば、その順序をメソッドの doc コメントに書く。書き手ごとに取る順序が違うと、デッドロックしうるからである。

### チャプター編集

チャプター編集は、状態の読み取り、版を比較した置き換え、自動層へ戻す操作を一つのドメイン契約に置く。置き換えと自動層へ戻す操作は、tx の最初に録画の行をロックする。CM 検出の書き込みも同じ行をロックするので、両者は直列化される。

カット版の作り直し要求の記録と、カット版の公開も同じ行をロックする。作り直し要求に記録する keep と、公開時に照合する keep が、途中のチャプター編集で変わらないようにするためである。

読み取った版を置き換えるときに再確認することで、古い下書きが新しい内容を上書きしない。区間の重なりは、最後に区間の排他制約が拒否する。その違反は `ErrConflict` に写す。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.ChapterEdits`
- PostgreSQL 実装: `internal/store/pgstore.Analysis`
- 録画単位の直列化: `lockRecording` / `lockRecordingState` / `internal/store/pgstore.Analysis.SaveDetection`
- 作り直し要求とカット版の公開の直列化: `ReencodeRecordingProfile` / `prepareEncodePublish`
- 範囲制約: `recording_chapter_spans` の排他制約
- 導出値と事実: 不変条件 9
- 意味のある行: 不変条件 10
- 行の寿命: 不変条件 12
- ライブラリの公開契約: 不変条件 14

</details>

### エンコード方針と試行

エンコード方針の契約は、追加・削除・作り直し要求・保持と CM 検出の更新をそれぞれ自身の tx で確定する。プロファイル名の検証と cut の選択規則は API が所有し、評価は store がロック後の desired に対して行う。

cut の選択規則は設定の `live.enabled` に依存し、store は設定を知らない。そのため所有者は API で、規則を検証関数として store へ渡す。評価を store に置くのは、方針の行をロックした後の desired に当てるためである。ロック前に desired を読むと、版の削除が commit した後の集合を見落とす。

版の削除は録画の行から方針の行の順に直列化する。残りのコピーの検査は要求を書いた後の状態で行うので、同じ録画の削除が競合しても最後のコピーを失わない。削除と作り直し要求は、ごみ箱にある録画にも適用できる。keep は、録画の行をロックしてから確認済みチャプターを読んで導出する。ロックの前に読むと、チャプター編集が commit した後の区間を見落とす。

保持と CM 検出の方針は同じ tx で更新する。適用条件は UPDATE の条件が判定する。`until_encoded` には空でない desired が必要で、CM 検出の有効化には active な原本が必要である。どちらかが拒否されたら両方の変更を rollback する。

試行の `Begin` は、行が無ければ作り、あれば先に作った行をロックする。上限に達したかを判定し、停止した `running` の試行を失敗として数えてから、次の fencing token を記録する。ここまでを一つの tx で行う。`attempted_at` は DB の時計で付ける。取り消し時は、開始前の状態に戻す。上限は `EncodeAttempts` の契約引数で判定する。

追加と作り直し要求のジョブ投入は store の commit 後のヒントである。投入に失敗しても API の 204 応答を変えず、定期 reconcile が DB の desired から不足分を拾う。

<details><summary>根拠</summary>

- 方針と作り直し要求: `internal/store.EncodePolicy`
- 試行: `internal/store.EncodeAttempts`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- 契約テスト: `TestEncodePolicyContract`

</details>

### EPG 同期の tx の境界

EPG 同期の各段階は、独立した store メソッドにして、それぞれ自分で commit する。1 つの tx にまとめると、数千行の upsert が長い tx になる。途中の段階が失敗すると、すでに書いた段階まで巻き戻る。

段階は、サービスの upsert、番組の upsert、sweep の印、古い行の削除、放送済みの刈り込みである。各段階が確定していれば、次の同期が残りを書き直すだけで済む。

空のレスポンスのときに sweep しない判定は、mirakc の応答を知っている worker に残す。store は、印（sweep mark）より古い行を、渡された site・サービスの範囲で消すだけにする。

<details><summary>根拠</summary>

- 同期の契約: `internal/store.EPGSync`

</details>

### 正規表現の構文

正規表現の構文は実装に任せ、契約で揃えない。正規表現エンジンごとに、受け付ける構文が違う。構文まで契約に含めると、別のエンジンを使う 2 つ目の実装が同じ契約を満たせなくなる。

契約で揃えるのは、キーワードの全角・半角の正規化と、大文字小文字を区別するかどうかの指定である。不正なパターンは `ErrInvalidPattern` に写す。

<details><summary>根拠</summary>

- 検索とルール評価の契約: `internal/store.ProgramSearch`
- 番組検索のキーワードのテスト: `RunEPG` の `keyword search normalizes width and respects case sensitivity`
- ルールの子表検索テスト: `RunEPG` の `rule matching applies every child-table condition`
- 正規表現検証テスト: `RunEPG` の `regex validation distinguishes syntax errors`

</details>

### CM 検出（局単位の状態）

ロゴ・枠・候補は局を単位とする状態である。同じ局について、これらを書くメソッド（枠の保存・削除、ロゴの削除、候補の削除・採用・開始）は、互いに直列化される。CM 検出の結果の保存とロゴの学習の保存も、同じ局の操作と直列化される。例外は候補の ready・失敗・破棄の 3 つで、直列化されず、版の照合で守る。

結果を保存する操作は、前提にした状態が書き込み時点でも同じかを検査し、違えば書かない。照合する版は操作ごとに違う。ロゴの学習の保存は枠の `updated_at` を、CM 検出の結果の保存はロゴの `learned_at` を照合する。

局の状態は別々の表にあり、初回の書き込みより前にはロックできる行がない。そのため、直列化は局（`network_id`, `service_id`）を単位にする。PostgreSQL 実装は advisory lock を使うが、契約が求めるのは同じ局の操作が互いに直列化されることだけである。

候補を ready にする書き込みは、候補行の `attempted_at` と、候補が読んだ枠の `updated_at` を照合する。現在の枠の `updated_at` が変わっていれば書かない。失敗と破棄は現在の枠を見ない。候補行の `attempted_at` と `observed_area_updated_at`（候補が読んだ枠の版）が渡された 2 値と一致する running 候補だけを対象にする。

候補の採用は、ロゴの更新、候補の削除、`redetect` を指定されたときの検出結果の削除を 1 つの tx で行う。枠の保存と候補の削除、枠の削除と採用の段の試行の削除も、それぞれ原子的に行う。

`DeleteLogo` は投入先（候補解析か CM 検出の reconcile か）を戻り値で返し、呼び出し側が commit 後にそこへ投入する。投入先が tx の中の読み（枠の有無、解析に使える録画の有無）で決まるからである。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.CMStation`
- PostgreSQL 実装: `internal/store/pgstore.Analysis`
- 契約テスト: `internal/store/contracttest.RunCMStation`
- 局単位の advisory lock: `LockCMStation`
- 候補結果の版照合: `MarkCMLogoCandidateReady` / `MarkCMLogoCandidateFailure`

</details>

### CM 検出（試行と結果）

結果の保存は、保存の時点で現在の試行・desired・局ロゴの版を確かめ直す。ロゴの版を照合するのは、解析をロゴありで始めた場合だけである。

結果の保存は、同じ局のロゴ・枠の操作と、同じ録画のチャプター編集の両方と直列化される。ロックは局、録画、試行の順に取る。録画単位の直列化により、検出結果の書き込みとチャプター編集の自動チャプター引き取りは、互いの commit を見落とさない。

再試行 API は結果と試行を消し、commit してからジョブを投入する。ingest と reconcile の投入も含め、どれも commit 後のヒントである。投入に失敗しても、定期 reconcile が desired な集合を取り直して投入する。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.CMDetection`
- PostgreSQL 実装: `internal/store/pgstore.Analysis`
- 契約テスト: `internal/store/contracttest.RunCMDetection`
- チャプター編集との録画単位の直列化: `lockRecordingState`
- レベル駆動の回復: 不変条件 5

</details>

### 分類ルール

分類ルールの API と全件の再評価は、別々の契約に分けて使う。

全件の再評価は、当たりの差分を原子的に適用する。途中で commit すると、一部の録画だけが新しい棚に入った状態が読み手に見えるからである。当たりが変わったときだけ、録画のトピックへ 1 回通知する。

全件の再評価どうしは直列化される。後から始まった再評価は、先に commit されたルールの集合で評価する。

録画の書き込みで分類の当たりを保つ責任は、録画のドメインが負う。PostgreSQL では、録画の INSERT とタイトル変更のトリガーがこれを行う。ルールを変更した後の全件の再評価は、既に commit 済みの録画を新しいルールに追従させる。ルールの編集をまたいで commit した録画は、定期の再評価が追従させる。

ルール値のキーは `SeriesKey` で取得する。`label_rules.value_key` の生成列と同じ DB 関数を使い、正規化の規則を Go に複製して片方だけ変わるのを防ぐ。

<details><summary>根拠</summary>

- API の CRUD と値キー: `internal/store.LabelRules`
- 全件の再評価と直列化: `internal/store.LabelRuleReevaluation`
- PostgreSQL 実装: `internal/store/pgstore.LabelRules`
- 録画の書き込みによる当たりの維持: `recordings_label_rule_sync_insert` / `recordings_label_rule_sync_update` トリガー
- 値キーの生成列: `label_rules.value_key`

</details>

### schedule 同期

観測行の置き換えと snapshot marker の更新は、1 つのトランザクションで確定する。

reconciler は、観測全量の置き換え、同期評価の候補、開始済みイベントの読み取りを `ScheduleSync` から使う。presync collector は、marker・観測一覧・同期評価の候補を `ScheduleSyncReader` から読む。実装は `pgstore.ScheduleSync` が両方を持つ。

置き換えは、観測行の upsert、古い観測行の削除、marker の更新を 1 つの tx で行う。marker を先に確定すると、collector が不完全な観測を新鮮と誤認する。

古い観測行を判定する時刻（sweep の印）は DB の時計で付け、呼び出し側は時刻を渡さない。

marker が無いときは、store がエラーと区別して返す。collector は、そのときだけ時刻を 0 として出す。

<details><summary>根拠</summary>

- reconciler の契約: `internal/store.ScheduleSync`
- presync の読み取り契約: `internal/store.ScheduleSyncReader`
- PostgreSQL 実装: `internal/store/pgstore.ScheduleSync`
- 全量置き換えの原子性: `TestScheduleSyncContract/failed_replacement_preserves_prior_observations_and_marker`
- marker の失敗による巻き戻し: `TestScheduleSyncContract/marker_failure_rolls_back_observations`

</details>

### never-scheduled 観測

欠測の記録は reconciler のドメインに置く。書き手が reconciler（mirakc の観測）だからである。

古い行の削除は、ruler の GC が同じ契約 `NeverScheduledEvents` を呼んで行う。実装は `pgstore.NeverScheduledEvents` が持つ。

記録は放送イベントキーで冪等に行い、新しい行を作ったときだけ true を返す。

削除は幅（horizon）だけを受け取る。`observed_at` を、DB の時計の now() から horizon を引いた時刻と比べる。`observed_at` が DB の時計で記録されているからである。

<details><summary>根拠</summary>

- 契約: `internal/store.NeverScheduledEvents`
- PostgreSQL 実装: `internal/store/pgstore.NeverScheduledEvents`
- 冪等記録と horizon による削除: `TestNeverScheduledEventsContract`
- 削除の呼び出し元: `internal/ruler/ruler.go` の GC

</details>

### ruler パス

ルールから予約を導出する処理は、読み取りを tx の外で行い、書き込みを 1 tx にまとめる。削除判断に必要な状態は適用時に確かめ直し、ブレーカー発動と同じ tx で確定する。

ruler は `RulerPass` から評価に必要なルール・意図・投資・予約・射影・履歴を読み、1 サイト分の書き込みを `ApplyPass` に渡す。`ApplyPass` は、スナップショットの追従、予約の差分 upsert、fulfilled / released / 導出削除を 1 つの tx で確定する。

読み取りの後に、削除候補の番組へ意図や投資が追加されることがある。導出削除と released 削除の `DELETE` は適用時に投資を再評価するため、古い入力で新しい予約意図の行を消さない。契約テストは入力を読んだ後に意図を書き、同じ予約が残ることを固定する。

大量削除ブレーカーの Trip も `ApplyPass` に含める。Trip が止める削除と発動行を別 tx にすると、発動を記録できないまま予約を消しうるためである。発動記録に失敗した tx は削除も巻き戻す。メトリクスとログは commit 後に反映する。

重複排除の類似度は pg_trgm の `similarity()` と同じ値で判定する。これは実装依存ではなく契約である。候補ごとに最良一致を 1 件返し、同率は録画 ID 昇順で決める。マッチの無い候補はマップに現れない。時間窓は録画の `program_start_at`（放送時刻）と比べる。比べる相手が store の記録した時刻ではないので、呼び出し側の時刻を受け取る（「時刻」節の規則）。番組 ID 再利用の検出と snapshot GC も、それぞれ比較に使う時刻を呼び出し側から受け取る。

<details><summary>根拠</summary>

- 契約: `internal/store.RulerPass`
- PostgreSQL 実装: `internal/store/pgstore.Reservations`
- 削除時の意図再評価、ブレーカーとの原子性、重複排除の類似度: `TestRulerPassContract`
- ruler の成功 marker と record_sweep marker: `internal/store.LoopMarkers` / `TestLoopMarkersContract`

</details>

### サーキットブレーカー

発動は行の存在で表し、再開は行の削除で表す。発動していない状態を表す行は作らない。

reconciler と削除 reconcile は `Breakers`、API は `BreakerAdmin` を使う。`Breakers` は `Trip` と `IsTripped` だけを持つ。`Trip` は発動時刻を返し、すでに発動中なら件数と抜粋だけを更新する。`Resume` は、対象の行が無ければ `ErrNotFound` を返す。

reconciler と削除 reconcile は、もともと tx の外で発動する。そのため、`Breakers.Trip` を単独で呼ぶ。ruler は `RulerPass.ApplyPass` の中で発動する（理由は「ruler パス」節）。

ゲージとログは store の外で出す。`breaker.Trip` と、ruler が commit 後に呼ぶ `breaker.AnnounceTrip` が同じ `announceTrip` を共有しているので、出し方は 1 か所にある。パスの先頭の `breaker.ObserveState` は、プロセス再起動後も DB の発動状態をゲージへ戻す。

<details><summary>根拠</summary>

- worker の発動・状態契約: `internal/store.Breakers`
- API の一覧・再開契約: `internal/store.BreakerAdmin`
- PostgreSQL 実装: `internal/store/pgstore.Breakers`
- ゲージとログ: `internal/breaker.ObserveState` / `internal/breaker.Trip`
- 発動時刻と再開後の状態: `TestBreakersContract`
- ruler の tx 内の発動: `internal/store/pgstore.Reservations.ApplyPass`
- ruler の発動ゲージ: `TestRunPass_TripInApplyPassRaisesCircuitBreakerGauge`

</details>
