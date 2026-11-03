> [runbook.md](../runbook.md) の一部。索引から辿る。

# S3 の bucket を置き場名のまま移す

派生物の S3 bucket を別の bucket へ移す。アプリ上の置き場名を維持し、既存の DB コピー行をそのまま有効にする。

## 移行前に Rokuban を止める

Rokuban を正常停止し、pending コピーと multipart の途中状態が残っていないことを確認する。

次の SQL で対象の置き場に pending コピーがないことを確かめる。結果が 1 行でもあれば、元の接続先で Rokuban を再起動して転送を終え、もう一度停止して確認する。`upload_state` は旧 bucket の UploadId を含むため、新 bucket では続きから送れない。

```sql
SELECT c.id, c.state, p.upload_store, p.upload_state IS NOT NULL AS has_upload_state
FROM media_asset_copies c
LEFT JOIN media_asset_copy_transfer_progress p ON p.copy_id = c.id
WHERE c.store = '<store>'
  AND c.state = 'pending'
ORDER BY c.id;
```

## オブジェクトをコピーする

新しい bucket にキーと user metadata をコピーし、内容と checksum を照合する。

旧・新 bucket の両方にアクセスできる `rclone` などを使う。キーを変えずにコピーし、S3 user metadata を保つ。Rokuban は `rokuban-sha256` metadata を全体の SHA-256 として読む。

```sh
rclone copy --metadata --ignore-times --progress s3-old:<old-bucket> s3-new:<new-bucket>
rclone check --download s3-old:<old-bucket> s3-new:<new-bucket>
```

`rclone check --download` で内容を照合する。さらに両 bucket のオブジェクト一覧を metadata 付きで出し、各キーと `rokuban-sha256` の値が一致することを確かめる。

```sh
rclone lsjson --recursive --metadata s3-old:<old-bucket>
rclone lsjson --recursive --metadata s3-new:<new-bucket>
```

移行対象に派生物以外のキーがある場合も、キーを維持してコピーする。検証が終わるまで旧 bucket は削除しない。

## 接続先を切り替える

コピー検証後に接続先を更新し、Rokuban を再起動する。

既存の置き場名の `bucket` と必要なら `endpoint` を新しい値に変える。置き場名と `storage.derived` は変えない。その後 Rokuban を再起動する。

bucket または endpoint を変えると `PolicyFingerprint` が変わる。worker は旧 fingerprint の転送ジョブを破棄し、次の transfer reconcile が現在の規則で積み直すため、キューの確認は要らない。切り替え後に転送待ちが残る場合は `rokuban_storage_transfer_waiting_assets` を見て、reconcile が進むことを確かめる。

## 置き場名を変えてしまった場合

派生物の置き場を別の S3 名に変える操作は、この bucket 移行とは異なる。

旧い S3 にだけ encoded があると、`unencoded_desired_profiles` は再エンコード候補を作らない。S3 は転送元にならないため、encoded は新しい派生物置き場に届かない。`until_encoded` の原本は削除対象に出ないまま残る。確認テストは [ストレージ転送](../storage/transfer.md) に記載している。

置き場名を変えた後も旧い置き場を `storage.stores` に残すと、配信には使える。active な派生コピーがあれば、その置き場名が reconcile の Warn に出る。`rokuban_storage_derived_copies_outside_derived_s3` は該当コピーの合計件数を示す。reconcile は旧い置き場のコピーを自動削除しない。

<details><summary>根拠</summary>

- [`rclone copy`](https://rclone.org/commands/rclone_copy/) の `--metadata` はコピー時に metadata を保つ
- [rclone S3 backend](https://rclone.org/s3/) は S3 object metadata を扱う
- Rokuban が読む checksum metadata の名前: `rokuban-sha256`

</details>
