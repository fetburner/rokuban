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

メソッドの doc コメントには、何と何を直列化するかを書く。機構を書くと、別の実装が同じ保証を別の手段で満たせなくなる。

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
