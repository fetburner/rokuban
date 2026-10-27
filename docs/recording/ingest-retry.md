> [recording.md](../recording.md) §5「ingest パイプライン」のうち、リトライ設計（5.3）。索引から辿る。

### 5.3 リトライ設計（3 層）

ingest の失敗は 3 層で回復する。接続断はジョブ内で Range 再開し、プロセス死はジョブ契約の区切りと回収で拾い、最後に完全性を検証してからコミットする。

#### Range と HEAD の前提

転送は Range で途中再開でき、長さは HEAD で転送せずに取れる。ただし録画中の HEAD は長さを返さないので、HEAD を打つのは録画完了後に限る。

`GET /records/{id}/stream` は Range ヘッダーに対応するので、`Range: bytes=N-` で途中再開できる。録画中の Range 応答は `Content-Range: bytes N-M/*` のように総サイズが `*`（不明）になる（実測。Content-Length 自体は具体値を返す）。

フィルタを併用すると、mirakc は Range を無視して 200 を返す（エラーは出ない）。ingest は素の TS が欲しいのでフィルタなしで pull する。この 200 の扱い（途中再開の offset > 0 では拒否）はソース確認で得た挙動で、conformance テストはフィルタを併用しないので未判定である。

HEAD エンドポイントは、転送せずに正確な Content-Length を取れる。ただし録画中は Content-Length を返さない。`HeadRecordStream` は `-1` を返し、その `-1` をそのまま長さとして使うと、ingest がゼロ長ファイルを正しいものとして扱いかねない。そのため HEAD を打つのは、下記「層 3」のとおり録画完了後に限る。実測では録画中に `-1` を返し、完了後は Content-Length と一致する。ここまでの実測は `mirakc 4.0.0-dev.0` 相当の pin に対する判定であり、他のバージョンでは未判定である。

<details><summary>根拠</summary>

- 測定対象の pin: `mirakc 4.0.0-dev.0`
- 完了後の Range と HEAD の Content-Length: `internal/mirakc/conformance` の `TestConformance/CompletedRecordStreamAndDelete`
- 録画中の Range と HEAD の Content-Length=-1: `internal/mirakc/conformance` の `TestConformance/RecordingInProgress`
- Range を無視した 200 の扱いをソースで確認した箇所: `mirakc-core/src/web/api/recording/records/stream.rs`

</details>

#### 層 1: 接続断の再開（ジョブ内リトライループ）

本文が一定時間止まったら閉じ、一時ファイルに書けた位置から Range で再開する。リトライ上限は連続した一時障害の数で数え、散在する失敗は累積しない。

reader は、読み取り中の本文が止まったときだけ stall timer を動かす。`ingest.stall_timeout`（既定 30 秒）を超えた本文を閉じ、temp に書けた offset から Range を再開する。消費側の `Write` が遅い間は `Read` が呼ばれないため、その時間を mirakc の stall と誤認しない。

reader は `Range → 追い付き時の status → 待つ` を繰り返す。待ちはジョブ内（queue にジョブを戻さない）なので、temp file と offset が同じ Work の中で生き続ける。slice が終わると層 2 が temp と SHA-256 state を保存して同じ Args を snooze する。

接続断のたびに、進捗を書き出す。Range 本文がバイトを返した後にエラー（読み取りエラー・stall）で終わったら、reader は再試行の前に最新の書き込み位置を進捗の書き出しへ渡す。正常に終わった Range では呼ばない。追従中は 500ms ごとに Range が終わるので、間引きを無視すると秒 2 行になる。temp への書き込みエラーは mirakc の一時障害として再試行せず、Work に返す。

リトライ上限は「連続した一時障害」の数である。数時間の録画中に散在する失敗で上限に達しないよう、累積しない。204・416・206 の 0 バイトは接続失敗ではない。連続 5 回まで再試行し、6 回目で Work を失敗させる。

**録画中の worker 再起動は temp の末尾から再開する。** `SoftStopTimeout` を超える SIGTERM でも、次のジョブは DB の進捗値を使わない。同じ temp のサイズを Range の開始点にする。

<details><summary>根拠</summary>

- 本文の止まりを見る reader: `RecordFollowReader`。進捗の書き出しは `progress.flush`
- 接続失敗として数えない応答: `204` / `416` / `206` の 0 バイト

</details>

#### 層 2: ジョブ契約によるプロセス死の回収

転送を 4 分の区切りに分け、区切りごとに途中状態を保存して同じジョブを再配送するので、再開は全量のやり直しにならない。プロセス死で止まったジョブは、約 6 分で回収されて再試行対象になる。

##### 区切りと回収の時間

ジョブの timeout は 5 分で、転送の区切りは 4 分、残り 1 分は後処理に使う。プロセス死で止まったジョブは、回収の既定の 6 分が経つと再試行対象になる。

後処理は HEAD、SHA-256 確認、fsync、DB commit である。区切りを有限にすることで、録画全体の長さや低速回線に依存せず、queue adapter が停止した Work を回収できる。

回収の既定は 6 分で、ingest の timeout 5 分と既定 job timeout 1 分より長い。River adapter の JobRescuer は設定した閾値とジョブの timeout の長い方を待つ。そのため約 6 分で再試行対象になる。長い timeout を持つ kind は、その timeout が経過するまで待つ。

SIGTERM による停止では Work context が `Canceled` になる。5 分の deadline 超過は `DeadlineExceeded` として失敗に数え、停止中断は最終結果のメトリクスへ数えない。checkpoint 保存が区切りに達する前の停止では、次の Work が最後の保存位置から残りを replay する。

##### チェックポイントと replay

区切りごとに途中状態を保存して同じジョブを再配送する。再配送は試行回数を消費しない。再開では保存が使えれば残りだけを replay し、使えなければ先頭から全量を replay する。

区切りでは temp を fsync し、SHA-256 の binary state と offset を `.checkpoint` sidecar に保存して `jobq.Snooze(0)` を返す。

checkpoint の offset が temp サイズ以下なら、保存した hash state を復元して残りのバイトだけを replay する。checkpoint が無い・破損している・offset が temp サイズを超える場合は、先頭から全量 replay する。replay 自体が区切りに達した場合も途中状態を保存して snooze するため、大きな temp の再試行が毎回先頭から始まらない。

SHA-256 state の書き込みと読み出しは、temp の flock を保持したまま行う。checkpoint は temp と同じ削除条件に従い、mtime 猶予と aging による orphan 回収の対象になる。checkpoint が無効な場合に全量 replay へ戻すのは、状態ファイルだけが temp の正しさを決めないためである。

この経路では、ingest の job-id advisory lock や `record_sweep` による River 行の生 SQL 回収を使わない。`record_sweep` は mirakc の record を定期取得し、欠けた ingest job の投入を補う。temp の flock は同じ record の書き込みを直列化し、公開時は DB の一意 reservation と rel_path lock が採用を決める。

##### KEDA Job での再開

KEDA で 1 件ずつ動かす Job は、区切りごとに終了して次の Pod が再開する。そのため KEDA の検出から Pod 起動までの時間が、各区切りに加わる。

`--once` の KEDA Job は、Work が snooze を返すと 1 件を消化した扱いで終了する。

4 分は replay と転送に割り当てる上限である。1 分の後処理を足した Work timeout 5 分が、回収の既定 6 分より短くなるように選んだ。Pod 起動時間はクラスタとイメージ状態に依存するため、運用クラスタの起動時間を測り、通常の起動時間に対して十分長い区切りであることを確認する。

<details><summary>根拠</summary>

- ジョブの timeout: `IngestJobArgs.Spec().Timeout`（5 分）。回収はこの `Spec.Timeout` と閾値の長い方を待つ
- 回収の既定: `worker.rescue_stuck_jobs_after`（既定 6 分）
- 区切りで保存する sidecar: `.checkpoint`。再配送の依頼は `jobq.Snooze(0)`
- 同じ Args の再配送を固定するテスト基盤: jobqfake
- Work timeout と有限区切り: `TestIngestWorker_TimeoutFitsBeforeRescue`
- checkpoint 後の snooze が試行回数を消費しないこと: `TestIngestWorker_SHA256WaitSnoozesJobWithoutMetrics`

</details>

#### 層 3: 完全性検証とコミット

転送したバイト列の長さと SHA-256 を照合し、一時ファイルの永続化を終えてから DB にコミットして公開する。**DB コミットが公開点であり、mirakc 側の record の削除はコミット後だけである。**

##### 長さと SHA-256 の照合

pull 完了後に、書き込んだバイト数を HEAD の長さと照合する。finished を観測した record では、転送したバイト列の SHA-256 も mirakc の値と照合する。

SHA-256 は、Range 再開を含む全バイトを 1 パスで計算する。stream レスポンスの Digest / ETag ヘッダーは使わない。

mirakc が finished 後にハッシュを計算中のとき、`content.sha256` が `null` または欠落することがある。finished record と HEAD の長さが temp のサイズに一致し、期限前なら、temp を同期する。同じ Args を snooze して待つため、worker 枠を占有しない。進捗行は commit まで残し、原本 INSERT が世代をそこから写す（待ちの間に世代が変わると、録画中に配った再生 URL と commit 後の URL がずれる）。次の Work は record 固有 temp を使い、毎回 temp を replay しない。jobqfake の連続した再配送で `attempt=1` が保たれる。hash 到着後は `verified` で commit する。

空文字・空白付きの値は正規化し、64 文字の hex でない値は警告を出してスキップする。Content-Length が不明（`-1`）なら、長さの照合だけをスキップする。長さまたは SHA-256 が不一致なら `hash mismatch` / `size mismatch` で失敗し、commit と edge record の削除へ進まない。不一致は通常の River 再試行に戻し、専用メトリクス `rokuban_ingest_hash_mismatches_total` で観測する。

##### SHA-256 待ちの期限

待ちの期限は、転送済みバイト数を速度見積もりで割って決め、mirakc の録画終了時刻を起点にする。期限後もハッシュが無ければ、SHA-256 の照合を省いてコミットする。

起点が nil または未来なら、temp の mtime を使う。報告された実測は 2.35 GB の読み直しに約 4 分（約 9.8 MB/s）で、25% の余裕を含めて 7.8 MB/s とする。計測点が 1 件のため設定キーにはしない。下限は 10 秒で、地デジ 30 分（約 3.8 GB）では約 8 分、BS 2 時間（約 20 GB）では約 43 分になる。

期限後は `timeout_skipped` で commit する。HEAD の長さが不明（`-1`）なら、転送完了を先に判定できないため、待たずに commit する。実機での新しい待ち時間は未検証である。

##### 公開の順序

公開は、一時ファイルの永続化、ロックの取得、予約の INSERT、link、DB commit、temp の unlink の順に行う。この順序を入れ替えて、DB が指す実体を先に公開してはならない。

```mermaid
flowchart LR
  A[長さと SHA-256 を照合] --> B[temp を fsync して Close]
  B --> C[rel_path のロックを取得]
  C --> D[original 行を INSERT して予約]
  D --> E[canonical へ link]
  E --> F[親ディレクトリを fsync]
  F --> G[DB commit: 公開点]
  G --> H[temp を unlink]
  H --> I[mirakc の record を削除]
```

照合を通ったら、canonical rel_path と同じディレクトリに作った record 固有 temp の fsync と Close を行う。区切りでの fsync（checkpoint 保存の直前）とは別に、replay した既存部分を含めて完了時にも行う。バッファごとには行わない。

その後の短い確定区間で、rel_path 固有 filesystem lock を取得し、original の media_assets 行を INSERT して一意性を予約する。INSERT は commit まで他セッションに見えない。lock と transaction を保持したまま temp を canonical へ link し、親ディレクトリを fsync してから DB transaction を commit する。commit が成功したら temp を unlink する。宛先に別のファイルがあれば、temp を残し、record を消さず、行も作らずにエラーで止める。

link 前の一時障害では、temp を残して次の試行へ渡し、pull を続けられる。長さ / SHA-256 の不一致では中身が不採用と確定するため temp を消す。`canceled` / `failed` では原本として commit しないが、途中までの temp と進捗行は追っかけ再生用に残す。temp は公開前 orphan の aging で回収する。link 後の親ディレクトリ fsync または DB commit が失敗した場合は transaction を rollback し、temp を残す。次の試行が commit だけをやり直す。canonical file は自動削除しない。mirakc record は削除しない。link と DB commit の順序を反転させて、DB が指す実体を先に公開してはならない。

##### fsync の失敗と再試行

ファイルの fsync・Close・link・親ディレクトリの fsync のどれかが失敗したら、DB 登録も record 削除も行わず、ジョブを失敗させる。mirakc の record は削除しないので、データ喪失は構造的に起きない。

fsync を入れる理由には、電源断のほかに、Linux の遅延した書き込みエラーがある。ENOSPC や I/O エラーは Close では報告されず、fsync でしか上がらない。

link 後の親ディレクトリ fsync は、新しい directory entry の永続化を確定する。

link 後の失敗の次の試行は、転送も replay もせずに commit だけをやり直す。理由と判定は [contract.md](../storage/contract.md) の「scratch と temp の置き場所」にある。

DB commit が成立して応答だけ失われた場合は、次の冪等性チェックで転送を省略する。

##### 途中で終わった record の扱い

取り消し・失敗で終わった録画も、受け取った部分を原本扱いせずに一時的に再生できる。

原本行の存在は「録れた」と読まれて後継の録画が取り消されうる上、mirakc の部分ファイルは後継の試行に上書きされうる安定しない事実でもある。Rokuban が受信済みの temp は worker が終了した時点のローカルコピーなので、`recording_ingest_progress` 行とともに一時的な HLS 追っかけ再生へ使う。

理由は次の 3 つである。

- original 行の存在は「その放送イベントは録れた」と読まれる。ruler は status を見ずに original の有無で予約を外す。取消した番組を録画中に再予約した場合、前の試行の部分を commit すると、予約が外れて後継の録画が取り消される
- エッジの部分ファイルは安定した事実ではない。content path は番組ごとに決まり、mirakc は同じ番組の次の試行で同じファイルを切り詰めて書く。終端を観測した後の最後の drain は、後継のバイトを読みうる
- 後継の無い `failed` でも、完結していない原本の行は何も主張できない（不変条件 10）。進捗行は未 commit の temp を一時再生する状態だけを指し、録画完了は主張しない

##### 転送失敗が続いたときの滞留

長時間の転送失敗が続くと、エッジのリングバッファに未取り込みの record が溜まり続ける。ジョブが捨てられても、mirakc 側の record は DB commit 後にしか削除されないので、データは失われない。

取り込みジョブ自体は River の既定の試行上限のままで、上限に達すると discard（dead-letter）されうる。それでも record が宙に浮かないのは、mirakc 側の record が DB commit 成功後にしか削除されないためである。discard された後も、5 分周期の定期全量突き合わせが同じ finished record を見つける（[watcher.md](watcher.md) §3.3 の (c)）。sweep は record の観測を commit した後に、ingest ジョブをヒントとして投入し直す。ヒントが失われても、次の sweep が再び投入する。

運用上の主なリスクはこのディスクの滞留なので、「未 ingest の record 総量」をメトリクス化してエッジのディスク残量と突き合わせ、アラートする（[storage.md](../storage.md) のサイジング指針参照）。

##### 滞留がエンコード方針に及ぼす影響

滞留が EPG の保持猶予を跨ぐと、その録画のエンコード方針は予約から解決できず、既定値で凍結される。エンコードは投入されないが、原本は残るのでデータは失われない。

猶予は `epg.retention_grace`（既定 24h）である。`recordings.source` と `rule_id` がどうなるかは、その録画の `recordings` 行が作られたのが GC より前か後かで分かれる。作成時にまだ予約が引ければ、どちらも通常どおり書かれ、影響は encode policy の凍結だけにとどまる。

作成が GC 後にずれ込んだ場合は、`rule_id` が NULL になり、`source` も `unattributed` に落ちる。**このケースは[ingest-followup.md](ingest-followup.md) §5.5「ingest 完了後のフロー」の `encode_reconcile` でも回復しない。** desired が空になるので候補に入らないためである。

詳細と、滞留の型ごとに見るメトリクスが分かれること（未 ingest 総量は回線断の滞留を数えない）は、[storage.md](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」と [operations.md](../operations.md) §4 にある。

##### 取消・失敗の record の回収

取消または失敗で終わった record は、誰も回収しない。削除に purge を付けて素直に回収すると後継の録画のファイルまで消えるので、してはならない。

ingest は commit しないのでエッジの record を消さず、mirakc にも record の保持期限は無い。後継の試行が同じ content path を上書きした場合、中身は後継の commit 時の purge で消え、record の JSON だけが残る。中身が残り続けるのは、後継が無い場合（番組途中の失敗、再予約されない取消）だけである。

**この record を `purge=true` で素直に回収してはならない。** mirakc の `remove_record` は、content path を共有する別の record を見ずにファイルを消す。後継の録画のファイルまで消える。未解決: 同じ content path を持つ生きた record が無いものだけを回収する経路と、残った部分を手動で原本に採用する経路。どちらもまだ書き手が無い。

<details><summary>根拠</summary>

- hash 待ちで同じ Args を再配送し、`attempt=1` を保つこと: `TestIngestWorker_SHA256WaitSnoozesJobWithoutMetrics`
- late hash の到着後に verified commit すること: `TestIngestWorker_CompletesUntilLateContentSHA256`
- SHA-256 待ちの期限起点: `TestIngestSHA256WaitDeadline`、起点は `recording.endTime`
- 長さ不明のときの照合スキップ: `ingest.go` の `expectedLen >= 0` ガード
- 原本の有無で予約を外す ruler の関数: `ListFulfilledProgramIDsBySite`
- discard されうる ingest worker: `IngestWorker`。再投入する処理: `processRecord`
- 公開の要素: `fsync` / `Close`、`rel_path`

</details>

#### 冪等性: コミット済みなら転送をやり直さない

原本がコミット済みなら、ジョブは転送せず、エッジの record の削除だけを再試行して終わる。止めないと全量を再ダウンロードして、欠けたファイルを配ることになる。

`media_assets` に `kind='original'` の行が既にコミットされていれば、転送しない。エッジ record の削除は、失敗してもログのみで ingest 自体は成功扱いにしている。そのため mirakc 側に record が残ったまま、record_sweep 経由で同じ record の ingest ジョブが再投入されうる。ここで止めないと、新しい試行が canonical file を置き換えて全量を再ダウンロードし、streamer は不変条件 3（コミット = DB 行）に反して欠けたファイルを配ることになる。

<details><summary>根拠</summary>

- コミット済みの判定: `IngestWorker.hasOriginalMediaAsset`

</details>

#### 同じ rel_path の競合: 一意 reservation で採用を決める

DB の一意な予約を先に取った取り込みが公開を完了して採用される。後発は先発の結果を待って失敗し、その一時ファイルは回収に委ねられる。転送中は canonical path へ書かないので、転送自体は並行してよい。

canonical path へ転送中のバイトが存在しないため、異なる record の ingest は、それぞれの record 固有 temp へ並行して pull できる。同じ record は temp の flock で直列化する。公開時は rel_path filesystem lock と DB の一意 reservation を使い、同じ canonical path の ingest と通常削除の競合を直列化する。canonical orphan は削除せず、aging 後に報告する。通常削除も同じ lock を使うため、公開と unlink の競合は直列化される。

各 transaction の original INSERT が部分一意索引を予約する。先に INSERT した transaction が link・親 directory fsync・DB commit を完了すれば、その内容が canonical file の勝者になる。後発 transaction の INSERT は先発の commit / rollback を待ち、先発が commit した場合は unique violation で失敗する。後発の temp は失敗時の規約に従って残るので、canonical file は勝者の内容のまま保たれ、残った temp は orphan 回収に委ねられる。

転送前の判定は安価なヒントにすぎず、転送中は lock を保持しない。最終判断は一意索引と適用時の状態遷移に残る。

##### ロックの範囲

rel_path の filesystem lock は、公開と通常削除の区間だけに使う。canonical path に直接転送しないので、転送全体の job lock heartbeat や lock 喪失による cancel は不要である。

ingest commit と通常削除は、同じ rel_path の予約 lock file に対する POSIX `flock` を保持する。対象区間は link / unlink から DB commit までである。

##### RWX 越しの排他

別ノードの 2 レプリカ構成で、RWX の media 越しの排他ロックが効くかは未検証である。効かなければ、旧実行と代替実行が同じ一時ファイルへ書きうる。

2 レプリカ構成は、`maxReplicaCount: 2` と RWX の media PVC である。

この書き込みは、旧実行が生きたまま lock だけを失ったときに起きる。SIGKILL されたプロセス自身はもう書かないが、RWX ではカーネルが未書き込みのページを後から書き戻しうる（未検証）。heartbeat が応答待ち上限を超えて接続が閉じられる既存の窓に加え、lease 方式では 30 秒以上止まったプロセスと、DB から分断されたが生きている worker でも代替実行が走る。そのぶん当たる確率が上がる。

##### 再試行と孤児の扱い

再試行は録画固有の一時ファイルで再開し、ロックが同時書き込みを防ぐ。一時ファイルの孤児は、mtime 猶予とエイジングの後に回収される。

中身の不一致では temp を消す。record の cancel / fail では部分 temp を HLS 追っかけ再生用に残す。それ以外の失敗も次の試行へ temp を渡す。いずれの temp も mtime 猶予（既定 7 日）とエイジング（既定 14 日）の後に回収する。temp の回収は同じ flock に参加し、実行中の ingest と競合した場合は次の pass へ延期する。

link 後の commit 失敗で残った canonical は、再試行が成功すれば行が付く。成功しないまま行が無い canonical は aging 後に報告し、自動削除しない。replay は同じ temp のローカル読み直しなので、scratch 経由の全長コピーは追加しない。追加コストは replay・temp の link と unlink・親 directory fsync である。

##### 弱い FS への直接書き込み

**弱い FS へ原本を直接書く設計は、将来課題へ戻した。** FUSE の rename 非対応や fsync / Close の不確かな意味論に合わせる必要があるためである。

`storage.media_dir` は強い FS に限定し、FUSE S3 は原本・派生物のどちらにも使わない。

<details><summary>根拠</summary>

- 転送前のヒント: `checkRelPathConflict` / `GetLiveMediaAssetByRelPath`
- 2 レプリカ構成の前提: `maxReplicaCount: 2`
- 原本を置く領域: `storage.media_dir`

</details>
