> [storage.md](../storage.md) §3 の一部。索引から辿る。

## FS 置き場間の転送

複数の FS 置き場に同じ論理資産を置くための転送契約を示す。DB の判定、帯域、公開順、削除条件を扱う。

置き場の間でファイルを複製し、DB の判定と公開順を守って物理コピーを追加する。現行の worker は設定された FS 置き場だけを購読し、宛先の root と目印を確認してから DB に pending copy を予約する。宛先が mount されていない間は予約せず、転送試行にも数えない。

転送は source store ごとの advisory lock を専用の pool 接続で保持し、source と destination の rel_path lock も置き場名と相対パスの順で取る。source root の目印を照合してから読み、lock を取得できないジョブは snooze する。destination の同じディレクトリに `.rokuban-transfer-{media_asset_id}-{copy_id}` を作り、checkpoint から再開する。帯域上限は source store の設定で適用する。source SHA-256 が既知なら、resume 時に source prefix を読み直さず、temp と残りの source を結合して全体を検査する。

コピーの完了時に期待サイズと SHA-256 を照合する。SHA-256 が未計測なら、完了時に `media_assets.sha256` へ一度だけ書く。既存 destination は size と SHA-256 が一致するときだけ採用する。既知 SHA が不一致のときは source を帯域制限の下で読み直し、destination temp の破損と source の破損を区別する。異なるファイルを rename や link で上書きしない。

temp の fsync と Close、canonical への no-overwrite link、親 directory の fsync を行う。その後に DB transaction で destination copy を active にする。DB commit が公開点である。commit 前に失敗した canonical は自動削除しない。再試行時は canonical を再照合して採用を試みる。同じ pending row の別 worker が先に active にした場合は、その copy を完了済みとして扱い、公開済み canonical や sidecar を失敗時の cleanup で消さない。

encoded の隣接 `.vtt` は media asset と同じ rel_path lock の下で destination に同期してから copy を active にする。source に sidecar が無ければ destination に残った古い sidecar を消す。旧 placement の削除も encoded 本体と sidecar を同じパスロックの下で消す。字幕は独立した `media_assets` 行ではないため、動画のコピー状態を commit する前に sidecar を揃える。

失敗理由は destination copy ごとの試行表に記録し、上限 10 回で再投入を止める。desired store または generation が変わると古い試行記録を消す。元の SHA と合わない原本は report-only で、source を残す。派生コピーが破損した場合は、実体のある原本コピーが同じ録画に残るときだけ破損コピーを退役させる。encode / thumbnail reconcile が再生成する。原本が無ければコピーを残して報告し、再試行回数を記録する。

trash・generation・desired placement の変更、または `until_encoded` の保持条件成立で不要になった pending copy は delete reconcile が回収する。

candidate の列挙後に rel_path lock を取る。recording・asset・copy を lock した transaction で obsolete 条件を再評価する。

条件に当たる copy は canonical、sidecar、transfer temp と checkpoint を消して `deleted` にする。今も desired な pending copy は残し、失敗回数と checkpoint からの再開を保つ。

最後の obsolete copy が退役すると recording purge が完了する。

旧コピーは delete reconcile が削除する。候補抽出と `deleting` 遷移の両方で、名前付き述語 `media_asset_copy_prunable` を使う。desired copy が active かつ実体ありで、overlap 経過済みになり、別の active copy も実体ありの場合だけ対象にする。`media_assets` 行を lock してから条件を再評価する。policy switch が最後の有効コピーを消すのを防ぐ。transfer temp と checkpoint は孤児監査で予約ファイルとして分類し、同じ temp の flock を取ってから回収する。

<details><summary>根拠</summary>

- 再開、hash、no-overwrite 公開、復旧後の採用: `TestStorageTransferWorkerCopiesAndAdopts`
- 転送時の sidecar 同期: `TestStorageTransferWorkerKeepsEncodedSubtitleSidecarInSync`
- 先行 worker が active にした copy の sidecar 保持: `TestStorageTransferWorkerPreservesSubtitleAfterConcurrentActivation`
- placement 削除時の sidecar 削除: `TestDeleteReconcilePlacementDeletionRemovesEncodedSubtitleSidecar`
- pending copy 退役後の recording purge: `TestDeleteReconcileReclaimsObsoletePendingFSCopyAndAllowsPurge`
- `until_encoded` で不要になった pending 原本コピーの退役: `TestDeleteReconcileReclaimsPendingOriginalAfterUntilEncoded`
- `until_encoded` 条件のロック後再判定: `TestStorageTransferPendingReclaimRechecksUntilEncodedEligibility`
- 削除時の logical row lock と policy 再判定: `TestStorageTransferDeletionSerializesPolicySwitches`

</details>
