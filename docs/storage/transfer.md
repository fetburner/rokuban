> [storage.md](../storage.md) §3 の一部。索引から辿る。

## FS 置き場間の転送

複数の FS 置き場に同じ論理資産を置くための転送契約を示す。DB の判定、帯域、公開順、削除条件を扱う。

転送 worker は、設定された FS 置き場のキューだけを購読する。宛先の root と目印を確認してから、DB に pending のコピー行を予約する。宛先がマウントされていない間は予約せず、転送試行にも数えない。

### 排他と再開

転送は転送元ごとに 1 本だけ走らせ、中断しても temp と checkpoint から再開する。

転送は転送元ごとの advisory lock を、pool から借りた接続のまま保持する。pool の外に接続を張ると、`db.max_conns` がプロセスの接続上限だという契約を破るためである。この接続の数は pool の予算に数える（[データベース](../operations/database.md)）。

転送元と宛先の rel_path lock は、置き場名と相対パスの順で取る。転送元の root の目印を照合してから読み、lock を取れない job は snooze する。

宛先と同じディレクトリに `.rokuban-transfer-{media_asset_id}-{copy_id}` を作り、checkpoint から再開する。帯域の上限は転送元の設定で適用する。論理資産の SHA-256 が既知なら、再開のとき転送元の先頭を読み直さず、temp と残りの転送元を結合して全体を検査する。

### 照合と公開

コピーの完了時に、期待するサイズと SHA-256 を照合する。論理資産の SHA-256 が未計測なら、完了時に観測値を書く。値があるときは書き換えず照合だけを行い、合わなければ完了を拒否する。

原本の既存宛先は、転送 temp と同じ inode なら前回の公開として採用する。inode が違う場合も、サイズと SHA-256 が一致するときだけ採用する。派生物のキーは、サイズと SHA-256 が一致すれば再開に使う。転送が新規作成したキーは所有記録を付け、同じ pending のコピーの再試行だけが再利用する。所有記録のない既存のキーも内容が一致すれば採用できるが、commit が拒否されたときに削除しない。

派生物の論理資産の SHA-256 が既知なら、転送元の読み取りを帯域制限の下で scratch に複製せず `Store.Put` へ流し、`Put` が宣言サイズと SHA-256 を照合する。合わなければ何も公開されず、転送元の不一致として扱う。SHA-256 が未計測のときだけ scratch に複製してハッシュを取る。

内容が異なる既存のキーは、動画も encoded の字幕も、所有記録の有無にかかわらず衝突として扱う。転送は失敗として記録し、そのキーは消さない。既知の SHA-256 が合わないときは、転送元を帯域制限の下で読み直し、原本の宛先 temp の破損と転送元の破損を区別する。異なる動画を rename や link で上書きしない。

temp の fsync と Close、正規名への上書きしない link、親ディレクトリの fsync を行う。その後に DB transaction で宛先のコピーを active にする。DB commit が公開点である。

commit が拒否されたとき、原本ではこの転送の temp と同じ inode の宛先を消し、派生物では所有記録のあるキーだけを消す。派生物の字幕 sidecar は本体より先に消し、sidecar の削除に失敗したら本体と pending 行を残して次回に再試行する。拒否は世代や規則の変化、または記録済みの SHA-256 との不一致で起きる。派生物は Put 成功後に所有記録を commit するため、その間にプロセスが落ちたキーは未記録のまま残り、回収では他の書き手のキーと区別できないので消さない。

採用する宛先が、この転送の temp と同じ inode なら、採用と同時に temp と checkpoint を片付ける。inode が違う宛先は他の書き手が置いたものなので、temp に触れない。同じ pending 行を別の worker が先に active にしたときは、そのコピーを完了済みとして扱い、公開済みの宛先と sidecar を消さない。

encoded の隣接 `.vtt` は、本体のキーを作った後、コピーを active にする前に、宛先のキーへ作る。転送元に sidecar が無いときは、この転送が作った（所有記録のある）宛先の sidecar だけを消し、所有記録のない sidecar は残す。字幕は独立した `media_assets` 行ではないので、動画のコピー状態を commit する前に sidecar を揃える。

### 失敗と壊れた転送元

転送の失敗は宛先ごとに記録して再投入を抑え、壊れた派生コピーは作り直せるときだけ消す。

失敗の理由は宛先のコピーごとの試行表に記録し、上限に達すると再投入を止める。止まっている件数はゲージ `rokuban_storage_transfer_exhausted_assets` で見る。宛先の置き場や世代が変わると、古い試行記録を消す。記録済みの SHA-256 と合わない原本は報告だけで、転送元を残す。

派生コピーが壊れていたときは、同じ録画に active・実体ありの原本コピーが残る場合だけ、壊れた転送元のコピーを退役させる。転送 worker は録画の行、派生の論理行の順にロックした 1 つの transaction で、転送元のコピーを deleting にする。この順序は until_encoded の原本削除と同じで、両者を直列化する。直列化しないと、原本の削除と派生コピーの退役が同時に通り、どちらの実体も残らない。

退役の続きは、転送元の rel_path lock の下で行う。目印を照合してからファイル（encoded なら字幕の sidecar も）を消し、親ディレクトリを fsync して deleted にする。

deleted への確定は、コピー自身の置き場の行をロックして行うので、転送 worker が束縛された置き場と違う置き場の転送元でも確定できる。同じ transaction で、実体無しの記録と転送の試行行も消す。退役させたら `rokuban_storage_transfer_corrupt_sources_retired_total` を増やし、Error ログを出す。deleting にして unlink の前に落ちた場合は、削除 reconcile がどの腕にも該当しない deleting 行として active に戻し、転送の再試行が不一致を再検出して消す。原本が無ければコピーを残して報告し、試行回数を記録する。

退役した encoded のコピーは、encode の補完の入力である `unencoded_desired_profiles` に行として現れる。この行から encode が再生成するところまでは確かめていない。thumbnail とシークタイルが再生成されるかも未検証である。

### 不要になったコピーの回収

不要になった pending のコピーは、転送と同じ順序で lock を取り、削除 reconcile が回収する。

ごみ箱・世代・desired の置き場の変更、または until_encoded の保持条件の成立で不要になった pending のコピーは、削除 reconcile が回収する。候補を列挙した後に rel_path lock を取り、録画・論理資産・コピーの行をロックした transaction で、不要になった条件を再評価する。

原本の回収では、正規名の宛先を転送 temp と inode が一致するときだけ消す。派生物の回収では、所有記録のあるキーだけを消す。所有記録のない宛先は残す。いずれも字幕 sidecar を本体より先に消し、sidecar を消せなかったときは本体と pending 行を残して次回に再試行する。

回収が終わった pending 行は、転送 temp と checkpoint も含めて退役させる。今も desired の pending のコピーは、失敗回数と checkpoint からの再開を保つために残す。最後の pending のコピーが退役すると、録画の完全削除（purge）が完了する。

desired の置き場から外れた配置は、削除 reconcile の腕の 1 つとして消す。腕は名前付き述語 `media_asset_copy_prunable` で、desired の置き場に active・実体ありのコピーがあり、overlap を過ぎていることだけを条件にする。desired のコピーは別の置き場の実体あるコピーなので、最後のコピーを消さないことをこの条件が守る。

腕を参照するのは、新規候補（active → deleting）の入口、deleting のまま止まった行の拾い直し、拾い直しの否定形（active に戻す判定）である。入口は `internal/db/queries/storage_transfer.sql`、残りは `internal/db/queries/delete_reconcile.sql` にある。入口は `media_assets` 行を lock してから deleting にするので、規則の切り替えが並走しても両方のコピーは消えない。deleting のまま落ちた行は、拾い直しが unlink を終える。その間に desired のコピーが実体無しと記録されたときは、否定形が active に戻す。

transfer temp と checkpoint は、孤児監査で予約ファイルとして分類し、同じ temp の flock を取ってから回収する。

<details><summary>根拠</summary>

- 再開、hash、上書きしない公開、復旧後の採用: `TestStorageTransferWorkerCopiesAndAdopts`
- 自分の temp と checkpoint の片付け、他の書き手の宛先を採用するときの temp の保持: `TestStorageTransferWorkerCopiesAndAdopts`
- 記録済みの SHA-256 を上書きしない: `TestCompleteMediaAssetTransferRejectsChangedSHA`
- 内容の違う既存の sidecar の衝突、source に sidecar が無いときは所有記録のある sidecar だけを消す: `TestStorageTransferWorkerEncodedSubtitleKeys`
- SHA-256 が既知の派生物を scratch に複製せず Put する: `TestStorageTransferWorkerStreamsKnownSHADerivedWithoutScratch`
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

</details>
