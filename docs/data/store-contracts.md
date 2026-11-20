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

### 述語の置き場

DB で評価しなくてよい述語は Go に置く。DB で評価する述語は、それを使う store のメソッドの数で置き場を決める。方言の間の一致は契約テストで守る。

次の 2 つの問いで決める。

1. その述語を使う読み手のうち、1 つでも DB で評価する必要があるか。集合の絞り込み・件数・ページング・ロックを取った同じ文の中での判定が該当する。必要が無ければ Go に置く。各方言の store 実装が同じ関数を呼ぶので、実装は全方言で 1 つで済む。呼び方は [ロック後の判断](#ロック後の判断) と同じである。
2. DB で評価するなら、その述語を使う store のメソッドが 2 つ以上あるか。1 つならそのメソッドのクエリに直接書く。2 つ以上なら、方言ごとに 1 つの定義から引く。定義の形は方言が選ぶ。Postgres は view や関数に置く。MySQL には表を返す関数も配列引数も無いので、view や SQL 断片の定数などに置く。

Go に置くと、DB で評価する読み手には述語が届かない。cut の keep がその例である。reconcile は `internal/chapters` で keep を導出し、空の keep は投入しない。待機列は SQL だけで数えるので、keep が空の cut を数え続ける。この差は [エンコード待機列](jobs.md#エンコード待機列) で受け入れている。

SQL で評価する述語は、view に置いてもクエリに直接書いても方言の数だけ実装される。view が防ぐのは、方言の中で片方のクエリにだけ条件を書き足す失敗である。方言の間の一致は守らない。

一致は契約テストで守る。述語を共有するメソッドの組が同じ集合を返すことは、`internal/store/contracttest` の `Run*` 関数で確かめ、各方言の `*_contract_test.go` から回す。前例は、同じ絞り込み条件で `Shelves` の件数と `List` の行数が一致するかを見る `runRecordingQueryShelves` である。

`internal/api` や `internal/worker` のテストに置くと、Postgres でしか回らない。

<details><summary>根拠</summary>

- Go に置く前例（keep の導出）: `internal/chapters.KeepRanges`
- Postgres の view: `encodable_recordings` と `unencoded_desired_profiles`（`internal/db/schema.sql`）
- Postgres の関数: `encodable_profile` と `awaiting_review_profiles`（`internal/db/schema.sql`）
- 一致の前例（Shelves の件数と List の行数）: `runRecordingQueryShelves`（`internal/store/contracttest/recordingquery.go`）。`RunRecordingQuery` から呼ばれ、`internal/store/pgstore/recordingquery_contract_test.go` が回す

</details>

### 時刻

時刻は、比べる相手を書いた時計に合わせる。

比べる相手が store の記録した時刻（`updated_at`・`attempted_at`・`observed_at` など）なら、DB の時計で比べる。この場合の契約は、時刻ではなく幅（`time.Duration`）を受け取る。比べる相手が外から来た時刻（EPG の放送時刻など）なら、契約は呼び出し側の `time.Time` を受け取る。

store が記録する時刻は DB の時計で付け、呼び出し側の時計を使わない。api と worker は別ホストなので、時計がずれると、書き手をまたぐ時刻の大小比較が反転しうる。

ごみ箱の猶予だけが例外である。api が DB の時計で付けた `deleted_at` を、worker の時計の cutoff と比べている。猶予が日単位でずれが効かないので、今の形のままにする。

<details><summary>根拠</summary>

- DB の時計で比べる前例（GC）: `internal/db/queries/reservations.sql` の `DeleteStaleNeverScheduledEvents`。`observed_at < now() - horizon` で比べ、`PruneNeverScheduled` が呼ぶ
- DB の時計で比べる前例（孤児候補）: `internal/store/pgstore.Analysis.FailOrphanLogoCandidates`。`ListRunningCMLogoCandidates` が返す DB の `now()` を使う
- 例外: `internal/storagemaint/delete_reconcile.go` の `trashCutoff` と `recordings_trash.sql` の `deleted_at = now()`

</details>

### 視聴状態

視聴状態の書き込みは 1 つの契約にまとめる。再生位置は録画ごとに保存する。視聴済みの印は、押した録画の行にだけ置く。

`MarkWatched` は印の upsert と再生位置の削除を 1 メソッドにする。2 つは「視聴済みにする」という同じ業務操作であり、呼び出し側が 2 回に分けて呼ぶと、印だけ付いて再生位置が残った状態で止まりうる。

<details><summary>根拠</summary>

- 契約: `internal/store.PlaybackState`
- PostgreSQL 実装: `internal/store/pgstore.Playback`

</details>

### ごみ箱と完全削除

ごみ箱の操作は、利用側ごとに 2 つの契約に分ける。api が使う操作と、削除 reconcile が使う操作が違うからである。

api が使うのは論理削除・復元・即時削除要求をまとめた `RecordingTrash` で、削除 reconcile が使うのは完全削除の確定だけの `RecordingPurge` である。実装は録画のドメインの `pgstore.Recordings` が両方を持つ。

同じ録画の復元と即時削除要求を直列化しないと、復元された録画に即時削除要求だけが残りうる。要求だけが残ると、次の普通の論理削除でごみ箱の猶予を飛ばして即時削除の対象になるが、ユーザーは即時削除を要求していないからである。

仕組みは [削除エンジン](../storage/retention.md) §7「復元と即時削除要求の競合」にある。

削除通知の発火対象は `RecordingPurge.MarkPurged` の返り値で決め、削除 reconcile は別の判定で作り直さない。

<details><summary>根拠</summary>

- 契約: `internal/store.RecordingTrash` / `internal/store.RecordingPurge`
- PostgreSQL 実装: `internal/store/pgstore.Recordings`
- 削除述語と `purged_at` の意味: [削除エンジン](../storage/retention.md) §7

</details>

### チューナー・EPG の同期と容量

外から取り込む同期（チューナーと EPG）は、同期全体を 1 つの tx にまとめない。

チューナー同期は、sweep の印の取得、全量 upsert、古い行の削除を別々の store メソッドで行う。印の取得と upsert の間に mirakc への問い合わせが入るためである。

EPG 同期の各段階は、独立した store メソッドにして、それぞれ自分で commit する。1 つの tx にまとめると、数千行の upsert が長い tx になる。途中の段階が失敗すると、すでに書いた段階まで巻き戻る。各段階が確定していれば、次の同期が残りを書き直すだけで済む。

空のレスポンスのときに sweep しない判定は、mirakc の応答を知っている worker に残す。

容量の読み取りは解決を store に持たせない。effective options の解決と Hall 条件の判定は `internal/capacity` が行う。

保存前プレビューは、対象ルールの状態の読み取りと候補の読み取りを別のメソッドにする。ルールはリクエストの時点で引けるが、候補は番組検索の後でしか引けないためである。1 つにまとめると、存在しないルールの 404 が検索の後ろへずれる。

<details><summary>根拠</summary>

- チューナー同期と一覧: `internal/store.TunerSync` / `TunerReader`
- 容量読み取り: `internal/store.Capacity`
- PostgreSQL 実装: `internal/store/pgstore.Tuners`
- 需要の区間の終了端: `TestTunersContract` の `capacity reads preserve interval endpoints`
- 容量需要と候補の抽出: `ListCapacityDemand` / `ListCapacityDemandAllSites` / `ListCapacityPreviewCandidates`
- EPG 同期の契約: `internal/store.EPGSync`

</details>

### 録画の観測

watcher は mirakc の record を観測するたびに、record の同期行・録画・品質イベントを 1 つのメソッドで原子的に書く。ingest の投入は、その commit の後に送るヒントにする。

契約は利用側ごとに 3 つに割る。観測を書く `RecordObservations`、ingest worker が転送先を読む `IngestTarget`、metrics が未取り込みの滞留を読む `RecordBacklog` である。実装は録画のドメインの `pgstore.Recordings` が全部持つ。

ingest をヒントにできるのは、取りこぼしを sweep が拾い直すからである。mirakc の record は ingest の DB commit 後にしか消えず、定期の record sweep は録画中・完了の record を再処理するたびに ingest を投入し直す。

<details><summary>根拠</summary>

- 契約: `internal/store.RecordObservations` / `IngestTarget` / `RecordBacklog`
- PostgreSQL 実装: `internal/store/pgstore.Recordings`
- title の分類（作成時と更新時）: `recordings_label_rule_sync_insert` と `recordings_label_rule_sync_update` の trigger。検査: `TestRecordingsLabelRuleTrigger_FollowsTitleUpdates`
- commit 後のヒント（commit の後に投入し、投入の失敗で観測を失敗させない）: `TestProcessRecord_EnqueueFollowsCommit`
- 上記以外の挙動: `TestRecordObservationsContract` の各ケース

</details>

### 録画の検索

録画一覧とシリーズ棚では検索条件を共有し、API の状態は取得した事実から導出する。

録画一覧・単体取得・棚は `RecordingQuery` に置く。フィルタは OpenAPI の生成型を使わない `RecordingFilter` とし、HTTP パラメータの検証と変換は api が行う。

SQL の組み立ては pgstore に置く。ジャンルの配列の重なり、行値の比較（カーソルと service の組）、キーワードの述語は方言の機能なので、契約は意味だけを持ち、方言は実装に置く。

一覧と棚は、共通する検索条件の WHERE を同じ関数で組み立てる。複製すると片方だけ条件が増え、絞り込み結果がずれる。

録画の検索は EPG に結合しない。理由は [検索](search.md) §5「録画検索は rulequery を共有しない」にある。共有するのはキーワードの正規化方言だけである。述語 `keywordClause` は pgstore の番組検索と録画検索が呼び、条件のコンパイラと列名の対応は共有しない。

API のレスポンス状態は `recordingFromListFields` で毎回導出する。導出済みの状態を SQL に焼くと、導出規則を API と DB の両方に持つことになる。

一覧と棚は、汎用プランへの切り替えを避ける実行方式で毎回計画させる。これは pgstore の実装上の制約で、契約には現れない。理由と測定値は [REST の判断](../api/rest.md) の「プリペアド文キャッシュと再計画」にある。

<details><summary>根拠</summary>

- 契約: `internal/store.RecordingQuery` / `RecordingFilter`
- PostgreSQL 実装: `internal/store/pgstore.Recordings`
- 一覧と棚の共通条件: `recordingsFilterWhere`
- キーワードの述語: `keywordClause`

</details>

### 録画ルールと予約の意図

予約の表を書くのは ruler と、ルールの削除だけである。利用者の録画意図・上書きを確定する契約は予約の表を書かず、それ自身の tx で確定する。ルールの作成・更新・削除と EPGStation からの取り込みも、それぞれ 1 つの tx で確定する。

ルールの削除が予約の表も書くのは、意図の無い予約の削除とルールの削除を別の tx にすると、間で切れたときにルールの無い予約が残るからである。

正規表現の構文検査は `ProgramSearch.ValidateRegex` を使う。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.Rules` / `internal/store.RuleImport`
- 録画意図・上書きの契約: `internal/store.Reservations`
- マージの設定と reset: `TestMergeOverrides_SetsOnlyGivenFields` / `TestMergeOverrides_ResetClearsEachField`
- reset の対象が Options と一致すること: `TestOverrideFieldsMatchOptions`
- PostgreSQL 実装: `internal/store/pgstore.Rules`

</details>

### メディア読み取りとストレージ観測

メディアとストレージの読み取りは、利用側ごとの契約に分ける。DB の行型を利用側へ広げないためである。

streamer のハンドラは `MediaServing` を受け取る。worker のジョブは `MediaReconcile` と `StorageObservation` を、API のハンドラは `MediaReader` と `StorageObservation` を受け取る。worker は、エンコード計画の比較材料と再計算の候補を `MediaReconcile` から読む。置き場単位の観測と孤児・実体無しの候補は `StorageObservation` に記録する。API は同じ interface から観測値を読む。api ロールと worker ロールの起動時の検査も `MediaReconcile` を使う。実装はどれも `pgstore.Media` が持つ。streamer や API が SQL の行型を知ると、配信してよい行や読み取りの条件が利用側に散らばる。

エンコード worker は、現在の keep 区間・録画時刻・通知対象を `MediaReconcile` から読む。通知対象に purged の録画を含めるのは、エンコードの完了・失敗を webhook で通知するとき、purged 後でも局名と番組名を載せるためである。

チャプターから keep を作る導出は、store に 1 つだけ置く。エンコード計画、公開時の再確認、作り直し要求（`RequestCutRebuild`）が同じ導出を使う。写しが複数あると、一部だけ直ったときに 3 つの判定がずれる。

ingest の転送前確認は `IngestPrecheck` に分ける。転送前確認が 'deleting' の行も衝突に数えるのは、`state = 'active'` に絞ると、'deleting' の行が使っている `rel_path` を ingest が上書きするためである。その結果、「DB は active、実体は別番組」の状態が再び生まれる。転送前に衝突を見落としても、`CommitOriginal` が `ErrConflict` で拒む。

起動時の検査は 2 つある。1 つは profile の意味の検査で、api ロールと worker ロールの両方が走らせる。もう 1 つは `rel_path` の名前空間の検査で、worker ロールだけが走らせる。違反を見つけたら起動を拒む。検査の対象は [`storage.stores` の FS 置き場](../configuration/keys.md#置き場の規則)の原本と encoded である。

孤児の判定に使う既知パスの一覧から、接頭辞・予約名前空間・削除途中のコピーを外すと、次の失敗が起きる。接頭辞を落とすと全ての原本が孤児候補になる。`thumbnails/` を除くとサムネイルが孤児として記録される。削除途中のファイルを外すと、削除エンジンが消している途中のファイルが孤児候補になる。

経過時間の比較を DB の時計に任せる。契約は時刻ではなく `time.Duration` を受け取るので、worker の時計がずれても、公開前の名前の孤児を自動削除する時期（`agedUnpublishedOrphans`）と、孤児・実体無しを報告する時期は変わらない。一般則は [時刻](#時刻) にある。

<details><summary>根拠</summary>

- 契約: `internal/store.MediaServing` / `MediaReconcile` / `IngestPrecheck` / `MediaReader` / `StorageObservation`
- エンコード進捗の契約: `internal/store.EncodeProgressReporter`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- encode queue が除く profile と `Running`: `TestMediaStoreContract` の `encode queue filters desired profiles and attempts`
- ドロップ統計の録画での絞り込み: `TestMediaStoreContract` の `drop stats are scoped to the target recording`
- ドロップ位置の録画での絞り込み: `TestMediaStoreContract` の `drop positions are scoped to the target recording`
- 送ったエンコード進捗が、同じ値のまま通知で届くこと: `TestMediaStoreContract` の `encode progress reaches the listener`
- 録画 HLS の対象: `TestRecordingHLSTargetPreservesCommittedGeneration` と `TestRecordingHLSTargetUsesIngestTempUntilCommit`
- エンコード計画の観測と作り直し要求の照合: `TestMediaStoreContract` の `encode planning reads asset and request state through its contract`
- 所有状態と区間が同じ時点の読み取りであること: `pgstore` の `TestGetCurrentCutKeepReadsOwnershipAndSpansFromOneSnapshot`
- encode 締切用の nullable 時刻: `TestMediaStoreContract` の `recording encode times preserve nullable timestamps`
- サムネイル再選択の候補の型付き値: `TestMediaStoreContract` の `thumbnail reselection candidates carry typed timeline and inputs`
- 孤児を報告し回収する仕組みと、実体無しを検出する仕組み: [storage contract](../storage/contract.md) §5「残量の観測」 / [retention contract](../storage/retention.md) §7「孤児の報告と回収」 / [missing-assets](../storage/missing-assets.md)
- `rel_path` の名前空間: [storage contract](../storage/contract.md) §5「rel_path の名前空間」

</details>

### メディア削除

削除の候補選びと状態遷移は store に集め、worker はファイル操作だけを持つ。

worker は `MediaDelete` から候補を読み、unlink の後に遷移を確定する。削除可否の述語（ごみ箱・until_encoded・外した encoded）は名前のまま実装の中に閉じ、契約はメソッド名と doc コメントで意味だけを示す。

遷移の時点で削除条件を再確認するのは `MarkUntilEncodedDeleting` と `RevertToActive` だけである。ごみ箱の猶予の cutoff は呼び出し側から受け取る（[時刻](#時刻) の例外）。

原本の規則・FS 置き場・派生物の置き場は、server の起動時に `StoragePolicy` にし、store の構築時に束ねる。設定で決まる値をメソッドの引数で渡すと、構築時に束ねた方針と異なる方針を渡せてしまう。JSON にするのは pgstore である。

`PrepareMediaAssetTransfer` の `fsStores` は、パスごとにマーカーを検査して決まる実行時のマウント集合なので引数に残す。設定にある FS 置き場をすべて渡すと、マウントしていない置き場の派生物コピーを転送元に選べてしまう。

配置削除は転送先が 2 つ以上あるときだけ有効にする。無効なら `PlacementPolicy` はゼロ値を返し、desired から外れたコピーも削除候補にならない。

<details><summary>根拠</summary>

- 契約: `internal/store.MediaDelete`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- マウント集合の固定: `TestPrepareMediaAssetTransferKeepsDerivedSourcesMounted`
- 配置削除が無効なとき: `TestStorageTransferDeletionSkipsWhenPlacementArmIsDisabled`
- 配置削除が有効なとき: `TestStorageTransferDeletionWiresOverlapCutoff`

</details>

### メディア公開

原本・encoded・thumbnail・seek tiles の公開は、1 つの tx を開いたまま、DB の判定、ファイルの公開、commit をこの順に行う。

公開物を作って永続化する処理は、呼び出し側が関数 `publish` として store に渡す。

`publish` は副作用を持つので、[ロック後の判断](#ロック後の判断)の「関数は DB に触れず状態を変えない」規則の例外である。DB の判定とファイルの公開の順序を固定するために、store が `publish` を呼ぶ。

先に浮かぶ案は、commit してから rename する順序である。この順序は、rename の失敗が行だけを残し、コミット = DB 行なのに実体が無い状態を作る。今の順序なら、commit の失敗は孤児ファイルを残すだけである。

store はファイルシステムと設定を知らない。in-place 登録のファイルの検査は `internal/inplace` が登録前に行い、設定に依存するクランプは凍結方針を導く呼び出し側の関数が持つ。

<details><summary>根拠</summary>

- 契約: `internal/store.MediaPublish`
- PostgreSQL 実装: `internal/store/pgstore.Media`
- ingest の commit 失敗で canonical が孤児として残ること: `TestIngestWorker_CanonicalOrphanSurvivesDBDisconnect`
- encode の commit 失敗で古い試行が後の公開を上書きしないこと: `TestEncodeWorker_DisconnectedAttemptCannotOverwriteLaterPublication`
- in-place 登録で通常ファイル以外を拒否すること: `TestRegister_RequiresExistingRegularFile`

</details>

### TS scan

TS の計測結果は、対象の状態を再確認した上で一括して保存する。

サイズを見るのは、読んでいる間に in-place 登録がファイルを差し替えうるからである。古い計測結果が残っても計測サイズがずれたままなので、次の reconcile が計測をやり直す。

<details><summary>根拠</summary>

- 契約: `internal/store.TSScan`
- PostgreSQL 実装: `internal/store/pgstore.Media`

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

### エンコード方針と試行

プロファイル名の検証と cut の選択規則は API が所有し、評価は store がロック後の desired に対して行う。

cut の選択規則は設定の `recording_hls.enabled` に依存し、store は設定を知らない。そのため所有者は API で、規則を検証関数として store へ渡す。評価を store に置くのは、方針の行をロックした後の desired に当てるためである。ロック前に desired を読むと、版の削除が commit した後の集合を見落とす。

作り直し要求の keep を録画の行のロックの後に導くのは、ロックの前に読むと、チャプター編集が commit した後の区間を見落とすからである。

追加と作り直し要求のジョブ投入は store の commit 後のヒントである。投入に失敗しても API の 204 応答を変えず、定期 reconcile が DB の desired から不足分を拾う。

<details><summary>根拠</summary>

- 方針と作り直し要求: `internal/store.EncodePolicy`
- 試行: `internal/store.EncodeAttempts`
- PostgreSQL 実装: `internal/store/pgstore.Media`

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

局単位の状態（ロゴ・枠・候補）を書く操作は局ごとに直列化し、候補の結果を書く操作だけは版の照合で守る。

局の状態は別々の表にあり、初回の書き込みより前にはロックできる行がない。そのため、直列化は局（`network_id`, `service_id`）を単位にする。契約が求めるのは、同じ局の操作が互いに直列化されることだけで、実現の手段は実装が選ぶ。

`DeleteLogo` が投入先（候補解析か CM 検出の reconcile か）を返し、呼び出し側が commit 後に投入するのは、投入先がロゴの削除と同じトランザクションの中の読みで決まるからである。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.CMStation`
- PostgreSQL 実装: `internal/store/pgstore.Analysis`
- 契約テスト: `internal/store/contracttest.RunCMStation`
- 局単位の advisory lock: `LockCMStation`
- 候補結果の版照合: `MarkCMLogoCandidateReady` / `MarkCMLogoCandidateFailure`

</details>

### CM 検出（試行と結果）とチャプター編集

CM 検出の結果とチャプター編集は、カット版の作り直し要求と公開を含めて録画単位で直列化する。CM 検出の結果は前提を保存の時点で確かめ直してから書き、ジョブの投入はすべて commit 後のヒントにする。

直列化により、検出結果の書き込みとチャプター編集の自動チャプター引き取りは、互いの commit を見落とさない。

再試行 API は結果と試行を消し、commit してからジョブを投入する。ingest と reconcile の投入も含め、どれも commit 後のヒントである。投入に失敗しても、定期 reconcile が desired な集合を取り直して投入する。

カット版の作り直し要求の記録と公開をチャプター編集と直列化するのは、作り直し要求に記録する keep と、公開時に照合する keep が、途中のチャプター編集で変わらないようにするためである。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.CMDetection`
- ドメイン契約（チャプター編集）: `internal/store.ChapterEdits`
- PostgreSQL 実装: `internal/store/pgstore.Analysis`
- 契約テスト: `internal/store/contracttest.RunCMDetection`
- チャプター編集との録画単位の直列化: `lockRecording` / `lockRecordingState` / `internal/store/pgstore.Analysis.SaveDetection`
- 作り直し要求とカット版の公開の直列化: `ReencodeRecordingProfile` / `prepareEncodePublish`
- 範囲制約: `recording_chapter_spans` の排他制約
- 導出値と事実: 不変条件 9
- 意味のある行: 不変条件 10
- 行の寿命: 不変条件 12
- ライブラリの公開契約: 不変条件 14
- レベル駆動の回復: 不変条件 5

</details>

### 分類ルール

分類ルールの API と全件の再評価は、別々の契約に分けて使う。

全件の再評価が差分を原子的に適用するのは、途中で commit すると、一部の録画だけが新しい棚に入った状態が読み手に見えるからである。

録画の書き込みで分類の当たりを保つ責任は、録画のドメインが負う。PostgreSQL では、録画の INSERT とタイトル変更のトリガーがこれを行う。ルールの編集をまたいで commit した録画は、定期の再評価が追従させる。

ルール値のキーの正規化を Go に複製せず DB 側の 1 箇所に置くのは、`label_rules.value_key` の生成列と片方だけ変わるのを防ぐためである。

<details><summary>根拠</summary>

- API の CRUD と値キー: `internal/store.LabelRules`
- 全件の再評価と直列化: `internal/store.LabelRuleReevaluation`
- PostgreSQL 実装: `internal/store/pgstore.LabelRules`
- 録画の書き込みによる当たりの維持: `recordings_label_rule_sync_insert` / `recordings_label_rule_sync_update` トリガー
- 値キーの生成列: `label_rules.value_key`

</details>

### schedule 同期と欠測の観測

mirakc から得た予定と欠測の観測は、書き手である reconciler のドメインの契約に置く。

観測行の置き換えと snapshot marker の更新は、1 つのトランザクションで確定する。marker を観測より先に確定すると、collector が不完全な観測を新鮮と誤認するからである。

reconciler は、観測全量の置き換え、同期評価の候補、開始済みイベントの読み取りを `ScheduleSync` から使う。presync collector は、marker・観測一覧・同期評価の候補を `ScheduleSyncReader` から読み、marker が無いときだけ時刻を 0 として出す。実装は `pgstore.ScheduleSync` が両方を持つ。

古い行の削除は、ruler の GC が同じ契約 `NeverScheduledEvents` を呼んで行う。実装は `pgstore.NeverScheduledEvents` が持つ。

削除は幅（horizon）だけを受け取り、基準時刻は受け取らない。`observed_at` は DB の時計で記録されるので、呼び出し側の時計で基準時刻を渡すと、比べる 2 つの時計がずれる。

<details><summary>根拠</summary>

- reconciler の契約（予定の観測の置き換え）: `internal/store.ScheduleSync`
- presync の読み取り契約: `internal/store.ScheduleSyncReader`
- PostgreSQL 実装（予定の観測）: `internal/store/pgstore.ScheduleSync`
- 欠測の記録と削除の契約: `internal/store.NeverScheduledEvents`
- 欠測の PostgreSQL 実装: `internal/store/pgstore.NeverScheduledEvents`
- 削除の呼び出し元: `internal/ruler/ruler.go` の GC

</details>

### ruler パス

ルールから予約を導出する処理は、読み取りを tx の外で行い、書き込みを 1 tx にまとめる。削除判断に必要な状態は適用時に確かめ直し、ブレーカー発動と同じ tx で確定する。

ruler は `RulerPass` から評価に必要なルール・意図・投資・予約・射影・履歴を読み、1 サイト分の書き込みを `ApplyPass` に渡す。

読み取りの後に削除候補の番組へ意図や投資が追加されうるので、削除は適用時に投資を再評価する。再評価しないと、古い入力で新しい予約意図の行を消しうるからである。

ブレーカーの発動を `ApplyPass` に含めるのは、発動行を削除と別の tx にすると、発動を記録できないまま予約を消しうるからである。

時間窓の比較時刻を呼び出し側から受け取るのは、比べる相手（録画の `program_start_at`）が store の記録した時刻ではないからである（「時刻」節の規則）。

<details><summary>根拠</summary>

- 契約: `internal/store.RulerPass`
- PostgreSQL 実装: `internal/store/pgstore.Reservations`
- ループの成功時刻（ruler・record_sweep・全件パス・削除 reconcile）: `internal/store.LoopMarkers` / `TestLoopMarkersContract`

</details>

### サーキットブレーカー

発動は行の存在で表し、再開は行の削除で表す。発動していない状態を表す行は作らない。

reconciler と削除 reconcile は `Breakers`、API は `BreakerAdmin` を使う。reconciler と削除 reconcile はもともと tx の外で発動するので、`Breakers.Trip` を単独で呼ぶ。ruler は `RulerPass.ApplyPass` の中で発動する（理由は「ruler パス」節）。

ゲージとログは store の外で出す。`breaker.Trip` と、ruler が commit 後に呼ぶ `breaker.AnnounceTrip` が同じ `announceTrip` を共有しているので、出し方は 1 か所にある。パスの先頭の `breaker.ObserveState` は、プロセス再起動後も DB の発動状態をゲージへ戻す。

<details><summary>根拠</summary>

- worker の発動・状態契約: `internal/store.Breakers`
- API の一覧・再開契約: `internal/store.BreakerAdmin`
- PostgreSQL 実装: `internal/store/pgstore.Breakers`
- ゲージとログ: `internal/breaker.ObserveState` / `internal/breaker.Trip`
- ruler の tx 内の発動: `internal/store/pgstore.Reservations.ApplyPass`
- ruler の発動ゲージ: `TestRunPass_TripInApplyPassRaisesCircuitBreakerGauge`

</details>

### catalog の export / restore

catalog の export と rescue の復元は、全保護対象の表を相手にする。ドメインごとの store を組み合わせると、export は表ごとに別の時点を読み、restore は途中まで書いた状態で失敗しうる。そのため、この 2 つだけはドメインをまたぐメソッドにする。

rescue は、文書にある deleted でないコピーのファイルの有無を確かめ、無かったコピーを `RestoreOptions.MissingAssetCopyKeys` で渡す。

restore が分類ルールの再評価と同じロックを最初の書き込みより前に取るのは、途中で取ると、recordings の upsert のトリガーが当たりの表の行を持ったままロックを待つからである。ロックを持つ再評価ジョブがその行を待つと、デッドロックする。

ストレージ走査からの登録は、§メディア公開の in-place 登録を使う。

<details><summary>根拠</summary>

- 契約: `internal/store.Catalog`
- PostgreSQL 実装: `internal/store/pgstore.Catalog`
- 往復: `TestCatalogContract` の `restore round trips an exported document`
- export 中の取り込み競合: `TestExport_ConcurrentIngestStaysConsistent`
- 欠損ファイルの deleted 復元（rescue 経由）: `TestRescueCatalogMissingFiles`
- rescue の再評価ロック位置: `TestCatalogContract/restore_waits_before_writing_for_label_rule_reevaluation`

</details>
