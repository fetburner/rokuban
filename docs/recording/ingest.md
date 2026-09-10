> [recording.md](../recording.md) §5「ingest パイプライン」・§6「B-CAS 復号の責務境界」の一部。索引から辿る。

## 5. ingest パイプライン

録画完了後、mirakc のエッジから Rokuban のアーカイブストレージへ録画データを取り込む一連の処理。

### 5.1 転送方式: API pull 固定

`records/{id}/stream` による HTTP pull を全構成で統一する。「monolith モードなら mirakc の basedir を直接読めるのでは」を検討し、**HTTP loopback 経由を維持**と結論した:

- **ディスク I/O は直読みでも減らない**。basedir（リングバッファ）→ メディアストレージのコピー自体は必要で、節約できるのは loopback TCP のオーバーヘッドだけ。1 日数本・数十 GB では無視できる
- **コピー自体が耐障害設計**。録画はシステム内で唯一のリアルタイム・リトライ不能な操作なので、ローカルディスクへ録画 → 完了後にリトライ可能な転送、という分離は崩さない（mirakc に最終保存先へ直接書かせる案は、録画中の NAS/FUSE ストールが放送の欠損に直結するため不採用）。ドロップスキャンも転送パスがあるからタダで載る
- **コードパスが 1 本**。HTTP pull は monolith / 分散 / ハイブリッドの全構成で動く唯一の方法
- **所有権が明確**。basedir は mirakc の所有物で、Rokuban は API 越しの客に徹する

「同居時の basedir 直読み」は loopback が実測でボトルネックになった時の最適化オプション（YAGNI）。契約は **mirakc とは常に API、自身のストレージとは常にファイルシステム**の 2 面で固定（[storage.md](../storage.md) 参照）。

### 5.2 インライン TS ドロップスキャン

ingest はどのみち record の全バイトをストリームコピーするので、その途中で 188 バイト境界の TS パケット統計を取得する。**追加 I/O パスゼロ**で EPGStation 相当のドロップログが作れる。

採取する統計:

- PID ごとの continuity counter 不連続
- transport_error_indicator
- scrambling_control

PID 別サマリを media_assets に紐づくテーブルへ格納し、UI で表示する。実装は `internal/tsstat`。

#### 判定の規約（誤検知を出さないために必要なもの）

continuity counter の不連続を数えるだけでは実放送で大量の誤検知が出る。実測した
既知の良品（NHK 総合、5.3GB / 2841 万パケット）では `discontinuity_indicator` が
230 回立っていた。以下を守って初めて drop が 0 になる。

| 規約 | 扱い |
|---|---|
| NULL パケット（PID 0x1FFF） | CC は意味を持たないので統計対象外 |
| payload なしパケット（`adaptation_field_control` が `00` / `10`） | CC は増えない。**増えていたら payload 付きパケットの欠落**として数える |
| `discontinuity_indicator` | CC の不連続は正常。基準を取り直す |
| CC が直前と同じ + payload が**同一** | 規格が許す重複。1 回までは正常、2 回以上は異常 |
| CC が直前と同じ + payload が**相違** | 重複ではなく 15 個（16n-1）欠落。CC が一周して直前と一致している |
| PID の初回パケット | 基準がないので数えない |
| `transport_error_indicator` | error に数え、**CC の追跡から外す**（後述） |
| `transport_scrambling_control` | `00` 以外を異常として数える |

**16 の倍数の欠落は原理的に検知できない。** CC は 4 ビットなので、ちょうど 16n 個
欠落すると CC が期待値と完全に一致し、payload を見ても正常な次のパケットと区別が
つかない。これは規格上の限界で、他実装も同じ。

#### 検証方法

`internal/tsstat/integration_test.go` が既知の良品に対する差分テストを持つ。
`ROKUBAN_TEST_TS_FILE` に別実装で drop / error /
scrambled がいずれも 0 と確認済みの .m2ts を指すと有効になる。ファイルは巨大かつ
著作物なのでリポジトリには置かない。

clean なファイルでは「誤検知がないこと」しか確かめられないので、検知側は同じ
ファイルをストリーム処理中に**メモリ上で**壊して検証する（欠落・TEI・scrambling を
注入。ディスクに改変コピーは作らない）。

#### [tspacketchk](https://github.com/kaikoma-soft/tspacketchk) との差分

判定ロジックは概ね一致している（重複を payload 比較で見分ける点、payload なし
パケットの CC を検査する点は同実装から取り入れた）。意図的に違えているのは 1 点:

**TEI パケットの CC を信用しない。** tspacketchk は TEI 時に error を数えつつ CC を
更新するため、直後のパケットで drop も 1 数える（1 つの破損が error と drop の両方に
計上される）。Rokuban は TEI 時に継続性の追跡を打ち切り、次のパケットで基準を
取り直す。破損の実数を二重に数えないことを優先した。

**他の候補との比較**:

| 候補 | 判定 |
|---|---|
| mirakc の `logFilter` ログ | Web API に record のログファイルを取り出すエンドポイントが存在しない。収集には共有 FS かエッジ転送エージェントが必要 → 不採用 |
| インラインスキャン | 追加 I/O ゼロ。採用 |
| `recording.record-broken` / `recording.failed` | 構造化品質シグナルとして補完的に使用 |

外部ツール（tsselect 等）の exec も検討したが、数十 GB への二度目の I/O パスと依存の追加に見合わないため、インライン 1 パスとする。

### 5.3 リトライ設計（3 層）

- `GET /records/{id}/stream` は **Range ヘッダー対応** → `Range: bytes=N-` で途中再開可能。`internal/mirakc/conformance` の `TestConformance/CompletedRecordStreamAndDelete`（完了後）・`TestConformance/RecordingInProgress`（録画中）が mirakc 4.0.0-dev.0 相当に対して判定している。録画中の Range 応答は `Content-Range: bytes N-M/*` のように総サイズが `*`（不明）になる（実測。Content-Length 自体は具体値を返す）
- **フィルタ併用時は Range が 400**。ingest は素の TS が欲しいのでフィルタなしで pull → 常に Range 可（ソース確認。`mirakc-core/src/web/api/recording/records/stream.rs`。conformance テストはフィルタを併用しないので未判定）
- **HEAD エンドポイントあり** → 転送せず正確な Content-Length を取得できる。ただし録画中は Content-Length を返さない（`HeadRecordStream` は `-1`。黙って `-1` を長さとして使うと ingest がゼロ長ファイルを正しいものとして扱いかねない）ので、HEAD を打つのは下記「層 3」のとおり録画完了後に限る。`TestConformance/RecordingInProgress` が録画中に `-1` を返すこと、`TestConformance/CompletedRecordStreamAndDelete` が完了後の Content-Length 一致を、それぞれ mirakc 4.0.0-dev.0 相当に対して判定している

#### 層 1: 接続断の再開（ジョブ内リトライループ）

切断時は書き込み済みオフセットから `Range: bytes=N-` で再接続して追記。ドロップスキャンのカウンタはメモリ上に生きているので継続できる。タイムアウトは総時間ではなく**ストール検知**（`ingest.stall_timeout`、既定 30 秒間無進捗で切断扱い）--- 総時間タイムアウトは遅い回線の正常な転送を殺す。

#### 層 2: ジョブ再試行（プロセス死）

ingest の転送中にプロセスが死ぬと River の行は `running` のまま残り、`Timeout() = -1` の
ingest は River の通常の stuck-job rescue 対象にならない。そこで `record_sweep` は watcher の
全量突き合わせより前に、最後の活動（`recording_ingest_progress.observed_at`、行がまだ無ければ
`river_job.attempted_at`）が 1 分以上古い `running` ingest を候補として調べる。

候補を時刻だけで死亡と判定してはいけない。ingest は Work の開始時に
`rokuban:ingest:job:<river_job.id>` の PostgreSQL セッションレベル advisory lock を取得し、
commit まで保持する。`record_sweep` がそのジョブ ID の lock を
`pg_try_advisory_lock` で取得できた場合だけ元プロセスのセッションが無い（= プロセス死）と
確定する。lock を取れなかった live transfer は回収しないので、遅い転送や HEAD / fsync /
commit 中の古い進捗を時間だけで打ち切らない。rel_path の排他にはこの lock を使わない。

job lock の heartbeat は lock 用セッションを idle 切断から守る keepalive だけを担う。
lock 喪失を検知しても転送をキャンセルしない。一時ファイル方式では古い実行が残っても
canonical file を壊せず、DB の一意 reservation が採用を決めるためである。

死亡と確定した場合は、古い `running` 行に回収理由と `finalized_at` を記録して `discarded` に
終端化し、同じトランザクションで別 ID の ingest ジョブを投入する。古い行を `running` のまま
再投入すると、UniqueOpts の `pendingJobStates` に `running` が含まれるため新しい試行が古い行へ
合流し、回収できない状態が続く。進捗行が作られる前に死んだケースも、`attempted_at` fallback
で同じ経路に乗る。新しい試行は canonical とは別の一意な temp にゼロから転送する。プロセス死で
temp が残った場合は既存の orphan 回収（mtime 猶予 + aging）が拾う。中途再開はスキャナ状態の
永続化と追記が必要になり、層 1 で大半が救われる以上、複雑さに見合わない。

回収は既定 5 分周期（起動時に 1 回実行）で走る `record_sweep` に組み込んでいるため、通常は
候補になってから最大で約 6 分以内に再投入される。総時間 timeout を有限値にする案は、録画
サイズや期待転送速度から安全な上限を決められず、正常な低速転送を殺すので採らない。

**残る誤検知の窓**: `IngestWorker.Work` の `defer jobLock.release()` は、River がジョブの
終端状態（`completed`）を DB に書くより前に走る。River の `BatchCompleter` は 50ms 周期の
tick に加え、backlog が閾値未満でも 5 tick ごと（250ms 相当）にバッチを確定する実装なので
（`river/internal/jobcompleter/job_completer.go`）、`completed` の永続化まで数百 ms かかり
うる。転送が長時間続くと `commit` が進捗行を消して `attempted_at` は何時間も前のままになる
ため、この数百 ms の間だけ「候補（進捗が古い）かつ job lock が空き（release 済み）かつ
`state='running'`（まだ completed 反映前）」が同時に成立し、成功したジョブが discarded に
され冗長な ingest が 1 本入りうる。壊れはしない --- 代替側は `hasOriginalMediaAsset` の
冪等性チェックで短絡し、転送をやり直さない。ただし失敗して再試行中のジョブがこの窓に入ると、
River のバックオフと `attempt` カウンタは失われる。この窓を塞ぐ二段確認の類は作らない（回収
遅延と状態を増やすだけで、上記のとおり破損はしないため）。

#### 層 3: 完全性検証とコミット

pull 完了後に書き込みバイト数を HEAD の Content-Length と照合する。長さが一致したら、canonical rel_path と同じディレクトリに作った試行固有 temp の `fsync` → `Close` を行う。`Content-Length` が不明（`HeadRecordStream` が `-1`）なら照合だけをスキップして `fsync` へ進む（`ingest.go` の `expectedLen >= 0` ガード）。

その後の短い DB transaction で original の `media_assets` 行を INSERT し、rel_path の一意性を予約する。INSERT は transaction が commit するまで他セッションから見えない。この transaction を保持したまま temp → canonical の atomic rename と親ディレクトリ `fsync` を行い、最後に DB transaction を commit する。**DB commit が公開点であり、mirakc 側の record 削除は commit 後だけ**である。

rename 前に失敗した試行は自分の temp を消す。rename 後の親ディレクトリ `fsync` または DB commit が失敗した場合は transaction を rollback し、canonical file は orphan として aging 回収に委ねる。mirakc record は削除しない。rename と DB commit の順序を反転させて、DB が指す実体を先に公開してはならない。

fsync を入れる理由は電源断だけではなく、Linux では遅延した書き込みエラー（ENOSPC / I/O エラー）が `Close` では報告されず `fsync` でしか上がらないためである。rename 後の親ディレクトリ `fsync` は新しい directory entry の永続化を確定する。ファイル `fsync` / `Close` / rename / 親ディレクトリ `fsync` のいずれかが失敗した場合は DB 登録も record 削除も行わず、ジョブを失敗させる。どこで落ちても最悪「もう一度 pull」で、データ喪失は構造的に起きない。

運用上の主なリスクは**長時間の転送失敗でエッジのリングバッファが溜まり続ける**こと。`IngestWorker` 自体は River の既定の試行上限のままで、上限に達すると discard（dead-letter）されうる。それでも record が宙に浮かないのは、mirakc 側の record が DB commit 成功後にしか削除されないため: discard された後も record_sweep（5 分周期の定期全量突き合わせ。[watcher.md](watcher.md) §3.3 の (c)）が同じ finished record を見つけ、`processRecord` が同一トランザクションで ingest ジョブを再投入し続けるからである。「未 ingest の record 総量」をメトリクス化してエッジのディスク残量と突き合わせてアラートする（[storage.md](../storage.md) のサイジング指針参照）。

**帰結はディスクだけではない。** 滞留が `epg.retention_grace`（既定 24h）を跨ぐと、その録画の encode policy は予約から解決できず既定値で凍結される（エンコードが投入されない）。原本は残るのでデータは失われない。`recordings.source` と `rule_id` がどうなるかは、その録画の `recordings` 行が作られたのが GC より前か後かで分かれる。作成時にまだ予約が引ければどちらも通常どおり書かれ、影響は encode policy の凍結だけにとどまる。作成が GC 後にずれ込んだ場合は `rule_id` が NULL になり `source` も `unattributed` に落ちる。**このケースは下記 §5.5 の `encode_reconcile` でも回復しない**（desired が空になるので候補に入らない）。詳細と、滞留の型ごとに見るメトリクスが分かれること（**未 ingest 総量は回線断の滞留を数えない**）は [storage.md](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」と [operations.md](../operations.md) §4。

#### 冪等性: コミット済みなら転送をやり直さない

`media_assets` に `kind='original'` の行が既にコミットされていれば、ジョブは転送せず、エッジ record の削除だけを再試行して終わる（`IngestWorker.hasOriginalMediaAsset`）。エッジ record の削除は失敗してもログのみで ingest 自体は成功扱いにしているため、mirakc 側に record が残ったまま record_sweep 経由で同じ record の ingest ジョブが再投入されうる。ここで止めないと新しい試行が canonical file を置き換えて全量を再ダウンロードし、streamer は不変条件 3（コミット = DB 行）に反して欠けたファイルを配ることになる。

#### 同じ rel_path の競合: 一意 reservation で採用を決める

canonical path へ転送中のバイトが存在しないため、同じ `rel_path` を算出した複数の ingest は、それぞれ自分の一意な temp へ並行して pull できる。`checkRelPathConflict` / `GetLiveMediaAssetByRelPath` は転送前の安価なヒントであり、同時 ingest の決着には使わない。

各 transaction の original INSERT が部分一意索引を予約する。先に INSERT した transaction が rename・親 directory `fsync`・DB commit を完了すれば、その内容が canonical file の勝者になる。後発 transaction の INSERT は先発の commit / rollback を待ち、先発が commit した場合は unique violation で失敗する。後発の temp は自分で消えるので canonical file は勝者の内容のまま保たれる。delete_reconcile の `deleting` 行との TOCTOU は閉じない: 先読みはヒントであり、正しさは一意索引と適用時の状態遷移に残る。

- **rel_path advisory lock は削除した。** canonical path に直接書かないので、ロック喪失から検知までの窓と heartbeat による転送 cancel は不要である。残る job-id advisory lock は record_sweep が live job と死亡 job を区別するためだけに使い、heartbeat はそのセッションの keepalive だけを担う
- **同一録画の再試行**: 現行の `IngestWorker.Timeout() = -1` と River の running を含む一意投入により、プロセス内の通常の River 経路では古い ingest と新しい ingest が同時に走らない。プロセス死で running 行だけが残った場合も、上記のジョブ lock 確認と旧行の終端化を経て新しい試行へ進む。temp は試行ごとに新しい名前になる
- **孤児と追加 I/O**: 失敗試行の temp は自分で消し、プロセス死や rename 後の DB 失敗で残るファイルは既存の `orphan_files` の mtime 猶予（既定 7 日）とエイジング（既定 14 日）が回収する。正常な転送に scratch 経由の全長コピーは追加せず、追加コストは temp の作成・rename・親 directory `fsync` である

**弱い FS へ原本を直接書く設計は、FUSE の rename 非対応や fsync/Close の不確かな意味論に合わせるための将来課題へ戻した。** 本 issue では `storage.media_dir` を強い FS に限定し、FUSE S3 は派生物専用の領域に限る。

### 5.4 負荷分担: worker

`records/{id}/stream` の負荷が乗るのは worker（ingest ジョブ、KEDA で 0〜N）であり、reconciler は数百件のメタデータ diff を回すだけの軽いジョブのまま。ただし**本当のボトルネックはクラウド側ではなくエッジ側**:

- ハイブリッド構成では自宅アップリンク帯域が律速。worker を増やしても速くならない
- エッジでは録画中の書き込みと pull の読み出しが同じディスクで競合する。pull がディスクを飽和させて録画をドロップさせるのは本末転倒

→ **ingest の同時実行数は mirakc サイト単位で少数（1〜2）にキャップ**する（サイト別キュー or River の同時実行数設定）。worker の水平スケールが効くのは encode（CPU バウンド、入力はクラウド側ストレージ）の方。

### 5.5 ingest 完了後のフロー

**同一トランザクションでの投入はしない。** `media_assets` のコミット**後**に、ベストエフォートのヒントとしてエンコードジョブを投入する（`IngestWorker.Work` → `EnqueueMissingEncodes`。`ingest.go` の `enqueueMissingEncodesFromContext` 呼び出し）。投入に失敗してもログのみで、コミット済みの ingest は巻き戻さない。

**落としたヒントは定期パスが埋める。** ヒント投入の失敗とエッジ record の削除成功（`DeleteRecord`。上記「層 3」）が両方起きると、そのヒントは二度と飛ばない —— エッジに record が残っていないので record_sweep も ingest ジョブを再投入しない。ヒントだけに頼ると、コミット済みの録画が誰にも再投入されず黙ってエンコードされないまま残る。これを塞ぐのが `encode_reconcile` ジョブ（`internal/worker/encode_reconcile.go`、既定 15 分周期）で、専用クエリが desired（`recording_encode_policy.encode_profiles`）− observed（active な `encoded` の `media_assets`）の不足する `(recording_id, profile)` を一括取得し、River に投入する。真実は DB の状態であって「ヒントが飛んだかどうか」ではない（不変条件 5）。

対象は「原本（`kind='original'`）が active でコミット済み」かつ「ごみ箱に入っていない」録画に限る（ingest 未完了の録画とユーザーが捨てた録画を掘り起こさない）。エンコードは site の属性を持たない（アーカイブもプロファイルも単一）ので、このジョブは record_sweep のような site 単位ではなく全体で 1 本。`worker.periodic_jobs: false` の構成では `rokuban enqueue encode-reconcile` を CronJob から叩く（[operations/monitoring.md](../operations/monitoring.md) の CronJob 一覧）。

**繰り返すパスは「投入しても必ず失敗する仕事」を作ってはならない。** ヒントは一度きりなので、設定から消えたプロファイルを投入して `unknown encode profile` で失敗させるのは運用者への通知として妥当だが、15 分ごとに同じことをすると失敗を無限に作り続ける。定期パスは desired を**現在の `encode.profiles` に存在する名前だけ**に絞る。落とした録画は数えて出す（`rokuban_encode_reconcile_unsatisfiable`。プロファイルを改名すると、その名前で凍結済みの過去録画が一斉にここへ落ちる）。

**挙動の変更**: このパスが入るまで、25 回失敗して discarded になった encode ジョブはそこで止まっていた。これからは `encoded` が生まれない限り 15 分ごとに投入し直す（River の一意制約は pending 状態にしか効かず、discarded 済みの引数には合流しない）。真実は River のジョブ履歴ではなく `media_assets` の有無なのでレベルトリガーとしては意図通りだが、**恒久的に失敗するエンコードは「静かに諦める」から「延々と再試行する」に変わる**。

**窓を回す**: 候補は `recording_id` 昇順で 1000 件に切る。このパス自身は候補を減らさない（減らすのは encode の完了）ため、「毎パス先頭から」窓を開くと永久に満たせない候補（録画単位の恒久失敗）が先頭に溜まったとき、それより後ろの録画に到達できなくなる。これを避けるため、窓は前パスが止まった位置の続きから開き、末尾に達したら先頭へ戻る（再開位置はプロセスローカルで永続化しない）。1000 件はこれにより「1 パスのコストの上限」という意味だけを持つ純粋なつまみになり、被覆は候補集合の大きさに応じて有限パス数で完了する。窓が埋まったパスは `rokuban_encode_reconcile_candidates` ゲージと、`resume_after` フィールドを持つ Warn / pass-complete Info ログ（回転が実際に進んでいることを確かめる唯一の手段）で見える。再開位置を失う（プロセス再起動）と挙動は「毎パス先頭から」に戻るだけで、悪化はしない。

### 5.6 転送の途中経過を見せる

`recordings.status = finished` は **mirakc の録画完了**であって取り込み完了ではない。原本が
コミットされるまでブラウザ再生も事後エンコードもできないが、その時間帯を表すものが
`sizeBytes` の省略しか無かったため、遅い回線（実測で数百 KB/s 台）では**止まっているのか
進んでいるのか判別できなかった**。ingest worker は転送中に
`recording_ingest_progress`（[schema/recordings.md](../schema/recordings.md) §5 の衛星表）へ
書けたバイト数を写し、api はそれを `Recording.ingest` として返す。転送開始を表す
`written_bytes=0` は進捗の間引き時計に含めず、最初にファイルへ書けた値は直ちに記録する。
その後の更新だけを最短 2 秒間隔にし、継続ストリームで DB 書き込みが Copy バッファ単位に
増えないようにする。接続断を含め、バイトを書けた転送試行が終わるときは、間引かれていた
最新値を記録してから再開または終了する。0 バイトで終わった試行は進捗ではないので
`observed_at` を更新しない（`TestIngestWorker_ProgressVisibleDuringTransfer` /
`TestIngestProgressReporter_ThrottlesContinuedWrites` /
`TestIngestWorker_ProgressFlushesInterruptedBurst`）。

**進捗の置き場は衛星表**（ジョブ引数でも `record_sync` でもない）。理由は 3 つとも別方向:

- ジョブ引数（`river_job.args`）に持たせると UI が River の内部表を読むことになる
- `record_sync` は mirakc 側の観測で書き手は watcher。転送の進捗は Rokuban 側のファイルに
  何バイト書けたかなので、1 表 2 書き手になる（不変条件 12）
- `recordings` 本体の列にすると、書き手が脊椎（watcher / reconciler）でない状態が脊椎に
  混ざる（不変条件 13）

**分母は `record_sync.content_length`**（watcher が mirakc record の `content.length` として
観測済みの値）。転送開始時に読んで衛星表へ写す。HEAD の `Content-Length` は転送完了後の
照合（層 3）にしか取っておらず転送中には使えない。ファイル stat は api ロールが
ファイルシステムに触れない（不変条件 1）ので分母にできない。mirakc が length を返さない
record では分母を NULL のままにし、UI は % を出さずバイト数だけを出す（でっち上げた分母を
置かない）。この分母が録画中も非 null で時間とともに増えることは `TestConformance/RecordingInProgress`
が mirakc 4.0.0-dev.0 相当に対して判定している --- `records/{id}/stream` の `Content-Length`
ヘッダ（録画中は不明）とは別物であることに注意（上記「HEAD エンドポイントあり」参照）。
**録画中の最初の観測は 0 でありうる（実測）。** 0 は「mirakc が length を返さない」場合の
NULL とは違い、非 null な `*int64(0)` として `watcher.go` の `contentLengthPtr` を素通りする
ので、上記の NULL ガードでは捕まらない。0 を分母にした場合の UI の挙動は本稿の対象外。

**「リトライ中」を「取り込み待ち」と区別する値は API に持たない。** 区別するには
`river_job` を API 契約に露出させるか、失敗の観測という別寿命の値を進捗行に混ぜる
（不変条件 9 / 12）必要がある。代わりに `observed_at`（進捗を最後に観測した時刻）を返し、
停滞はその古さで読ませる。UI の停滞しきい値は 60 秒 —— 既定のストール検知
（`ingest.stall_timeout` = 30 秒）で正常に再接続している往復を「停滞」と呼ばないため
（`web/src/lib/ingest.ts` の `ingestStaleAfterMs`）。

**API の状態は 4 値で、原本 `media_assets` 行の有無を最優先に導出する**（列に焼いた値では
ない。`internal/api/recordings.go` の `ingestProgressFromFields`）。`kind='original'` の行が
`state` を問わず存在すれば `committed` —— `state='deleted'`（取り込んだ後に削除した）でも
`committed` のままにするのは、**「取り込めなかった」と「取り込んだ後に消した」を混同しない**
ため（[#211](https://github.com/fetburner/rokuban/issues/211) の症状。原本が**いま**あるかは
`sizeBytes` の有無が答える）。取り残された進捗行がコミット済みの録画に「取り込み中」を
名乗らないのも、この優先順位による（真実は `media_assets` 側。不変条件 5）。

**`pending`（取り込み待ち）の根拠は、watcher が ingest ジョブを投入する条件と同じ述語に
揃える**（`record_sync.status = 'finished'`）。`record_sync` 行の**存在**を根拠にしては
ならない —— 行は `failed` / `canceled` の record にも作られ、Rokuban はこの行を消さない
（本番に `DELETE FROM record_sync` の経路は無い）ので、ingest ジョブが一度も投入されない
録画が**永久に「取り込み待ち」を名乗る**。`pending` は「これから来る」の断定なので、
来る根拠が述語として書けないものを入れない（表示側の規律は
[frontend/recordings.md](../frontend/recordings.md)「取り込み中であることを画面に出す」）。

**途中ファイルのサイズを `sizeBytes` に混ぜない**（不変条件 3。コミット = DB 行）。
進捗は `ingest.writtenBytes` という別のフィールドで、原本 `media_assets` 行が生まれる
tx の中で進捗行が消える。

---

## 6. B-CAS 復号の責務境界

### 6.1 復号はエッジ（mirakc パイプライン）の責務

MULTI2 スクランブルの復号（B-CAS カードによる鍵処理 + デスクランブル）を実行するのは mirakc 本体ではなく、mirakc が編成する外部ツール。構成は 2 通り:

1. **チューナーコマンド段で復号**: `recpt1 -b25` 等がチューナー読み出しと同時に libaribb25 + PC/SC カードリーダーで復号。mirakc には `tuners[].decoded: true` を設定
2. **mirakc のフィルタで復号**: `filters.decode-filter` に arib-b25-stream-test（libaribb25 系）等を指定

どちらでも mirakc の録画パイプラインと Web API（ライブ・records `/stream`）から出る TS は**復号済み**。したがって **Rokuban のシステム内に暗号化された TS は一切現れない**。B-CAS カード・カードリーダー（pcscd）・libaribb25 の用意は ISDBScanner と同様、エッジ環境構築時のセットアップ事項としてインストールドキュメントで扱う（アーキテクチャの構成要素ではない）。

ドキュメントでは正規の B-CAS カード + PC/SC リーダー構成のみを扱う（カードエミュレーション類は法的にグレーなため扱わない）。

### 6.2 scrambled カウントは復号障害の検出器

ingest のインラインドロップスキャンで数える scrambling_control ビットは、復号が正常なら常にゼロのはず。**scrambled > 0 は放送品質でなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味するので、ドロップ数とは別枠のアラート対象とする（EPGStation ドロップログの scramble 列と同じ役割）。

---
