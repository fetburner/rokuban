> [recording.md](../recording.md) §5「ingest パイプライン」・§6「B-CAS 復号の責務境界」の一部。索引から辿る。

## 5. ingest パイプライン

mirakc のエッジから Rokuban のアーカイブストレージへ録画データを取り込む一連の処理。**録画開始と同時に始まり、録画に追従する。**

### 5.1 転送方式: API pull 固定

`records/{id}/stream` による HTTP pull を全構成で統一する。「monolith モードなら mirakc の basedir を直接読めるのでは」を検討し、**HTTP loopback 経由を維持**と結論した:

- **ディスク I/O は直読みでも減らない**。basedir（リングバッファ）→ メディアストレージのコピー自体は必要で、節約できるのは loopback TCP のオーバーヘッドだけ。1 日数本・数十 GB では無視できる。追従では各ポーリングが返すのが直前の間隔ぶんの伸び（放送レートで数 MB）なので、その読み出しはページキャッシュに乗っており実ディスク読みはほぼ生じない
- **コピー自体が耐障害設計**。録画はシステム内で唯一のリアルタイム・リトライ不能な操作である。そのためローカルディスクへ録画 → 完了後にリトライ可能な転送、という分離は崩さない（mirakc に最終保存先へ直接書かせる案は、録画中の NAS/FUSE ストールが放送の欠損に直結するため不採用）。ドロップスキャンも転送パスがあるからタダで載る
- **コードパスが 1 本**。HTTP pull は monolith / 分散 / ハイブリッドの全構成で動く唯一の方法
- **所有権が明確**。basedir は mirakc の所有物で、Rokuban は API 越しの客に徹する

**ingest は初回から Range で追従する。** `RecordFollowReader` は `Range: bytes=N-` を N=0 から送り、各応答をリクエスト時点のサイズまでの有限な差分として読む。先頭からの追っかけ再生だけは最初に mirakc の非 Range 追従配信を使い、その本文が閉じた後を同じ reader が Range で続ける。
初回から Range を使うことで、通常の追従と切断後の再開を同じ HTTP 契約で扱う。

**record の status が録画終了の真実である**（不変条件 5）。本文が空の Range に追い付いたときだけ `GetRecord` で status を読む。空ではない差分を読んだ直後には status を取り直さない。未知の status は終了とみなさず、連続失敗として再試行する。

`recording` なら待って Range を再要求する。通常の要求は 500ms 以上あけ、追い付いた状態が続けば 1 秒まで間隔を広げる。`finished` / `canceled` / `failed` の後は同じ offset へ最後の Range を送り、空であることを確かめる。`canceled` / `failed` は ingest の status hook がジョブをキャンセルし、temp を commit しない。

追い付いた Range は `204` / `416` / `206` の 0 バイトで返りうる。録画中は `ContentRange::without_size` が first/last を検査しないため 206 になる。これらの応答は接続障害として数えず、status を再取得する。未知の status、本文の切断、stall は共通の連続失敗回数とバックオフを使う。

`204` は content file がまだ 0 バイトの状態で、録画開始直後に出る。これも同じく待つ。

**Range を無視した `200` は offset 0 だけ受理する。** RFC 9110 はサーバが Range を無視して完全な表現を返すことを許す。offset 0 では本文が求めた差分と同一なので、そのまま受理してよい。

offset > 0 では本文が先頭から始まる。そのまま追記すると先頭から offset ぶんが二重になり、先頭を捨てて読むとポーリングごとに offset バイトを再転送する黙った O(n^2) になる。どちらも取らず、原因がログに残る形で失敗させる。フィルタを併用すると mirakc は Range を黙って無視するので、この分岐はその構成でだけ到達しうる（ingest はフィルタなしで pull する）。

**「同居時の basedir 直読み」は loopback が実測でボトルネックになった時の最適化オプション（YAGNI）。**契約は **mirakc とは常に API、自身のストレージとは常にファイルシステム**の 2 面で固定（[storage.md](../storage.md) 参照）。

### 5.2 TS 統計の解析ジョブ

ingest は mirakc の status・長さ・SHA-256 を確認して原本をコミットし、TS のバイト列は解釈しない。コミット後に `ts_scan` ジョブが active な原本をファイルから読み、188 バイト境界の統計を採る。ジョブのヒント投入が失敗しても、定期 reconcile が未計測の原本を拾う。

`media_asset_ts_scans` は計測したサイズを記録する。現在の原本サイズと一致する計測記録ができるまで、`until_encoded` の原本は削除対象にならない。解析後の全量読み出しにかかる追加コストは未検証である。

採取する統計:

- PID ごとの continuity counter 不連続
- transport_error_indicator
- scrambling_control

PID 別サマリを media_assets に紐づくテーブルへ格納し、UI で表示する。解析ロジックは `internal/tsstat`、呼び出し元は `internal/tsscan` の解析ジョブに限る。

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

**TEI パケットの CC を信用しない**。tspacketchk は TEI 時に error を数えつつ CC を
更新するため、直後のパケットで drop も 1 数える（1 つの破損が error と drop の両方に
計上される）。Rokuban は TEI 時に継続性の追跡を打ち切り、次のパケットで基準を
取り直す。破損の実数を二重に数えないことを優先した。

### 5.3 リトライ設計（3 層）

- `GET /records/{id}/stream` は **Range ヘッダー対応** → `Range: bytes=N-` で途中再開可能。`internal/mirakc/conformance` が mirakc 4.0.0-dev.0 相当に対して判定している。対象は `TestConformance/CompletedRecordStreamAndDelete`（完了後）と `TestConformance/RecordingInProgress`（録画中）である。録画中の Range 応答は `Content-Range: bytes N-M/*` のように総サイズが `*`（不明）になる（実測。Content-Length 自体は具体値を返す）
- **フィルタ併用時は Range を黙って無視して 200 になる**。ingest は素の TS が欲しいのでフィルタなしで pull する。
- Range を無視した 200 は offset 0 だけ受理する。途中再開の offset > 0 では拒否する（ソース確認。`mirakc-core/src/web/api/recording/records/stream.rs`。conformance テストはフィルタを併用しないので未判定）
- **HEAD エンドポイントあり** → 転送せず正確な Content-Length を取得できる。ただし録画中は Content-Length を返さない（`HeadRecordStream` は `-1`。黙って `-1` を長さとして使うと ingest がゼロ長ファイルを正しいものとして扱いかねない）。そのため HEAD を打つのは下記「層 3」のとおり録画完了後に限る。これらは mirakc 4.0.0-dev.0 相当に対して判定されている。`TestConformance/RecordingInProgress` は録画中に `-1` を返すことを判定している。`TestConformance/CompletedRecordStreamAndDelete` は完了後の Content-Length 一致を判定している

#### 層 1: 接続断の再開（ジョブ内リトライループ）

`RecordFollowReader` は読み取り中の本文が止まったときだけ stall timer を動かす。`ingest.stall_timeout`（既定 30 秒）を超えた本文を閉じ、temp に書けた offset から Range を再開する。消費側の `Write` が遅い間は `Read` が呼ばれないため、その時間を mirakc の stall と誤認しない。録画全体を 1 Work の総時間制限に収めず、4 分の slice ごとに snooze して再開する。

reader は `Range → 追い付き時の status → 待つ` を繰り返す。待ちはジョブ内（River にジョブを戻さない）なので、temp file と offset が同じ Work の中で生き続ける。slice が終わると層 2 が temp と SHA-256 state を保存して同じ job を snooze する。プロセス死で checkpoint が使えない場合も、temp を replay して状態を復元する。

**接続断のたびに進捗を書き出す。** Range 本文がバイトを返した後にエラー（読み取りエラー・stall）で終わったら、reader は再試行の前に最新の書き込み位置を `progress.flush` へ渡す。正常に終わった Range では呼ばない（追従中は 500ms ごとに Range が終わるので、間引きを無視すると秒 2 行になる）。temp への書き込みエラーは mirakc の一時障害として再試行せず、Work に返す。

リトライ上限は「連続した一時障害」の数である。数時間の録画中に散在する失敗で上限に達しないよう累積しない。`204` / `416` / `206` の 0 バイトは接続失敗ではない。連続 5 回まで再試行し、6 回目で Work を失敗させる。

**録画中の worker 再起動は temp の末尾から再開する。** 追従では録画中も ingest ジョブが走る。
`SoftStopTimeout` を超える SIGTERM でも、次のジョブは DB の進捗値を使わない。
同じ temp のサイズを Range の開始点にする。

#### 層 2: River によるプロセス死の回収

`IngestWorker.Timeout()` は 5 分、1 回の replay と転送の区切りは 4 分である。残り 1 分は HEAD、SHA-256 確認、fsync、DB commit に使う。区切りを有限にすることで、録画全体の長さや低速回線に依存せず、River が停止した Work を回収できる。

`worker.rescue_stuck_jobs_after` の既定は 6 分で、ingest の `Timeout()` 5 分と client の既定 `JobTimeout` 1 分より長い。River の JobRescuer は設定した閾値と worker 固有の `Timeout()` の長い方を待つため、プロセス死で `running` のまま残った ingest job は約 6 分で retry が予約される。長い worker timeout を持つ他の kind は、その timeout が経過するまで待つ。

生きている Work は 4 分の区切りで temp を fsync し、SHA-256 の binary state と offset を `.checkpoint` sidecar に保存して `river.JobSnooze(0)` を返す。snooze は同じ River job row を available に戻し、`attempt` を消費しない。次の Work は同じ job ID と record 固有 temp を使う。

checkpoint の offset が temp サイズ以下なら、保存した hash state を復元して残りのバイトだけを replay する。checkpoint が無い、破損している、または offset が temp サイズを超える場合は先頭から全量 replay する。replay 自体が区切りに達した場合も途中状態を保存して snooze するため、大きな temp の再試行が毎回先頭から始まらない。

SHA-256 state の書き込みと読み出しは temp の flock を保持したまま行う。checkpoint は temp と同じ削除条件に従い、mtime 猶予と aging による orphan 回収の対象になる。checkpoint が無効な場合に全量 replay へ戻すのは、状態ファイルだけが temp の正しさを決めないためである。

`--once` の KEDA Job は Work が snooze を返すと 1 件を消化した扱いで終了する。次の Pod が同じ job row を再開するため、各区切りで KEDA の検出・スケジューリング・Pod 起動の時間が加わる。4 分は replay と転送に割り当てる上限で、1 分の後処理を足した Work Timeout 5 分が rescue の既定 6 分より短くなるように選んだ。Pod 起動時間はクラスタとイメージ状態に依存するため、運用クラスタの起動時間を測り、通常の起動時間に対して十分長い区切りであることを確認する。

この経路では ingest の job-id advisory lock や `record_sweep` による River 行の生 SQL 回収を使わない。`record_sweep` は mirakc の record を定期取得し、欠けた ingest job の投入を補う。temp の flock は同じ record の書き込みを直列化し、公開時は DB の一意 reservation と rel_path lock が採用を決める。

SIGTERM による停止は River の Work context が `Canceled` になる。5 分の deadline 超過は `DeadlineExceeded` として失敗に数え、停止中断は最終結果のメトリクスへ数えない。checkpoint 保存が区切りに達する前の停止では、次の Work が最後の保存位置から残りを replay する。

#### 層 3: 完全性検証とコミット

pull 完了後に書き込みバイト数を HEAD の Content-Length と照合する。finished を観測した record では、同じ転送バイト列の SHA-256 と `content.sha256` も照合する。Range 再開を含む全バイトを 1 パスで計算する。これは stream レスポンスの Digest / ETag ヘッダーではない。

`content.sha256` が `null` または欠落している場合、finished 後の mirakc がハッシュを計算中の可能性がある。
finished record と HEAD の長さが temp のサイズに一致し、期限前なら、temp を同期して進捗行を消す。
同じ River job を snooze して待つため、worker 枠を占有しない。
次の Work は同じ job ID と temp を使い、毎回 temp を replay しない。
`TestIngestWorker_SHA256WaitSnoozesJobWithoutMetrics` は snooze 後の `attempt=0` を確認する。
`TestIngestWorker_CompletesUntilLateContentSHA256` は hash 到着後の `verified` commit を確認する。

待ちの期限は転送済みバイト数を速度見積もりで割って決め、mirakc の `recording.endTime` を起点にする。値が nil または未来なら temp の mtime を使う。報告された実測は 2.35 GB の読み直しに約 4 分（約 9.8 MB/s）で、25% の余裕を含めて 7.8 MB/s とする（計測点が 1 件のため設定キーにはしない）。下限は 10 秒で、地デジ 30 分（約 3.8 GB）では約 8 分、BS 2 時間（約 20 GB）では約 43 分になる。期限後もハッシュが無ければ `timeout_skipped` で commit する。HEAD の長さが不明（`-1`）なら転送完了を先に判定できないため待たずに commit する。実機での新しい待ち時間は未検証。期限の起点は `TestIngestSHA256WaitDeadline` で確認する。

空文字・空白付きの値は正規化し、64 文字の hex でない値は警告を出してスキップする。Content-Length が不明（`HeadRecordStream` が `-1`）なら長さの照合だけをスキップする（`ingest.go` の `expectedLen >= 0` ガード）。長さまたは SHA-256 が不一致なら `hash mismatch` / `size mismatch` で失敗し、commit と edge record の削除へ進まない。不一致は通常の River 再試行に戻し、専用メトリクス `rokuban_ingest_hash_mismatches_total` で観測する。

長さと（存在する場合の）SHA-256 の照合を通ったら、canonical rel_path と同じディレクトリに
作った record 固有 temp の `fsync` → `Close` を行う。区切りでの fsync（checkpoint 保存の直前）とは別に、replay
した既存部分を含めて完了時にも行う。バッファごとには行わない。

その後の短い確定区間で、media root の `rel_path` 固有 filesystem lock を取得する。
original の
`media_assets` 行を INSERT して rel_path の一意性を予約する。INSERT は
commit まで他セッションに見えない。transaction と filesystem lock を保持して temp を
canonical に atomic rename し、親ディレクトリを `fsync` する。その後 DB transaction を
commit する。**DB commit が公開点であり、mirakc 側の record 削除は commit 後だけ**である。

rename 前の失敗では temp を残して次の試行へ渡す。ただし長さ / SHA-256 の不一致と
`canceled` / `failed` は中身が不採用と確定しているので temp を消す。rename 後の親ディレクトリ
`fsync` または DB commit が失敗した場合は transaction を rollback し、canonical file は orphan
として aging 後に報告する。名前だけでは公開済みか判定できないため、自動削除はしない。
mirakc record は削除しない。rename と DB commit の順序を反転させて、
DB が指す実体を先に公開してはならない。

**`canceled` / `failed` で終わった record の途中までのバイトは資産にしない。** 理由は 3 つある。

- **original 行の存在は「その放送イベントは録れた」と読まれる。** ruler（`ListFulfilledProgramIDsBySite`）は status を見ずに original の有無で予約を外す。取消した番組を録画中に再予約した場合、前の試行の部分を commit すると、予約が外れて後継の録画が取り消される
- **エッジの部分ファイルは安定した事実ではない。** content path は番組ごとに決まり、mirakc は同じ番組の次の試行で同じファイルを切り詰めて書く。終端を観測した後の最後の drain は、後継のバイトを読みうる
- **後継の無い `failed` でも、完結していない原本の行は何も主張できない**（不変条件 10）。取消は予約を手放した結果であり、部分を残す意図を表す書き込みは存在しない

fsync を入れる理由は電源断だけではなく、Linux では遅延した書き込みエラー（ENOSPC / I/O エラー）が `Close` では報告されず `fsync` でしか上がらないためである。rename 後の親ディレクトリ `fsync` は新しい directory entry の永続化を確定する。ファイル `fsync` / `Close` / rename / 親ディレクトリ `fsync` のいずれかが失敗した場合は DB 登録も record 削除も行わず、ジョブを失敗させる。

rename 前の失敗なら、残った temp を replay して pull を続けられる。rename 後の親ディレクトリ `fsync` または DB commit の失敗では、temp はすでに canonical へ移動済みである。DB commit が成立しなかった場合、次の ingest は orphan 回収を待たずに全量 pull を開始し、同じ rel_path へ再度 rename する。残った canonical orphan は再試行の rename で置き換わるか、人が調査して回復・削除するまで残る。DB commit が成立して応答だけ失われた場合は、次の冪等性チェックで転送を省略する。いずれも mirakc record は削除せず、データ喪失は構造的に起きない。

運用上の主なリスクは**長時間の転送失敗でエッジのリングバッファが溜まり続ける**こと。`IngestWorker` 自体は River の既定の試行上限のままで、上限に達すると discard（dead-letter）されうる。それでも record が宙に浮かないのは、mirakc 側の record が DB commit 成功後にしか削除されないためである。discard された後も record_sweep（5 分周期の定期全量突き合わせ。[watcher.md](watcher.md) §3.3 の (c)）が同じ finished record を見つける。そして `processRecord` が同一トランザクションで ingest ジョブを再投入し続ける。「未 ingest の record 総量」をメトリクス化してエッジのディスク残量と突き合わせてアラートする（[storage.md](../storage.md) のサイジング指針参照）。

**帰結はディスクだけではない。** 滞留が `epg.retention_grace`（既定 24h）を跨ぐと、その録画の encode policy は予約から解決できず既定値で凍結される（エンコードが投入されない）。原本は残るのでデータは失われない。`recordings.source` と `rule_id` がどうなるかは、その録画の `recordings` 行が作られたのが GC より前か後かで分かれる。作成時にまだ予約が引ければどちらも通常どおり書かれ、影響は encode policy の凍結だけにとどまる。作成が GC 後にずれ込んだ場合は `rule_id` が NULL になり `source` も `unattributed` に落ちる。**このケースは下記 §5.5 の `encode_reconcile` でも回復しない**（desired が空になるので候補に入らない）。詳細と、滞留の型ごとに見るメトリクスが分かれること（**未 ingest 総量は回線断の滞留を数えない**）は [storage.md](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」と [operations.md](../operations.md) §4。

**`canceled` / `failed` の record は誰も回収しない。** ingest は commit しないのでエッジの record を消さず、mirakc にも record の保持期限は無い。後継の試行が同じ content path を上書きした場合、中身は後継の commit 時の purge で消え、record の JSON だけが残る。中身が残り続けるのは後継が無い場合（番組途中の失敗、再予約されない取消）だけである。このときエッジの部分ファイルは唯一のデータなので、消さないことを許容する。

**この record を `purge=true` で素直に回収してはならない。** mirakc の `remove_record` は content path を共有する別の record を見ずにファイルを消す。後継の録画のファイルまで消える。未解決: 同じ content path を持つ生きた record が無いものだけを回収する経路と、残った部分を手動で原本に採用する経路。どちらもまだ書き手が無い。

#### 冪等性: コミット済みなら転送をやり直さない

`media_assets` に `kind='original'` の行が既にコミットされていれば、ジョブは転送せず、エッジ record の削除だけを再試行して終わる（`IngestWorker.hasOriginalMediaAsset`）。エッジ record の削除は失敗してもログのみで ingest 自体は成功扱いにしているため、mirakc 側に record が残ったまま record_sweep 経由で同じ record の ingest ジョブが再投入されうる。ここで止めないと新しい試行が canonical file を置き換えて全量を再ダウンロードし、streamer は不変条件 3（コミット = DB 行）に反して欠けたファイルを配ることになる。

#### 同じ rel_path の競合: 一意 reservation で採用を決める

canonical path へ転送中のバイトが存在しないため、異なる record の ingest は、それぞれの
record 固有 temp へ並行して pull できる。同じ record は temp の flock で直列化する。
公開時は rel_path filesystem lock と DB の一意 reservation を使い、同じ canonical path の
ingest と通常削除の競合を直列化する。canonical orphan は削除せず、aging 後に報告する。
`checkRelPathConflict` / `GetLiveMediaAssetByRelPath`
は転送前の安価なヒントであり、転送中は lock を保持しない。

各 transaction の original INSERT が部分一意索引を予約する。先に INSERT した transaction が rename・親 directory `fsync`・DB commit を完了すれば、その内容が canonical file の勝者になる。後発 transaction の INSERT は先発の commit / rollback を待ち、先発が commit した場合は unique violation で失敗する。後発の temp は失敗時の規約に従って残るので、canonical file は勝者の内容のまま保たれ、残った temp は orphan 回収に委ねられる。通常削除は同じ rel_path filesystem lock を使うため、公開と unlink の競合は直列化される。転送前の `GetLiveMediaAssetByRelPath` はヒントであり、最終判断は一意索引と適用時の状態遷移に残る。

- **rel_path の filesystem lock は公開・通常削除の区間だけに使う。**
canonical path に直接転送しないので、転送全体の job lock heartbeat や lock 喪失による cancel は不要である。
ingest commit と通常削除は同じ `rel_path` の予約 lock file に対する POSIX `flock` を保持する。
対象区間は rename / unlink から DB commit までである。canonical orphan は公開状態を判定できないため、
自動削除せず aging 後に報告する。
- **未検証: RWX の media 越しの `flock`。** 別ノードの 2 レプリカ構成（`maxReplicaCount: 2` + RWX の media PVC）で、RWX 越しの `flock` 排他が効くかを確かめていない。効かなければ旧実行と代替実行が同じ temp へ書く。これは旧実行が生きたまま lock だけを失ったときに起きる。SIGKILL されたプロセス自身はもう書かないが、RWX ではカーネルが未書き込みのページを後から書き戻しうる（未検証）。heartbeat が応答待ち上限を超えて接続が閉じられる既存の窓に加え、lease 方式では 30 秒以上止まったプロセスと、DB から分断されたが生きている worker でも代替実行が走る。そのぶん当たる確率が上がる
- **同一録画の再試行**: Work は 4 分の transfer slice ごとに同じ River job を snooze する。プロセス死で running 行だけが残った場合は JobRescuer が retry を予約する。snooze では同じ job ID と record 固有 temp で再開し、temp の flock が同時書き込みを防ぐ。checkpoint が使えない場合は full replay に戻る
- **孤児と追加 I/O**: 中身の不一致または record の cancel / fail では temp を消し、それ以外の失敗では次の試行へ残す。ingest temp は mtime 猶予（既定 7 日）とエイジング（既定 14 日）の後に回収する。temp の回収は同じ flock に参加し、実行中の ingest と競合した場合は次の pass へ延期する。rename 後に残った canonical orphan は aging 後に報告し、自動削除しない。replay は同じ temp のローカル読み直しなので、scratch 経由の全長コピーは追加せず、追加コストは replay・temp の rename・親 directory `fsync` である

**弱い FS へ原本を直接書く設計は、FUSE の rename 非対応や fsync/Close の不確かな意味論に合わせるための将来課題へ戻した**。本 issue では `storage.media_dir` を強い FS に限定し、FUSE S3 は派生物専用の領域に限る。

### 5.4 負荷分担: worker

`records/{id}/stream` の負荷が乗るのは worker（ingest ジョブ、KEDA で 0〜N）であり、reconciler は数百件のメタデータ diff を回すだけの軽いジョブのまま。ただし**本当のボトルネックはクラウド側ではなくエッジ側**:

- ハイブリッド構成では自宅アップリンク帯域が律速。worker を増やしても速くならない
- エッジでは録画中の書き込みと pull の読み出しが同じディスクで競合する。pull がディスクを飽和させて録画をドロップさせるのは本末転倒

→ **ingest の同時実行数は mirakc サイト単位で `チューナー数 + 全速 pull の許容本数（1〜2）`** にする（`ingest.concurrency`。サイト別キュー or River の同時実行数設定）。worker の水平スケールが効くのは encode（CPU バウンド、入力はクラウド側ストレージ）の方。

**追従は番組長のあいだ ingest worker の枠を占有し続けない。** 4 分の Work ごとに snooze して枠を返し、次の Work が同じジョブを続ける。枠を返している間は全速 pull が使えるが、追従ジョブは優先度 1、追い付きは 2 なので、次に空いた枠は追従が先に取る。N 本の同時録画には N 個の追従枠が必要で、4 分ごとに一時的に追い付きへ渡る分の競合は残る。

**この余り枠は 2 つの仕事を兼ねる。** 追従後も全速 pull は残る（障害復旧後のバックログ・`record_sweep` の再投入・遅れて枠を得た追従の追い付き）。枠を録画数ちょうどにすると全速 pull が枠待ちで詰まり、逆に録画数に合わせて広げると復旧中の全速 pull が並列に走ってエッジの録画書き込みと競合する。N 本録画中は N 枠を追従が持ち、残りだけが全速 pull に回る。録画が無いときは全枠が全速 pull に回るが、そのときは邪魔する録画書き込みも無い。

**追従は追い付きより優先する。** watcher は投入時にどちらかを知っているので、River の `Priority` で追従を 1、finished 後の追い付きを 2 にする。バックログ消化中に始まった生録画の追従が、枠待ちで後ろに回らないようにするためである。

### 5.5 ingest 完了後のフロー

**同一トランザクションでの投入はしない**。`media_assets` のコミット**後**に、ベストエフォートのヒントとしてエンコードジョブを投入する（`IngestWorker.Work` → `EnqueueMissingEncodes`）。呼び出しは `ingest.go` の `enqueueMissingEncodesFromContext` である。投入に失敗してもログのみで、コミット済みの ingest は巻き戻さない。

**落としたヒントは定期パスが埋める**。ヒント投入の失敗とエッジ record の削除成功（`DeleteRecord`。上記「層 3」）が両方起きると、そのヒントは二度と飛ばない。エッジに record が残っていないので、record_sweep も ingest ジョブを再投入しない。ヒントだけに頼ると、コミット済みの録画が誰にも再投入されず黙ってエンコードされないまま残る。これを塞ぐのが `encode_reconcile` ジョブである（`internal/worker/encode_reconcile.go`、既定 15 分周期）。専用クエリが desired（`recording_encode_policy.encode_profiles`）− observed（active な `encoded` の `media_assets`）の不足分を一括取得する。取得するのは不足している `(recording_id, profile)` であり、それを River に投入する。真実は DB の状態であって「ヒントが飛んだかどうか」ではない（不変条件 5）。

対象は「原本（`kind='original'`）が active でコミット済み」かつ「ごみ箱に入っていない」録画に限る（ingest 未完了の録画とユーザーが捨てた録画を掘り起こさない）。エンコードは site の属性を持たない（アーカイブもプロファイルも単一）ので、このジョブは record_sweep のような site 単位ではなく全体で 1 本。`worker.periodic_jobs: false` の構成では、`rokuban enqueue encode-reconcile` を CronJob から叩く。一覧は [operations/monitoring.md](../operations/monitoring.md) の CronJob 一覧にある。

**thumbnail も同じ穴を定期パスで埋める**。ingest 完了後の thumbnail ヒント投入が失敗し、その後に `DeleteRecord` が成功すると、edge record が無いため record_sweep から再投入できない。この状態を `thumbnail_reconcile`（既定 15 分周期）が active な原本と active な thumbnail の差分として拾い、`thumbnail` ジョブを再投入する。対象は encode と同じく site 非依存で、ごみ箱の録画は除外する。原本が `missing_media_assets` に記録されている間は、ファイルが無いことが分かっているため定期パスから除外し、復旧後のマーカー解除を待つ。`worker.periodic_jobs: false` の構成では `rokuban enqueue thumbnail-reconcile` を CronJob から叩く。

定期パスは pending 中の thumbnail ジョブを River の一意制約で合流させ、抽出に失敗し続ける録画があっても候補窓を recording ID 順に回す。これにより同じ失敗を 1 パスごとに無制限に新規投入せず、後続の録画を恒久的に隠さない。明示的な `EnqueueMissingThumbnails` は復旧・テスト用の全件投入なので、ファイルを戻した直後の即時回収に使える。

**TS scan も定期パスで不足分を埋める**。`ts_scan` は active な original を先頭から全体読み、`tsstat.Counter` で `drop_stats` / `drop_positions` を置き換える。
`media_asset_ts_scans` には計測サイズを記録し、サイズが変われば再計測する。ごみ箱の録画と `missing_media_assets` の原本は候補から除外する。
計測済みの判定は view `current_ts_scanned_originals` にまとめてある。理由は [削除エンジン](../storage/retention.md) の「削除可否の述語に名前を与える」を参照。
候補は recording ID の keyset pagination で拾う。ページが上限に達したら、次のカーソルを持つ reconcile ジョブを投入する。
KEDA の `--once` で reconcile ワーカーが再起動しても、先頭に未計測の失敗が残る候補集合から後続ページへ進める。この動作は `TestTSScanReconcile_ContinuationSurvivesFreshWorker` で固定する。
scan の timeout は有限（6 時間。未検証）で、プロセスが死んだ `running` ジョブは River の rescuer が回収する。scan は冪等で再実行できるので、advisory lock は持たない。timeout が最長の原本より短いと、その原本の scan は timeout と再試行を繰り返す。

ingest の commit 後に scan をヒント投入し、既定 15 分の reconcile が取りこぼしを拾う。`worker.periodic_jobs: false` では `rokuban enqueue ts-scan-reconcile` を CronJob から実行する。scan は ingest の後に原本を全量読む。追加読み出しのコストは未検証である。

読み出しに失敗した scan に終端記録は作らず、後続の reconcile が再投入する。削除条件が scan 完了を要求するようになった後は、恒久的な読み出し失敗がある原本は保持され続ける。

**繰り返すパスは「投入しても必ず失敗する仕事」を作ってはならない**。ヒントは一度きりなので、設定から消えたプロファイルを投入して `unknown encode profile` で失敗させるのは、運用者への通知として妥当である。だが 15 分ごとに同じことをすると失敗を無限に作り続ける。定期パスは desired を**現在の `encode.profiles` に存在する名前だけ**に絞る。落とした録画は数えて出す（`rokuban_encode_reconcile_unsatisfiable`。プロファイルを改名すると、その名前で凍結済みの過去録画が一斉にここへ落ちる）。

**挙動の変更**: このパスが入るまで、25 回失敗して discarded になった encode ジョブはそこで止まっていた。これからは `encoded` が生まれない限り 15 分ごとに投入し直す（River の一意制約は pending 状態にしか効かず、discarded 済みの引数には合流しない）。真実は River のジョブ履歴ではなく `media_assets` の有無なのでレベルトリガーとしては意図通りだが、**恒久的に失敗するエンコードは「静かに諦める」から「延々と再試行する」に変わる**。

**窓を回す**: 候補は `recording_id` 昇順で 1000 件に切る。このパス自身は候補を減らさない（減らすのは encode の完了）ため、「毎パス先頭から」窓を開くと永久に満たせない候補（録画単位の恒久失敗）が先頭に溜まったとき、それより後ろの録画に到達できなくなる。これを避けるため、窓は前パスが止まった位置の続きから開き、末尾に達したら先頭へ戻る（再開位置はプロセスローカルで永続化しない）。1000 件はこれにより「1 パスのコストの上限」という意味だけを持つ純粋なつまみになり、被覆は候補集合の大きさに応じて有限パス数で完了する。窓が埋まったパスは `rokuban_encode_reconcile_candidates` ゲージで見える。`resume_after` フィールドを持つ Warn / pass-complete Info ログでも見える（回転が実際に進んでいることを確かめる唯一の手段）。再開位置を失う（プロセス再起動）と挙動は「毎パス先頭から」に戻るだけで、悪化はしない。

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
`observed_at` を更新しない。この挙動は `TestIngestWorker_ProgressVisibleDuringTransfer` /
`TestIngestProgressReporter_ThrottlesContinuedWrites` が固定している。
`TestIngestWorker_ProgressFlushesInterruptedBurst` も同じである。

**進捗の置き場は衛星表**（ジョブ引数でも `record_sync` でもない）。理由は 3 つとも別方向:

- ジョブ引数（`river_job.args`）に持たせると UI が River の内部表を読むことになる
- `record_sync` は mirakc 側の観測で書き手は watcher。転送の進捗は Rokuban 側のファイルに
  何バイト書けたかなので、1 表 2 書き手になる（不変条件 12）
- `recordings` 本体の列にすると、書き手が脊椎（watcher / reconciler）でない状態が脊椎に
  混ざる（不変条件 13）

**分母は `record_sync.content_length`**（watcher が mirakc record の `content.length` として
観測済みの値）。転送開始時に読んで衛星表へ写し、追従中は下記のとおり `GetRecord` の
`content.length` で更新する（開始時の値に固定すると録画中に 100% で止まる）。HEAD の
`Content-Length` は転送完了後の照合（層 3）にしか取っておらず転送中には使えない。ファイル stat は api ロールが
ファイルシステムに触れない（不変条件 1）ので分母にできない。mirakc が length を返さない
record では分母を NULL のままにし、UI は % を出さずバイト数だけを出す（でっち上げた分母を
置かない）。この分母が録画中も非 null で時間とともに増えることは、`TestConformance/RecordingInProgress`
が mirakc 4.0.0-dev.0 相当に対して判定している。`records/{id}/stream` の `Content-Length`
ヘッダ（録画中は不明）とは別物であることに注意（上記「HEAD エンドポイントあり」参照）。
**録画中の最初の観測は 0 でありうる（実測）。** 0 は「mirakc が length を返さない」場合の
NULL とは違う。非 null な `*int64(0)` として `watcher.go` の `contentLengthPtr` を素通りする
ので、上記の NULL ガードでは捕まらない。0 を分母にした場合の UI の挙動は本稿の対象外。

**「リトライ中」を「取り込み待ち」と区別する値は API に持たない。** 区別するには
`river_job` を API 契約に露出させるか、失敗の観測という別寿命の値を進捗行に混ぜる
（不変条件 9 / 12）必要がある。代わりに `observed_at`（進捗を最後に観測した時刻）を返し、
停滞はその古さで読ませる。UI の停滞しきい値は 60 秒である。既定のストール検知
（`ingest.stall_timeout` = 30 秒）で正常に再接続している往復を「停滞」と呼ばないためである
（`web/src/lib/ingest.ts` の `ingestStaleAfterMs`）。

**追従の待ちを「停滞」と呼ばない。** stall timer は Range 本文の `Read` 中だけ動く。信号断で録画ファイルが伸びない間は空 Range の後に status を読み、待って再試行する。消費側の逆圧は stall 判定に入らない。

**健全な status 観測は `observed_at` を進める（0 バイトでも）。** 追い付いている状態は止まっているのではなく、追従が正常な状態そのものである。observed_at を「バイトを書けたときだけ」進めると、**正常に追従できている録画ほど** UI の停滞判定（60 秒）に引っかかり「取り込み中（停滞）」と表示される。未知の status と失敗経路は進捗として記録しない。

間引きは既存の進捗書き込みと同じ最短 2 秒なので、追従中の DB 書き込みは録画 1 本あたり秒 0.5 行に留まる。これに接続断ごとの 1 行が加わる。正常 Range での flush は `TestIngestWorker_CleanRangeEndsDoNotFlushProgress` が禁じている。接続断の再試行はここを通らないため、**「0 バイトの試行は進捗ではない」という規律は失敗経路側に残る**（`TestIngestWorker_ProgressFlushesInterruptedBurst` が固定している）。

**進捗の分母は追い付き時の status 観測で更新する。** `record_sync.content_length` は watcher が観測した時点の値である。Work 開始時に固定すると、録画が伸びて `written_bytes` が分母を追い越し、UI が「取り込み中 100%」を録画中ずっと出し続ける。Web 側は `min(100, ...)` で頭打ちにするので、嘘が % として出る。

`GetRecord` は空 Range の後だけ呼ぶため、録画中の分母更新も追い付き時に限る。録画中は UI が % を表示しない。`TestIngestWorker_FollowingCaughtUpKeepsProgressFresh` が分母と observed_at の両方を固定している。

**録画中の分母は最終サイズではないので、UI は % を出さない。** 録画中に読めるのは「mirakc がその時点で観測しているサイズ」であり、`writtenBytes` がそれを追い越すことがある。割合にすると `min(100, ...)` で「録画全体を取り込み済み」と読める嘘になる。分母が確定するのは録画終了後で、% はそこから出す（`web/src/lib/ingest.ts` の `ingestDisplay`）。分母が NULL のときも同じくバイト数だけを出す。`content.length` は照合に使わず、finished 確認後に HEAD と `content.sha256` を照合する。SHA-256 が無い場合は、HEAD の長さが分かる間だけ期限までジョブを完了して待ち、期限後または長さ不明なら `timeout_skipped` で commit する。

**API の状態は 4 値で、原本 `media_assets` 行の有無を最優先に導出する**（列に焼いた値では
ない。`internal/api/recordings.go` の `ingestProgressFromFields`）。`kind='original'` の行が
`state` を問わず存在すれば `committed` である。`state='deleted'`（取り込んだ後に削除した）でも
`committed` のままにするのは、**「取り込めなかった」と「取り込んだ後に消した」を混同しない**
ためである（原本が**いま**あるかは
`sizeBytes` の有無が答える）。取り残された進捗行がコミット済みの録画に「取り込み中」を
名乗らないのも、この優先順位による（真実は `media_assets` 側。不変条件 5）。

**原本の次に優先するのは「record が `failed` / `canceled` で終わった観測」である。**
この観測は進捗行より先に見る。worker は cancel / fail の観測後に進捗行を消し、ジョブを終端する。
DELETE が失敗してもログだけで続行するため、進捗行が残ることがある。
残った行があると、二度と取り込まれない録画を「取り込み中（停滞）」と表示する。
**この述語は `has_ingestable_record` の否定にしてはならない。**
未知の status では共有 reader が再試行を続けるため、進捗行は生きた観測である。
`TestIngestWorker_UnknownStatusRetriesInJobWithoutRestart` がこの動作を固定する。
終端する status の集合は worker の `errIngestRecordEndedAbnormally` と一致させる。

**`pending`（取り込み待ち）の根拠は、watcher が ingest ジョブを投入する条件と同じ述語に
揃える**（`record_sync.status` が `recording` または `finished`）。`record_sync` 行の**存在**を根拠にしては
ならない。行は `failed` / `canceled` の record にも作られ、Rokuban はこの行を消さない
（本番に `DELETE FROM record_sync` の経路は無い）。そのため ingest ジョブが一度も投入されない
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

原本解析で数える scrambling_control ビットは、復号が正常なら常にゼロのはず。**scrambled > 0 は放送品質でなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味する。そのためドロップ数とは別枠のアラート対象とする（EPGStation ドロップログの scramble 列と同じ役割）。

---
