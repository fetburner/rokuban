> [storage.md](../storage.md) §7 の一部。索引から辿る

## カット版と CM 検出の扱い（§7 の一部）

カット版は、利用者がチャプターを確認するまで作らない。チャプターを直した後も自動では作り直さず、利用者が明示的に要求したときだけ作り直す。CM 検出を有効にした録画では、CM 検出の結果か最終失敗の記録が揃うまで、原本は消さない。

`cut: true` のプロファイルは、確認済み（`recording_chapter_ownership` の行がある）でなければ投入されない。この条件は関数 `encodable_profile` が持ち、削除側の view `until_encoded_deletable_originals` は持たない。確認前にカット版を作ると、誤検出のまま本編が削られ、原本がごみ箱を経由せずに消えて取り返せなくなる（原本は `until_encoded` で猶予なしに消える）。

確認していない録画は desired が満たされないので、`until_encoded_deletable_originals` の `unencoded_desired_profiles` の腕が原状を残す方向に自然に機能する。確認待ちの件数は `rokuban_cut_awaiting_review` で見る（失敗ではないのでアラート対象ではない）。

**チャプターを直した後、古くなったカット版は自動では作り直さない。** 作り直すと、ユーザーが確認していない区間が気づかないうちに本編から消える。API は現在のタイムラインから導出した keep 区間と凍結した区間を比べて「編集前の内容です」を出す。ユーザーが `POST /api/recordings/{id}/encoded/{profile}/reencode` を明示的に呼んだときだけ作り直す。

要求は、押した時点の量子化済み keep 区間（`requested_keep`）とともに `encoded_asset_rebuild_requests` に記録する。要求行は満たされても消さない desired で、消すのは api だけである（対象の encoded を外すとき）。満たされたかは、同じプロファイルの active な encoded の凍結区間が `requested_keep` と等しいことから導出する（view `unsatisfied_encode_rebuilds`）。比べるのは keep の値で、チャプターの版は使わない。区間名の変更は出力に入らないので、版で比べると偽の「編集前」が出る。

作り直しは押した時点の keep で描き、後のチャプター編集では取り消さない。変えたいときは押し直す。押し直すと `requested_keep` が上書きされ、古い keep のジョブは公開しない（公開 tx が要求行を読んで照合する）。押した時点の keep が利用者の確認なので、`recording_chapter_ownership` は投入の条件にしない。

通常の不足補完は、満たされていない作り直しがある profile を候補から外す。active なカット版を置き換えるのは要求に由来するジョブだけで、新しい世代のパスへ置き換える（[contract.md](contract.md) §3「カット版の置き換え」）。要求の commit 後の `encode_reconcile` はヒントにすぎず、失われても定期パスが拾う。

初回のカット版（要求に由来しないジョブ）は、エンコード中に編集されたら公開時に現在の keep と照合して公開をやめ、描き直す。照合を外すと、編集前の版が desired を満たして原本が消え、編集した内容を二度と描けなくなる。

`keepOriginal: until_encoded` の録画は、満たされていない作り直しがある間、原本を削除対象にしない。要求行はカタログに含めない。

`until_encoded_deletable_originals` は、`recording_encode_policy.cm_detect` が true の録画について、CM 検出結果か現在の世代の最終失敗も要求する。結果表の行があれば CM が 0 区間でも検出完了である。試行表の `failed` は 3 回の自動試行を終えたことを示し、`running` / `retrying` は削除を許可しない。CM 検出を無効化して再び有効にすると世代が進み、前の世代の失敗では原本を消さない。`failed` で削除を許した後に同じ局のロゴが新しく学習されて再検出が desired に戻っても、原本が既に削除されていれば再検出はできない。

この学習による再検出が有効なのは、原本が残っている場合（`keep_original=always` や削除 reconcile の前）に限る。API の再試行操作は結果と試行行を消し、active な原本があれば再び desired にする。ただし、`stage = 'adopt'` は諦めた状態を表さず、候補の採用待ちを表す。採用待ちの局があると、その局の `until_encoded` の原本は消えない。削除エンジンに容量トリガーは無く、ディスク残量の警告で気付く。
