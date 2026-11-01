> [storage.md](../storage.md) §3 の一部。索引から辿る。

## 置き場間の転送

複数の置き場に同じ論理資産を置くための転送契約を示す。宛先は FS の置き場と、派生物の置き場の S3 である。DB の判定、帯域、公開順、削除条件を扱う。

転送 worker は、マウントした FS 置き場のキューだけを購読する。宛先が FS の置き場なら、宛先の root と目印を確認してから、DB に pending のコピー行を予約する。宛先がマウントされていない間は予約せず、転送試行にも数えない。宛先が S3 の派生物の置き場なら、転送元の FS 置き場の worker が実行する（次の節）。

### 宛先が S3 の派生物

派生物の置き場が S3 のときは、転送元の FS 置き場の worker が S3 へ直接送る。宛先が S3 のジョブは、転送元の置き場の転送キューに積む。配信の route と ScaledJob は増やさない。

派生物の置き場が S3 のときは中央を中継しない。S3 は API で直接書けるので、中継は要らない。中継すると、データが WAN を 2 回通る（転送元から中央へ、中央から S3 へ）。配信の route と ScaledJob も置き場ごとに増える。

帯域は、転送元の置き場の `storage.transfer.bandwidth` で絞る。S3 宛ての送信も同じ上限を通る。宛先の目印は確かめない（S3 の宛先には root が無い）。転送元の FS 置き場の転送 worker には、S3 の資格情報が要る（[ストレージ運用](../operations/storage.md) の「S3 の置き場の運用要件」）。

宛先の判定は、FS の置き場の集合か、派生物の置き場（`p_derived_store`）に一致するかで行う。転送元の集合は FS の置き場のままである。S3 の古い派生物のコピーを転送元に数えると、マウントしていない置き場から読む候補が残り続けるからである。原本の宛先は FS だけである。

転送の宛先が 2 つ未満のとき、転送も配置の削除も走らない。FS の置き場 1 つと S3 の派生物の置き場は、2 つに数える。

### 排他と再開

転送は転送元ごとに 1 本だけ走らせる。1 回の Work は slice（4 分）だけ続け、時間が来たら進捗を保存して snooze する。次の Work が保存された位置から続ける。原本と派生物で手順は同じである。Work の締切（5 分）は slice より長い。締切が先に来ると、slice の終わりの保存より前に ctx が切れるからである。

保存は親の ctx と独立した ctx（5 秒）で行う。SIGTERM や締切で ctx が切れた Work も、保存してから抜ける。lease の期限は slice + 2 分（6 分）で、締切より後まで残す。締切で止まった Work が最後の保存を終える前に、lease が切れないようにするためである。

転送は転送元ごとの advisory lock を、pool から借りた接続のまま保持する。pool の外に接続を張ると、`db.max_conns` がプロセスの接続上限だという契約を破るためである。この接続の数は pool の予算に数える（[データベース](../operations/database.md)）。

転送元の root の目印を照合してから読み、コピー単位の DB progress lease を取る。temp は copy ID と世代を含む名前で分離する。全体一意索引が `(store, rel_path)` の再利用を防ぐ。

進捗（lease、世代、保存した長さと SHA-256 の状態、送信状態、照合中であること）は、pending のコピーの衛星表に持つ。lease の期限は DB の `now()` と比べ、プロセスの時計は使わない。

temp は宛先と同じディレクトリに `.rokuban-transfer-{media_asset_id}-{copy_id}-g{generation}` の名前で作る。世代はきれいな引き継ぎ以外では上がる。同じ temp を続けるのは、前の試行が保存して lease を手放した後だけなので、2 つの試行が同時に同じ temp に書くことはない。転送は flock を使わない。

前の試行が進捗を保存して lease を手放したとき（きれいな引き継ぎ）だけ、同じ世代の temp の続きから書く。転送元は保存された長さから開く。temp の長さか SHA-256 の状態が保存と合わないときは、この試行を失敗として数えて止める。次の試行は世代を上げて先頭から取り直す。

前の試行が保存せずに抜けて lease が切れただけのときは、世代を上げて先頭から取り直す。これは中断であり、失敗としては数えない。

転送元の接続が切れるなど、コピーの途中で失敗した試行も、書いた分を保存してから失敗を記録する。次の試行は切れた位置から続ける。先頭から取り直すと、切れやすい経路では大きな原本の転送が終わらない。

再開のとき、転送元は保存された位置から開く。先頭は読み直さない。slice の合間に temp を読み直してハッシュを取り直さない。保存した長さまでのバイトが残っていることは、「fsync してから DB に保存する」順序で保証し、再開時は長さだけを確かめる。きれいな引き継ぎなのに長さや状態が合わないのは、この順序が破れたことを示すので、試行の失敗として数える。完了時の SHA-256 の照合は、転送経路と再開手順の誤りを、気づかれないまま壊れたコピーとして残すのではなく試行の失敗として表に出すためのものである。

未解決: 宛先の中身が、書き込みの後、転送元が prune されるまでの間に壊れても検出しない（コピーの active 化は中身を読み直さない）。

### 派生物の再開できる送信

派生物は、宛先と転送元の両方が続きから読み書きできる置き場なら、再開できる送信で送る。FS と S3 はどちらもそれを満たす。両方がそろわない組み合わせは、従来の経路（一度に送り切る）を通る。

判定は、宛先が `objstore.Resumable`、転送元が `objstore.RangeReader` を満たすかで行う。送信状態は進捗の行の `upload_store` と `upload_state` に、置き場の名前と不透明なバイト列で保存する。S3 の状態は UploadId、パートの大きさ、送り終えたパートの番号と ETag の一覧、SHA-256 の途中状態を持つ。FS の状態は staging の相対パス、offset、SHA-256 の途中状態を持つ。

パートの一覧の正は DB である。UploadPart が成功するたびに一覧へ積み、Complete は保存した一覧で行う。Complete では ListParts を使わない。ListParts は掃除の Abort のあとの確認にだけ使う。パートの大きさは開始時に決めて続きで変えない（`multipartPartSize`）。送り元の区切りが再開のたびに変わらないようにするためである。

きれいな引き継ぎ（同じ置き場の送信状態が保存され、保存で lease を手放したあと）だけ続きを送る。保存せずに抜けた試行、置き場が違う送信状態、続けられない送信状態は、新しいアップロードから始める。古い送信状態は置き換えのときに掃除待ちへ移る。転送元の長さが宣言と違うと分かった試行も、ずれたバイトを含む送信状態は保存しない。

続けられないと分かったとき（`ErrUploadInvalid`）は、送信状態を保存してから lease を切り、この試行を失敗として数える。Complete が InvalidPart などで落ちたときも、`ErrUploadInvalid` に変換して同じ扱いにする。次の試行は新しいアップロードで最初から送る。古い試行の送信中のパートが同じ番号を後から上書きすることがある。上書きされたパートの ETag が保存した ETag と合わなければ、Complete が InvalidPart で落ちる。同じ番号のパートを送り直しても ETag が一致するとは限らない（SSE-KMS などでは ETag が MD5 ではない）ので、続けずに取り直す。

進捗の行が消えると（完了、破棄、回収、コピーの CASCADE）、送信状態は DB の trigger で掃除待ちの表に移る。掃除は転送の Work の冒頭で、その宛先の置き場の分だけ行う。掃除待ちの行ごとに置き場の `AbortUpload` を呼び、置き場が `gone` と答えた行だけを消す。S3 では Abort のあとの ListParts が NoSuchUpload を返したときが gone で、FS では staging が無いときが gone である。掃除全体は短い締切（30 秒）で区切り、超えた残りは次の Work に回す。

S3 の Abort は 1 回で済むとは限らない。AWS のリファレンスは、Abort のあとにパートが残りうるので ListParts で確かめるよう勧めている。そこで Abort が成功しても gone とは扱わない。Abort のあとの ListParts が NoSuchUpload を返すまで記録を残し、次の Work で繰り返す。掃除が失敗しても転送は止めない。記録の無い送信状態は掃除しない。

FS の staging は、書かれないまま mtime 猶予と aging を過ぎると削除 reconcile の孤児回収が消す。孤児回収は進捗の行を見ない。消えた staging の続きは `ErrUploadInvalid` になり、1 回の失敗として数えて新しいアップロードから始まる。

未解決: 回収で掃除待ちに移った送信状態は、その置き場への転送の Work が次に走るまで Abort されない。規則の変更で置き場への転送が止まると、掃除待ちが残る。S3 では、bucket のライフサイクル規則（[ストレージ運用](../operations/storage.md) の「S3 の置き場の運用要件」）が最後の防御になる。

### SHA-256 の測定

原本は、宛先がまだ無ければ送りながら SHA-256 を計算し、完了時に論理資産へ書く。先に測らない。宛先が既にあって論理資産の SHA-256 が未知のときだけ、転送元を先に測る。

原本の宛先が既にあるときは、宛先は読まずサイズだけを見て、転送元を slice ごとに測る（`measureAdoptedSource`）。測り終えたら観測値を書き、次の Work が既知の経路で宛先と比べる。

派生物で論理資産の SHA-256 が未知なら、送る前に転送元を slice ごとに測る（`measureDerivedSource`）。宛先には書かず、scratch にも複製しない。測り終えたら観測値を書き、次の Work が既知の経路で送る。

測定の途中経過は、進捗の行の offset と hash state に入る。その値は転送元の先頭から offset までの SHA-256 で、コピーの途中経過と同じ意味である。測定の続きにそのまま使える。観測値は、SHA-256 が NULL で世代が同じ行にだけ書く。世代が変わっていれば書かず、その試行は古いものになる。

測定の間に転送元が変わったときは、送るときのハッシュ照合で合わなくなる。その後の扱いは次の節と、派生物の不一致の扱いに従う。

### 原本の不一致の照合

原本の不一致（送ったバイトのサイズや SHA-256 が論理資産と合わない、既存の宛先の内容が違う）を見つけたら、失敗として数える前に照合へ入る。照合は転送元を offset から末尾まで slice ごとに読み直し、SHA-256 とサイズを求める。temp は使わず、照合に入るときに temp を消す。

照合中であることは進捗の行の `verifying_source` で表す。途中経過は offset と hash state に入り、lease の引き継ぎをまたいで続く。

照合が末尾まで終わったときに、試行を 1 回だけ数える。合わなかった理由は失敗の文言にだけ出し、DB には残さない。転送元が論理資産と合うなら、宛先の temp の破損として数え、次の試行が取り直す。転送元が論理資産と合わないなら、転送元の不一致として記録し、原本は残す。

数えるのを照合の後にするのは、不一致の時点で数えると、上限の直前の不一致で照合が二度と回らないためである。

派生物には照合の段階が無い。転送元の再検査は全長を読み、その場で判定する（`handleTransferMismatch`）。

### 照合と公開

コピーの完了時に、期待するサイズと SHA-256 を照合する。論理資産の SHA-256 が未計測だった原本は、ここで送ったバイトの値を書く。値があるときは書き換えず照合だけを行い、合わなければ完了を拒否する。

原本の既存宛先は、転送 temp と同じ inode なら前回の公開として採用する。inode が違う場合も、サイズと SHA-256 が一致するときだけ採用する。派生物のキーは、サイズと SHA-256 が一致すれば再開に使う。転送が新規作成したキーは所有記録を付け、同じ pending のコピーの再試行だけが再利用する。所有記録のない既存のキーも内容が一致すれば採用できるが、commit が拒否されたときに削除しない。

派生物は、再開できる送信で宛先へ送る（前の節）。`CompleteUpload` は送りながら計算した SHA-256 と宣言が合ったときだけ公開するので、合わなければ何も公開されない。SHA-256 が既知の派生物は scratch に複製しない。

内容が異なる既存のキーは、動画も encoded の字幕も、所有記録の有無にかかわらず衝突として扱う。転送は失敗として記録し、そのキーは消さない。原本で既知の SHA-256 が合わないときは、転送元を slice ごとに読み直して照合し（前の節）、原本の宛先 temp の破損と転送元の破損を区別する。異なる動画を rename や link で上書きしない。

原本は、temp の fsync と Close、正規名への上書きしない link、親ディレクトリの fsync を行う。その後に DB transaction で宛先のコピーを active にする。DB commit が公開点である。

commit が拒否されたとき、原本ではこの転送の temp と同じ inode の宛先を消し、派生物では所有記録のあるキーだけを消す。派生物の字幕 sidecar は本体より先に消し、sidecar の削除に失敗したら本体と pending 行を残して次回に再試行する。拒否は世代や規則の変化、または記録済みの SHA-256 との不一致で起きる。派生物は Put 成功後に所有記録を commit するため、その間にプロセスが落ちたキーは未記録のまま残り、回収では他の書き手のキーと区別できないので消さない。

採用する宛先が、この転送の temp と同じ inode なら、採用と同時に temp を片付ける。inode が違う宛先は他の書き手が置いたものなので、temp に触れない。同じ pending 行を別の worker が先に active にしたときは、そのコピーを完了済みとして扱い、公開済みの宛先と sidecar を消さない。

encoded の隣接 `.vtt` は、本体のキーを作った後、コピーを active にする前に、宛先のキーへ作る。転送元に sidecar が無いときは、この転送が作った（所有記録のある）宛先の sidecar だけを消し、所有記録のない sidecar は残す。字幕は独立した `media_assets` 行ではないので、動画のコピー状態を commit する前に sidecar を揃える。字幕は小さいので、slice にせず従来どおり scratch に複製してから置く。

### 失敗と壊れた転送元

転送の失敗は宛先ごとに記録して再投入を抑え、壊れた派生コピーは作り直せるときだけ消す。

失敗の理由は宛先のコピーごとの試行表に記録し、上限に達すると再投入を止める。止まっている件数はゲージ `rokuban_storage_transfer_exhausted_assets` で見る。宛先の置き場や世代が変わると、古い試行記録を消す。記録済みの SHA-256 と合わない原本は報告だけで、転送元を残す。

派生コピーが壊れていたときは、同じ録画に active・実体ありの原本コピーが残る場合だけ、壊れた転送元のコピーを退役させる。転送 worker は録画の行、派生の論理行の順にロックした 1 つの transaction で、転送元のコピーを deleting にする。この順序は until_encoded の原本削除と同じで、両者を直列化する。直列化しないと、原本の削除と派生コピーの退役が同時に通り、どちらの実体も残らない。

退役の続きでは、目印を照合してからファイル（encoded なら字幕の sidecar も）を消し、親ディレクトリを fsync して deleted にする。

deleted への確定は、コピー自身の置き場の行をロックして行うので、転送 worker が束縛された置き場と違う置き場の転送元でも確定できる。同じ transaction で、実体無しの記録と転送の試行行も消す。退役させたら `rokuban_storage_transfer_corrupt_sources_retired_total` を増やし、Error ログを出す。deleting にして unlink の前に落ちた場合は、削除 reconcile がどの腕にも該当しない deleting 行として active に戻し、転送の再試行が不一致を再検出して消す。原本が無ければコピーを残して報告し、試行回数を記録する。

退役した encoded のコピーは、encode の補完の入力である `unencoded_desired_profiles` に行として現れる。この行から encode が再生成するところまでは確かめていない。thumbnail とシークタイルが再生成されるかも未検証である。

未解決: 原本で SHA-256 が既知の宛先が既にあるときの照合、派生物の宛先キーの照合のうち SHA-256 を返さない置き場（FS）の分、派生物の転送元の再検査（`handleTransferMismatch`）は、全長を読む。slice になっていない。S3 の宛先キーは Head の SHA-256 で比べ、中身を読まない。その SHA-256 はオブジェクトに付けたメタデータで、中身を読み直して確かめてはいない（未検証）。

### 不要になったコピーの回収

不要になった pending のコピーは、削除 reconcile が回収する。

ごみ箱・世代・desired の置き場の変更、または until_encoded の保持条件の成立で不要になった pending のコピーは、削除 reconcile が回収する。候補を列挙した後、録画・論理資産・コピーの行をロックした transaction で不要になった条件を再評価する。

原本の回収では、正規名の宛先を転送 temp と inode が一致するときだけ消す。派生物の回収では、所有記録のあるキーだけを消す。所有記録のない宛先は残す。いずれも字幕 sidecar を本体より先に消し、sidecar を消せなかったときは本体と pending 行を残して次回に再試行する。

回収が終わった pending 行は、転送 temp も含めて退役させる。進捗の行も消えるので、その送信状態は掃除待ちに移る。今も desired の pending のコピーは、失敗回数と進捗からの再開を保つために残す。最後の pending のコピーが退役すると、録画の完全削除（purge）が完了する。

desired の置き場から外れた配置は、削除 reconcile の腕の 1 つとして消す。腕は名前付き述語 `media_asset_copy_prunable` で、desired の置き場に active・実体ありのコピーがあり、overlap を過ぎていることだけを条件にする。desired のコピーは別の置き場の実体あるコピーなので、最後のコピーを消さないことをこの条件が守る。

腕を参照するのは、新規候補（active → deleting）の入口、deleting のまま止まった行の拾い直し、拾い直しの否定形（active に戻す判定）である。入口は `internal/db/queries/storage_transfer.sql`、残りは `internal/db/queries/delete_reconcile.sql` にある。入口は `media_assets` 行を lock してから deleting にするので、規則の切り替えが並走しても両方のコピーは消えない。deleting のまま落ちた行は、拾い直しが unlink を終える。その間に desired のコピーが実体無しと記録されたときは、否定形が active に戻す。

転送 temp は、孤児監査で予約ファイルとして分類し、コピーの進捗が指す世代で回収する。flock は取らない。進捗の行が無い temp は、mtime の猶予と aging の後に消す。

<details><summary>根拠</summary>

- 再開、hash、上書きしない公開、復旧後の採用: `TestStorageTransferWorkerCopiesAndAdopts`
- 自分の temp の片付け、他の書き手の宛先を採用するときの temp の保持、完了時の進捗の行の消去: `TestStorageTransferWorkerCopiesAndAdopts`
- 保存された状態から、同じ世代の続きを転送元の残りだけから読む: `TestStorageTransferWorkerResumesSavedProgressWithoutReadingSourcePrefix`
- 保存せずに抜けた試行は、世代を上げて先頭から取り直し、失敗として数えない: `TestStorageTransferWorkerRestartsFromSourceWhenPreviousAttemptFailed`
- 長さや状態が合わない引き継ぎは失敗として 1 回数える。次の試行は世代を上げて取り直し、合わなかった世代の temp を消す: `TestStorageTransferWorkerRestartsWhenSavedTempLengthDiffers`
- 転送元の接続が切れて失敗した試行の次は、切れた位置から Range で続ける: `TestStorageTransferWorkerResumesFromWrittenBytesAfterRemoteDisconnect`
- コピーを pending でなくす transaction と並行に進捗を取っても、行を作らない: `TestTransferProgressAcquireWaitsForConcurrentCopyRetirement`
- slice の時間で止まって保存し snooze し、繰り返すと完了する: `TestStorageTransferWorkerSliceStopsAndResumesToCompletion`
- slice が止まったとき自分の lease が切れていて保存できなければ、成功で終えず snooze する: `TestStorageTransferWorkerSnoozesWhenOwnLeaseExpiredDuringSlice`
- lease を別の試行に取られた試行の保存を拒む: `TestTransferProgressZombieSaveIsRejected`
- lease が切れた後の、持ち主自身の保存を拒む: `TestTransferProgressSaveAfterOwnLeaseExpiryIsRejected`
- 進捗の行は、コピーが pending でなくなるときに消える: `TestTransferProgressRowIsDeletedWithPendingCopy`
- 古い世代の temp は回収し、今の世代の temp と猶予内の進捗の無い temp は残す: `TestTransferTempOrphanCleanupFollowsProgressGeneration`
- temp 名の旧形式と新形式の判定: `TestParseStorageTransferTempFile`
- 記録済みの SHA-256 を上書きしない: `TestCompleteMediaAssetTransferRejectsChangedSHA`
- 内容の違う既存の sidecar の衝突、source に sidecar が無いときは所有記録のある sidecar だけを消す: `TestStorageTransferWorkerEncodedSubtitleKeys`
- SHA-256 が既知の派生物を scratch に複製せず、再開できる送信で送る: `TestStorageTransferWorkerStreamsKnownSHADerivedWithoutScratch`
- 宣言サイズや SHA-256 と違う転送元は何も公開せず、転送元の不一致にする: `TestStorageTransferWorkerStreamedDerivedSourceMismatchPublishesNothing`
- 壊れた派生コピーのファイルと sidecar の削除、原本が無いときの保持: `TestStorageTransferWorkerRetiresCorruptDerivedSourceOnlyWhenOriginalCanRebuild`
- 束縛と違う置き場の転送元を deleted に確定する: 同じテスト
- 退役した encoded のコピーが `unencoded_desired_profiles` に現れる: 同じテスト
- 退役と until_encoded の原本削除の直列化: `TestStorageTransferCorruptRetirementSerializesWithUntilEncoded`
- 配置の削除時の sidecar 削除: `TestDeleteReconcilePlacementDeletionRemovesEncodedSubtitleSidecar`
- S3 切替後の旧 FS の派生物の削除: `TestDeleteReconcileWorker_StaleFilesystemDerivedCopyIsDeletedThroughItsFilesystemStore`
- deleting の配置コピーを拾い直して終わらせる: `TestDeleteReconcilePendingPathFinishesPlacementDeleting`
- desired が実体無しになった deleting の配置コピーを active に戻す: `TestDeleteReconcileRevertsPlacementDeletingWhenDesiredMissing`
- 規則を A → B → A と切り替えても有効なコピーが 0 にならない: `TestStorageTransferPolicyRoundTripConverges`
- desired のコピーが無いとき、最後のコピーを消さない: `TestStorageTransferDeletionRequiresDesiredPresentCopyAndOverlap`
- 削除時の論理行の lock と規則の再判定: `TestStorageTransferDeletionSerializesPolicySwitches`
- 不要になった試行記録の削除: `TestStorageTransferReconcileReportsExhaustionAndClearsObsoleteAttempts`
- pending のコピーの退役後の録画の完全削除（purge）: `TestDeleteReconcileReclaimsObsoletePendingFSCopyAndAllowsPurge`
- 既にあった宛先の保持: `TestDeleteReconcilePreservesPreexistingDestinationAfterPendingTransferReclaim`
- sidecar の削除に失敗した後の pending の回収の再開: `TestDeleteReconcileRetriesPendingReclaimWhenSubtitleRemovalFails`
- until_encoded で不要になった pending の原本コピーの退役: `TestDeleteReconcileReclaimsPendingOriginalAfterUntilEncoded`
- until_encoded の条件のロック後の再判定: `TestStorageTransferPendingReclaimRechecksUntilEncodedEligibility`
- 転送 lock の接続を pool に戻さない条件: `TestTransferBandwidthLeaseHijacksOnUnlockFailure`
- 途中で止めて続きから送ると、同じバイト列のオブジェクトになる（置き場ごと）: `checkResumableStopAndContinue`。`internal/objstore/contracttest` にあり、fsstore と s3store の適合テストから呼ぶ
- 宣言と違うバイトは何も公開しない: `checkResumableWrongDigestPublishesNothing`（同じ）
- Abort は残骸なしと答えるまで繰り返す: `checkResumableAbortRepeatsUntilGone`（同じ）
- Abort が成功しても、ListParts が NoSuchUpload を返すまでは gone にしない: `TestAbortUploadIsGoneOnlyWhenListPartsSaysNoSuchUpload`
- Abort が成功しても ListParts が成功するなら gone にしない: `TestAbortUploadWithRemainingPartsIsNotGone`
- ListParts の失敗は gone と誤らず、資格情報の分類で返す: `TestAbortUploadReturnsListPartsErrorWithCredentialClass`
- Abort が NoSuchUpload 以外で失敗したら、ListParts を呼ばずに返す: `TestAbortUploadSkipsListPartsWhenAbortFails`
- MinIO で、Abort のあとの ListParts が NoSuchUpload を返す（適合テストの機能 `multipart-abort-gone`）: `TestS3Conformance/multipart-abort-gone`
- 転送元の長さが宣言と違う送信の扱い: `checkResumableSourceLength`（同じ）
- slice をまたいで派生物を送り続け、完了する: `TestStorageTransferWorkerResumesDerivedUploadAcrossSlices`
- きれいでない引き継ぎは新しい UploadId で送り直し、古い送信状態を掃除待ちに移す: `TestStorageTransferWorkerRestartsDerivedUploadAfterUncleanHandoff`
- Complete が InvalidPart で落ちたら最初から送り直す: `TestStorageTransferWorkerRestartsDerivedUploadWhenCompleteIsInvalid`
- 転送元の長さが宣言と違う試行は送信状態を保存せず、次の試行は新しい UploadId と staging から始まる: `TestStorageTransferWorkerRestartsAfterSourceLengthMismatch`
- 掃除待ちの送信状態は、置き場が gone と答えるまで Abort を繰り返す: `TestStorageTransferWorkerKeepsAbandonedUploadUntilStoreReportsGone`
- 掃除が締切で止まったら、残りの掃除待ちの行に触れず、転送は次の Work で完了する: `TestStorageTransferWorkerCleanupStopsWhenItRunsOutOfTime`
- 未計測の派生物の転送元を slice で測り、送る前に観測値を書く: `TestStorageTransferWorkerMeasuresUnknownDerivedSourceBeforeSending`
- 宛先が既にある未計測の原本を slice で測ってから採用する: `TestStorageTransferWorkerMeasuresUnknownSourceAcrossSlicesBeforeAdopting`
- 照合を slice をまたいで行い、失敗は照合の後に 1 回だけ数える: `TestStorageTransferWorkerVerifiesMismatchAcrossSlicesBeforeCountingFailure`
- 上限の直前の不一致でも照合が回り、失敗が 1 回数えられる: `TestStorageTransferWorkerCountsVerifiedFailureAtAttemptLimitEdge`
- S3 の派生物への転送は、転送元の FS 置き場の worker が slice をまたいで完了する: `TestStorageTransferWorkerSendsDerivedCopyToS3FromFSSource`。宛先の root は無い
- MinIO での同じ経路。5 MiB 以上のパートを slice をまたいで送る: `TestS3DerivedObjectPublicationAndRecovery/FS_derived_copy_transfers_to_S3_across_slices`
- 完了した upload の送信状態は、掃除で gone になり行が消える（実 MinIO）
  - 検査: `TestS3DerivedObjectPublicationAndRecovery/FS_derived_copy_transfers_to_S3_across_slices`
- 転送元が未マウントの S3 宛ての job は、予約せず再試行できるエラーで止まる: `TestStorageTransferWorkerRequiresSourceMountForS3Destination`
- S3 宛ての候補は転送元のキューに積む。FS 宛ての候補は宛先のキューのままで、原本は S3 宛てにならない: `TestStorageTransferReconcileRoutesS3CopiesToSourceQueue`
- キューの選択: `TestPhysicalQueueForRequiresQueueSiteOnlyForSiteBoundQueues`
- S3 の派生物の pending 予約を、FS だけを見る掃除が消さない: `TestStorageTransferS3PendingReservationIsNotObsolete`
- FS 1 つ + S3 の派生物で、転送の後に旧い FS のコピーを配置の削除が消す: `TestStorageTransferS3CopyLetsDeleteReconcileRetireFSCopy`。宛先の集合: `TestTransferDestinationStoreNamesIncludesS3Derived`
- 宛先キーの照合は、Head の SHA-256 があれば中身を読まず、無ければ読んで測る: `TestExistingTransferredObjectUsesHeadSHA256`
- 照合の途中で lease が引き継がれても、続きから読む: `TestStorageTransferWorkerResumesSourceVerificationAfterHandoff`
- 世代が変わった後の SHA-256 の観測を書かない: `TestStorageTransferWorkerDoesNotRecordSHA256AfterGenerationChanges`
- 送信状態の引き継ぎは、きれいなときだけ続きから: `TestTransferProgressCleanHandoffResumesSavedState`
- 送信状態を次の試行へ返すのは、きれいな引き継ぎのときだけ: `TestTransferUploadSavedStateReturnsOnlyOnCleanHandoff`
- 置き換えと行の削除で古い送信状態が掃除待ちに移る: `TestTransferUploadReplaceAbandonsPreviousState` / `TestTransferUploadRowDeletionAbandonsState`
- 掃除待ちは置き場で絞り、ID 順に返す: `TestTransferUploadAbandonedListFiltersByStoreInIDOrder`
- 送信状態の表は、置き場と状態の両方か、どちらも無いかだけを表現する: `TestTransferUploadPairCheckRejectsHalfState`
- 観測値は NULL の行で、現在の世代のものだけ書く: `TestRecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration`
- 照合の途中経過は lease の引き継ぎをまたいで残る: `TestTransferSourceVerificationSurvivesHandoffs`
- 照合の状態は送信状態と同じ行に共存しない: `TestTransferSourceVerificationExcludesUploadState`

</details>
