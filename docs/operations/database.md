> [operations.md](../operations.md) §3「DB 運用」の一部。索引から辿る。

## 3. DB 運用

### 輻輳時の隔離

「ユーザー操作で DB が詰まったら録画やエンコードに影響しないか」という懸念への対策。DB 輻輳から分離できる処理はあるが、故障モードを常に「収束の遅れ」とは一般化しない。運用上の保証は次の条件付きである:

- **録画は、mirakc に番組終了前まで同期済みの予約に限って DB 停止から分離される**。スケジュールは mirakc 側の `schedules.json` に永続化済みで、録画実行は mirakc が自律的に行う。ただし mirakc 自身、録画バッファ、チューナーが動作していることが条件であり、新規・変更予約は reconciler が期限内に同期できなければ録画されない
- **実行中の ingest は、転送中のバイト I/O だけを見れば DB の外側にあるが、ジョブ全体は DB に依存する**。開始時の `record_sync` 参照、進捗の書き込み、公開点である `media_assets` コミットが必要である。4 分の slice ごとに temp と SHA-256 checkpoint を保存し、同じ River job を snooze して再開する。プロセス死では JobRescuer が同じ job を回収する。DB 障害で接続やコミットを失えば、録画バッファに record が残り、再試行できる範囲では収束する（詳細は [ingest](../recording/ingest.md) §5.3）
- **実行中の encode は ffmpeg のバイト処理だけを見れば DB の外側にあり、公開も `media_assets` コミットで決まる**。encode は `Timeout() = -1` なので JobRescuer の対象外だが、`EncodeWorker` は Work 中に job-id advisory lock を保持する。`encode_reconcile` は 1 分以上古い `running` 行を候補にし、lock の解放を確認する。取得できた場合だけ旧行を `discarded` にして別 ID の代替ジョブを投入するため、ライブ中の長時間 encode は時刻だけでは回収しない。lock 用セッションには `idle_session_timeout`（30 秒）の lease が付く。プロセスが 30 秒以上止まるか DB から分断されると、生きている encode でも lock が外れて代替ジョブが投入される。scratch はジョブ ID ごとなので、並走した 2 本は互いの出力を消さない（代償として両方が ffmpeg を完走する）。canonical へは同じディレクトリの一時ファイルへ lock の外でコピーする。公開は rel_path の filesystem lock と advisory xact lock の中で、行を読み直してから rename で行う（`O_TRUNC` で直接コピーしない）。既に active で（カット版は凍結区間も）同じ内容なら、成功で飛ばして何も置かない。`rel_path` が計画時と違うのは「自分の計画が古い」という意味なので、行が active のままなら公開せずに River の snooze で戻して計画をやり直す（成功で飛ばすと新しいチャプター編集が消える）。行が active でなければ成功で飛ばす。これで後発の実行が先発の commit を巻き戻したり、ファイルだけ上書きして行と食い違わせたりしない。advisory xact lock が排他するのは ingest commit と孤児回収に対してだけで、通常削除とは filesystem lock でしか排他されない。RWX のメディア越しに `flock` が効くかは未検証。`recording_encode_attempts` は回収時には触らない。代替ジョブの開始時に上書きする。詳細は [k8s 運用](k8s.md)。
- **長時間の滞留はポリシーを失うことがある**。ingest が `epg.retention_grace` を跨ぐと、予約から encode policy を解決できず既定値で凍結され、作成時点で予約も意図も無ければ `source` は `unattributed` になる。原本の保持・エンコードの扱い、回線断を含む滞留の測り方は [ストレージ運用](storage.md) §4 と [ストレージ](../storage.md) §6 を参照する
- **ルール評価は UI と同期しない**。ルール編集 API は編集を書いて再評価ジョブを投入するだけで即応答

実装規律:

- **ロール別コネクションプール上限を分ける**。api が全コネクションを食い潰して worker / reconciler が待つ事態を防ぐ。
  ただしプロセスは常に 1 個のコネクションプールしか持たない（`cmd/rokuban/server.go` が起動時に 1 回だけ作り、
  そのプロセスが担う全ロールが共有する）。したがって「ロール別」とは複数プールを作ることではなく、
  **そのプロセスが担う roles 集合と束縛サイト数（worker は引くキューと concurrency から
  数えた lock 枠も）から、そのプロセスが持つ唯一のプールの `MaxConns` を決める**
  ことを指す（`internal/db.NewPool`）。`db.max_conns` を明示すればそれを使い、未指定ならロールごとの
  budget（1 サイト束縛時: api: 10 / worker: 床 8（下記）/ watcher: 3 / notifier: 3 / streamer: 4。根拠は
  `internal/db.roleConnBudget` の doc コメント）を roles の分だけ合計する。monolith（`--all`）は
  全ロール分の合計になる。**1 プロセスが N サイトを束縛できるため（`--sites`）、watcher は
  束縛サイトごとに advisory lock 用コネクションを 1 本専有し続ける**（site ごとに
  `role.RunSingleton` の goroutine を持つ。`cmd/rokuban/server.go` の watcher ループ）。
  2 サイト目以降は budget にも自動で上乗せされる（`internal/db.perSiteConnBudget`）。
- **worker の budget だけは表ではなく、実行中のジョブの本数から導出する**（`internal/db.workerConnBudget`）。
  `worker: 8` は「この構成ではこれだけ要る」ではなく**床**である。実行中の ingest / encode /
  cm_detect はそれぞれ job advisory lock 用のコネクションを Work の冒頭から commit まで
  1 本保持し、その本数は設定（`ingest.concurrency` / `encode.concurrency`）と束縛サイト数から
  決まる。**本数を数えるのは `internal/worker.LockSlots` で、`db` はそれを int 1 個として受け取る**
  （`db` は `worker` を import しない）。予算は `1(LISTEN) + lock 本数 + 3` を床 8 で下支えした値。
  運用者が `ingest.concurrency` を上げれば予算は自動で追随する。`--once` の Job は 1 枠、
  ingest / encode / cm_detect を引かないプロセスは 0 枠になる。
- `db.max_conns` を明示指定する場合の fail-fast（`internal/db.minRequiredConns`）も同じ枠を数える。
  数える対象は **「解放が別の接続取得に依存する専有」** である。内訳は watcher の advisory lock
  （1 サイトあたり 1 本）と、worker / notifier の LISTEN である。加えて実行中の ingest / encode /
  cm_detect が job advisory lock を 1 つずつ持つ。合計に余地 1 本を足した値が下限になる。
  **job advisory lock を「転送中だけの一時専有」として下限から外してはならない。** lock を
  持つジョブは解放する前に同じプールからもう 1 本取る（進捗書き込み・commit）ので、LISTEN と
  lock でプールが埋まるとジョブ同士が循環待ちになり、heartbeat は lock セッション自身の上で
  動くため lock は生き続ける（構造から確定した結論で、実測はしていない）。
  **lock をプール外の接続で張る案は採らない** --- `db.max_conns` がプロセスの接続上限だという
  契約を破ることになる。
- **API 系クエリに `statement_timeout`** を設定する。クエリ単位の context timeout だと「付け忘れた 1 本」が
  必ず生まれる。接続の `RuntimeParams`（起動パケットの session default）で一括適用する
  （`db.api_statement_timeout`、未指定なら 30s）。**api ロールを含むプロセスのプール全体に適用される**。
  monolith で api と worker/watcher を同居させると worker 側のクエリにも同じ上限がかかる。
  世帯スケールの通常クエリを十分に上回る値（既定 30s）にしてあるので実害は想定していないが、
  ロールを分離すれば worker 単独プロセスには一切適用されなくなる。`statement_timeout` は行ロック待ちにも
  効く（Postgres は「クエリの実行時間」と「ロック待ちの時間」を区別しない）ため、monolith で
  `record_sweep` 等が別トランザクションの行ロックを長く待つ状況では statement_timeout で中断されうる。
  中断されても River が再試行するので致命的ではないが、意図しない再試行が増える兆候として覚えておく

### transaction pooling は通さない

Rokuban は transaction pooling を通さない。
worker は River の LISTEN を使い、`notifier.New` の 1 個の Listener を elector と job-available 通知で共有する（`river@v0.47.0` で確認済み）。
watcher は advisory lock、notifier は SSE 用の LISTEN を使うため、transaction pooling で接続が要求ごとに入れ替わると壊れる（[data.md](../data.md) §2 / §3）。
将来ハイブリッド構成を実装するときは、必要な pooler 対応の形をその PR で改めて決める。

### managed PostgreSQL の `btree_gist`

チャプター区間の `EXCLUDE` 制約を作る migration は、先に `btree_gist` を `public` へ追加する。
この文が失敗すると `migrate up` はそこで止まり、後続の migration に進まない。
k8s では [`migrate-job.yaml`](../../deploy/k8s/base/migrate-job.yaml) の Job が失敗し、手順は `kubectl wait` で止まる。
api の Deployment は同じ `kubectl apply` で新しい版へ更新されており、migration 未完了のスキーマに乗る。
Job の失敗を解消してから、Job を delete して手順を再実行する（順序と理由は [deploy/k8s/README.md](../../deploy/k8s/README.md) §使い方）。

- PostgreSQL 標準の `btree_gist` は trusted extension なので、対象 DB の `CREATE` 権限があれば作れる（[btree_gist](https://www.postgresql.org/docs/current/btree-gist.html)）。ただし managed provider がこの拡張を許可するかは接続先ごとに違う
- `IF NOT EXISTS` は既存の拡張があると作らずに notice を返す。成功しても新規作成権限の証明にはならない
- 権限エラーになったら、DB 管理者が同じ文を事前に実行してから migration を再実行する。エラーを握りつぶす変更や migration の書き換えはしない

本番相当の managed 環境での実測は未実施である。確認の手順は [runbook/managed-postgres.md](../runbook/managed-postgres.md)。

### EPG churn / autovacuum

EPG テーブルは 1 日に何度も大量 upsert されるため、遅くなるとしたら検索ではなく書き込みと autovacuum の追従。対策:

- バッチ upsert
- GIN fastupdate
- **テーブル別 autovacuum チューニング**（EPG テーブルに対して `autovacuum_vacuum_scale_factor` を小さく設定する等）

EPG テーブルは 8 日分 + 猶予のローリングウィンドウであり、永遠に太らない。検索性能自体は世帯スケール（数万〜20 万行）では問題にならない。

### Postgres datadir とエンコード scratch の分離

monolith モードでは Postgres のデータディレクトリとエンコードの scratch が同じディスクに載りやすい。実際に競合しやすいのはロックよりディスク I/O であり、**両者を同じディスクに置かない構成を推奨する**。

### バックアップ

保護対象は「ルール・録画履歴・media_assets・ドロップ統計・tombstone・手動オーバーライド」のみ（数 MB）。EPG プロジェクションは mirakc から再構築可能、ジョブキューは一時的。

- **catalog エクスポート**: worker の定期ジョブが、コアデータを JSON でメディアストレージ自身の `catalog/` 配下に書き出す（日次 + 世代保持）。メディアが生き残る障害では catalog も一緒に生き残る。pg_dump に依存しない（公式イメージに postgres クライアントを同梱しなくてよい）アプリレベルのエクスポート
  - **1 世代 = 1 ディレクトリ（`catalog/catalog-<時刻>/`）で、`manifest.json` を最後に書き終えたものだけが完成世代**。判定基準の詳細は [storage.md](../storage.md) §8
  - **健全性の確認は `rokuban catalog verify`**（DB に触らない。完成世代が 1 つも無ければ非ゼロ終了する）。何世代が完成しているか / rescue が使う世代 / 落ちた世代とその理由を出す。**`manifest.json` の存在を目視するだけでは確認にならない**（存在は必要条件でしかなく、完成判定はサイズと sha256 の照合まで含む）
  - **`rokuban rescue` を健全性の確認に使わない。破壊的な操作である**。rescue は検証の後に必ず DB へ書き、live DB を catalog スナップショットで**上書きする**。`recordings.status` / `deleted_at` / `purged_at`、`media_assets.state` / `deleted_at` を catalog の値で上書きし、id シーケンスを巻き戻す。健全な DB に対して実行すると、catalog を書き出した時点まで状態が巻き戻る（削除済み asset の復活を含む）。使うのは DB を失った後だけ
- **pg_dump（推奨・非必須）**: フル忠実度が欲しい場合の日次 pg_dump 構成例をドキュメントに記載する
- 世帯スケールでは catalog + 任意の pg_dump で十分。WAL アーカイビングは過剰
