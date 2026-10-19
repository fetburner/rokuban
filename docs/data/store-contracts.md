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

- DB の時計で比べる前例（GC）: `internal/ruler.runGC` の `observed_at < now() - horizon` と `postgresInterval`
- DB の時計で比べる前例（孤児候補）: `internal/worker.failOrphanCMLogoCandidates`。`ListRunningCMLogoCandidates` が返す DB の `now()` を使う
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
- マージ関数: `internal/reservation.MergeOverrides` の単体テスト

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

録画の行ロックは pgstore の共通ヘルパ `lockRecording` だけで取り、録画の行に触れる最初の文にする。ロックを取る前に読んだ古い値で判断すると、書き手どうしが互いの書き込みを観測できなくなるからである。

それより先に取るロック（局の advisory lock など）があれば、その順序をメソッドの doc コメントに書く。書き手ごとに取る順序が違うと、デッドロックしうるからである。

### チャプター編集

チャプター編集は、状態の読み取り、版を比較した置き換え、自動層へ戻す操作を一つのドメイン契約に置く。置き換えと自動層へ戻す操作は、tx の最初に録画の行をロックする。CM 検出の書き込みも同じ行をロックするので、両者は直列化される。

カット版の作り直し要求の記録と、カット版の公開も同じ行をロックする。作り直し要求に記録する keep と、公開時に照合する keep が、途中のチャプター編集で変わらないようにするためである。

読み取った版を置き換えるときに再確認することで、古い下書きが新しい内容を上書きしない。区間の重なりは、最後に区間の排他制約が拒否する。その違反は `ErrConflict` に写す。

<details><summary>根拠</summary>

- ドメイン契約: `internal/store.ChapterEdits`
- PostgreSQL 実装: `internal/store/pgstore.Analysis`
- 録画単位の直列化: `LockRecording` / `internal/worker/cm_detect.go`
- 作り直し要求とカット版の公開の直列化: `ReencodeRecordingProfile` / `prepareEncodePublish`
- 範囲制約: `recording_chapter_spans` の排他制約
- 導出値と事実: 不変条件 9
- 意味のある行: 不変条件 10
- 行の寿命: 不変条件 12
- ライブラリの公開契約: 不変条件 14

</details>
