> [operations.md](../operations.md) §4「ストレージ運用」の一部。索引から辿る。

## 4. ストレージ運用

ストレージ運用の判断を、原本の FS 契約・録画バッファ・アーカイブ・災害復旧の順に置く。

### 原本 ingest 先の FS 契約

原本の ingest 先は、ファイルの同期、クローズ時のエラー報告、同一 FS 内の atomic rename と hard link、公開後の親ディレクトリの同期を信頼できる FS に限る。ingest を引く worker は起動時に、実際の操作列が一度成功することを probe で確かめる。

この契約が対象にするのは `storage.media_dir` である。ファイル `fsync`、`Close` のエラー報告、同一 FS 内の atomic rename と hard link、公開後の親ディレクトリ `fsync` を信頼できる root に限定する。

#### 対象内と対象外

ローカル FS・JuiceFS・NFS は対象内で、FUSE S3 は対象外である。

ローカル FS / JuiceFS / NFS は対象内である。ただし NFS export は `sync`、client mount は `hard` を推奨する。NFS の open 中 unlink による `.nfsXXXX`（silly rename）は、canonical orphan として報告されることがある。自動削除はせず、ファイルを閉じた後に状態を確認する。

geesefs / s3fs / AWS Mountpoint など FUSE S3 は、原本にも派生物にも使わない。atomic rename、file / parent `fsync`、`Close` のエラー、ロックの意味論を、ファイルの公開根拠として信頼できないためである。派生物を S3 に置く場合は API を使う。

#### 起動時の probe

ingest を引く worker は起動時に、実際の操作列が一度成功することを probe で確かめる。

worker は ingest キューを購読する起動時に、temp 作成、file `fsync`、`Close`、同一ディレクトリ内の hard link、親ディレクトリ `fsync` の順で probe を行う。この probe は、実際の操作列が一度成功することだけを確認する。パス文字列から FS の種類や意味論を推測する検査ではない。

worker は起動時と、ファイルシステム操作や DB への記録の前に、`.rokuban-store` を `storage.stores` の FS 置き場の名前と照合する（FS 置き場は今 1 つだけ。[置き場の規則](../configuration/keys.md#置き場の規則)）。照合する場面は [missing-assets.md](../storage/missing-assets.md) の「検出の安全弁」にある。目印の内容が設定と一致しないか、目印を読み取れないときは、その操作を見送る。見送るたびに `rokuban_store_marker_mismatch_total{store}` が増える。worker ロールと streamer ロールは、`storage.stores` に FS 置き場がちょうど 1 つ無いと起動しない。

見送る操作は、ingest・encode・サムネイル・シークタイル・catalog の書き込みと、ファイルの削除・欠損の記録・孤児の回収である。

目印を確認できないまま起動した worker は、ingest probe を保留する。probe は `media_dir/sites` を作る書き込みなので、最初の ingest ジョブが目印の照合を通った後に 1 回だけ走る。失敗した probe は固定せず、次の ingest ジョブが再試行する。

目印は `rokuban storage adopt <store-name>` で作る。このコマンドは、互換列がある間に未割り当ての論理資産へコピーを作り、`store` が空の観測行も割り当てる。互換列を削除した後は、コピーの無い論理資産があればエラーにする。同じ名前の目印への再実行は成功し、異なる名前の目印は上書きしない。目印が無い root に目印を新しく作る前に、DB にその置き場のコピー行が 1 件でもあるかを確かめる。あれば、マウント漏れか別のボリュームの可能性があるので、エラーにして目印を書かない。置き場の名前変更は未対応である。

導入と、`storage.media_store_name` を設定済みの構成の移行手順は [runbook/setup.md](../runbook/setup.md) の「置き場の目印」にある。

<details><summary>根拠</summary>

- ingest 抑止: `TestIngestWorkerWithholdsWhenStoreMarkerDoesNotMatch`
- probe の遅延と再試行: `TestIngestWorkerProbesStorageAfterMarkerCheckAndRetriesFailure`
- 起動時に済んだ probe を繰り返さない: `TestIngestWorkerSkipsStorageProbeAlreadyDoneAtStartup`
- 生成物の公開抑止: `TestGeneratedMediaPublishDoesNotStageWhenStoreMarkerIsMissing`
- encode の字幕 stage と lock の抑止: `TestPublishEncodedCreatesNoSubtitleTempOrLockWhenMarkerDisappearsAfterBodyStage`
- unlink 抑止: `TestDeleteMediaAssetWithholdsUnlinkWhenMarkerDisappears`
- 削除済みの確定抑止（unlink 成功）: `TestDeleteMediaAssetWithholdsMarkDeletedWhenMarkerDisappears`
- 削除済みの確定抑止（`ErrNotExist`）: `TestDeleteMediaAssetWithholdsMarkDeletedAfterErrNotExistWhenMarkerDisappears`
- deleting 行の決着抑止: `TestResolveDeletingAssetWithholdsDecisionWhenMarkerDisappearsAfterStat`
- 孤児の unlink 抑止: `TestOrphanUnlinkRechecksStoreMarkerImmediatelyBeforeRemoval`
- 孤児の「無い」の記録抑止: `TestOrphanAbsentAfterUnlinkWithholdsRecordWhenMarkerDisappears`
- 欠損・孤児操作の抑止: `TestStoreMarkerMismatchWithholdsMissingAssetAndOrphanActions`
- 実体無しの記録の再照合: `TestMissingAssetRecordRechecksStoreMarkerImmediatelyBeforeWrite`
- 孤児候補の記録の再照合: `TestOrphanCandidateRecordRechecksStoreMarkerAfterWalk`
- replay checkpoint 抑止: `TestInterruptedIngestReplayWithholdsCheckpointWhenMarkerDisappears`
- catalog の途中中断: `TestWriteWithholdsManifestAndPruneWhenMarkerDisappearsMidway`
- catalog の開始時の照合: `TestWriteCreatesNothingWhenMarkerIsAlreadyBroken`
- catalog の prune 抑止: `TestPruneStopsWhenMarkerDisappearsBeforeDestructiveAction`
- root の目印を `rel_path` にさせない: `TestResolve_ReservesRootStoreMarkerOnly`
- adopt: `TestStorageAdoptWritesStoreMarker`
- 異なる名前への adopt 拒否: `TestStorageAdoptDoesNotRenameExistingStoreMarker`

</details>

#### ingest の一時ファイルと orphan

ingest の一時ファイルは、canonical path と同じディレクトリに作られる。プロセス死で残った ingest temp は、mtime 猶予と aging（既定 7 日 + 14 日）の後に回収する。

link 後の DB 失敗は temp を残し、再試行が commit をやり直す。再試行が成功せず行の無いまま残った canonical orphan は、aging 後に Warn と `rokuban_orphan_files_unresolved` で報告し、手動調査まで保持する。rescue の catalog 無し走査は、temp を original として復元しない。

### 録画バッファのサイジング

録画バッファ（mirakc の録画先、エッジのローカルディスク）は、ingest が詰まったときの滞留分で容量を決め、速度はレイテンシを優先して見る。容量アラートとセットで運用する。

対象は mirakc の `recording.basedir` である。録画バッファの容量アラートは、[監視メトリクス](monitoring.md) の「未 ingest record 総量」と [アラート設計](alerts.md) のエッジディスク残量アラートである。

#### 容量

容量の支配項は、ingest が詰まったときの滞留分である。同時録画数は支配項にならない。回線断・クラウド側障害時は、未 ingest の record が溜まり続ける。

推奨値は「**N 日分の全録画を保持できる容量**」である（地デジ約 7 GB/時で見積り）。

#### 速度

速度要件はレイテンシである。書き込みは 1 録画あたり約 2 MB/s（地デジ 17 Mbps）で、同時 8 本でも 16 MB/s に過ぎないので、絶対帯域は問題にならない。

怖いのは他 I/O との競合によるレイテンシスパイクである。ingest pull のサイト単位 1〜2 本キャップは、このための決定でもある。

#### N 日は容量だけでは決まらない: `epg.retention_grace` との交点

滞留を N 日まで許すなら、ディスクだけでなく、EPG の保持猶予も N 日以上にする。encode 意図が生き残る滞留の上限は、猶予で決まるためである。

> **滞留を N 日まで許すつもりでリングバッファをサイジングするなら、`epg.retention_grace >= N` にする。**

録画の encode policy は、ingest が原本をコミットする瞬間に、`program_snapshots` 経由で予約から解決して凍結される（[ストレージ](../storage.md) §6）。その `program_snapshots` は、放送終了 + `epg.retention_grace`（既定 24h）で GC される。GC は DB の時計だけで動き、エッジに到達できるかどうかを見ない。

<details><summary>根拠</summary>

- encode policy の項目: `keepOriginal` / `encodeProfiles`
- 凍結時に解決元となる表: `program_snapshots`

</details>

##### 猶予を跨いだ ingest の帰結

GC がその番組のスナップショットを刈った後に ingest が走ると、その録画は既定ポリシーで凍結される。原本は常に残すが、エンコードは投入されない。データは失われない。

作成時点で予約も意図も無かった録画は、出自が帰属なしになり、ルールへの紐付けも外れる。復帰は障害復旧時に一括で起きるので、まとまった件数が同時にこうなりうる。

事後回復の手段は `POST /api/recordings/{id}/encode-profiles`（追加のみ）**だけ**である。エンコード reconcile の定期パスは拾わない。desired が空なので候補にならないためである（[ストレージ](../storage.md) §6）。

```mermaid
flowchart TD
  A["録画の ingest が滞留する"] --> B{"GC がスナップショットを刈る前に ingest が走るか"}
  B -->|はい| C["予約から encode policy を解決して凍結する"]
  B -->|いいえ: GC が先に刈る| D["既定ポリシーで凍結する"]
  D --> E["原本は残るがエンコードは投入されない"]
  D --> F["予約も意図も無ければ出自が帰属なしになる"]
```

<details><summary>根拠</summary>

- 既定ポリシー: `keep_original='always'` / `encode_profiles=[]`
- 帰属なしになる出自: `recordings.source = 'unattributed'`
- 紐付けが外れる列: `rule_id` が NULL になる
- 拾わない定期パス: `encode_reconcile`

</details>

##### GC 自身が止まる障害

GC 自身が止まる障害では、結果が変わる。worker 全停止や DB 到達不能のように、ルール評価のパスごと止まる障害では、断のあいだ GC も進まない。

復帰時は、sweep + ingest とルール評価のパスの競争になる。ingest が先に走れば、意図は守られる。どちらが先かはジョブの実行順に依存する。未検証である。

決定論的に上の結果になるのは、GC は動き続けたが、その録画の ingest だけが猶予を跨いで遅れた場合である。

<details><summary>根拠</summary>

- GC を含むルール評価のジョブ: `ruler_pass`

</details>

##### 猶予を上げる代償

猶予を上げるのは無料ではない。同じキーが EPG 射影のローリングウィンドウも駆動するので、番組の保持と欠測行を作る窓が広がる。

N 日にすると、`epg_programs` に全サービスの放送済み番組が N 日ぶん残る。

終了済み番組に対して欠測行が作られる窓も、同じだけ広がる（増加量は未検証）。欠測行の GC 閾値は `retention_grace + 30日` なので、行の寿命も N + 30 日に追従して伸びる。表の件数には上限があるままである。

詳細と、GC 側を滞留と連動させない理由、確認しているテストは、[ストレージ](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」にある。

<details><summary>根拠</summary>

- 同じ猶予で駆動される射影の GC: `PruneEpgPrograms`
- 欠測行の表: `never_scheduled_events`

</details>

##### 滞留の型と見るメトリクス

見張るメトリクスは、その record が終了済みとしてクラウドに観測されていたかで分かれる。リンクが生きているかでは分かれない。どちらも閾値は、悪い状態が保持猶予を超えて続いたかである。

終了済みは `finished` の状態を指し、保持猶予は `epg.retention_grace` である（[監視メトリクス](monitoring.md) の一覧）。

| 滞留の型 | 見るメトリクス |
|---|---|
| 観測済みの ingest 詰まり | `rokuban_uningested_records{site}` / `rokuban_uningested_record_bytes{site}` の増加 |
| 回線断中の録画 | `rokuban_sweep_last_success_timestamp_seconds{site}` / `rokuban_epg_sync_last_success_timestamp_seconds` の停滞 |

観測済みの ingest 詰まりは、worker 停止・ストレージ障害・キュー詰まりで起きる。断の直前に `finished` として観測済みの record もこちらである。回線断の型は、断の最中に始まった録画と、断が始まった時点でまだ録画中だった録画である。

後者では、未 ingest メトリクスは増えない。バックログの述語は `record_sync.status = 'finished'` で、その行は watcher が mirakc を観測して初めて作られ、更新される。断の最中に始まった録画は行そのものが無く、断が始まった時点で `status='recording'` だった録画も gauge に現れない。`finished` への更新には復帰後の再観測が要るので、断のあいだ gauge は平らなままである。**滞留量のアラートだけでは回線断を検知できない。**

> 「アンカーがある（案 1 で留め置ける）」と「gauge に出る」は別の条件である。案 1 は述語を自分で選べるので `status='recording'` の観測でも足りる（[ストレージ](../storage.md) §6）。gauge の述語は `finished` 固定で選べない。

<details><summary>根拠</summary>

- バックログを数えるクエリ: `GetUningestedRecordBacklog`（`internal/db/queries/metrics.sql`）

</details>

### アーカイブの速度要件

アーカイブ（Rokuban のメディアストレージ。ローカル FS / 条件付き NFS / JuiceFS）は低速で良い。ただし原本の ingest 先として、上の強い FS 契約を満たす必要がある。

#### スループット

**平均スループットは、1 日の録画総量 / 24 時間以上あればよい。** 瞬間的な変動は録画バッファが吸収するので、リアルタイム性は一切要求されない。エンコードの読み出しもバッチなので遅くて良い。

#### 再生時のシーク

唯一レイテンシが人間に見えるのは、再生時のシークである。原本削除ポリシーと組み合わせた「視聴は H.265 派生物、原本は消すか別の低頻度領域」という運用を採る。

### S3 の置き場の運用要件

S3 の置き場は、AWS S3 と S3 互換サーバー（MinIO、Cloudflare R2 など）のどちらでも使う。サーバーごとに対応の違う機能があるので、使う前に適合テストで測る。

適合テストが測るのは 4 つで、条件付き書き込み（`If-None-Match: *`）、multipart の完了と abort、HEAD での SHA-256 の取得、署名付き GET の Range である。結果は機能ごとに「対応 / 非対応 / 一部対応」で記録するだけで、非対応や一部対応を理由に失敗にしない。条件付き書き込みは、対応していなくても、書き込み先のキーが一意なので使える。

置き場の bucket には、`AbortIncompleteMultipartUpload` のライフサイクル規則を必ず設定する。途中で終わった multipart のアップロードはオブジェクトとして見えず、孤児回収では拾えない。abort への対応の有無にかかわらず、worker が途中で落ちれば部品が残るためである。`DaysAfterInitiation` は、正常な転送にかかる最長時間より後にする。

multipart の SHA-256 は、AWS では常に部品から作る composite checksum である。オブジェクト全体の SHA-256 にはならないので、HEAD で得た値を全体の SHA-256 と直接比較してはならない。アプリは、データを流しながら全体の SHA-256 を自分で計算し、照合にはその値を使う。

#### 適合テストの回し方

テスト用の bucket を 1 つ用意して、リポジトリのルートでテストを実行する。

接続先は環境変数で渡す。`ROKUBAN_S3_CONFORMANCE_ENDPOINT`、`_REGION`、`_BUCKET`、`_ACCESS_KEY_ID`、`_SECRET_ACCESS_KEY` である。bucket をテストに作らせるときは `ROKUBAN_S3_CONFORMANCE_CREATE_BUCKET=true` を足す。

```bash
go test -tags s3conformance ./internal/objstore/conformance -count=1 -v
```

テストが呼ぶ操作は次のとおりで、これが必要な権限になる。

- bucket 内のオブジェクトへの `PutObject`、`GetObject`、`HeadObject`、`DeleteObject`
- multipart の `CreateMultipartUpload`、`UploadPart`、`CompleteMultipartUpload`、`AbortMultipartUpload`、`ListParts`
- `CREATE_BUCKET=true` のときだけ `CreateBucket`

テストが作るオブジェクトは `rokuban-conformance/{UUID}/` の下に置き、終わると削除する。

結果は `-v` の出力の `S3_CONFORMANCE` で始まる行に、機能ごとの JSON で出る。`status` が `supported`、`unsupported`、`partial` のいずれかで、`checks` に内訳が入る。想定外のエラーで判定できなかった機能は `unknown` で、`note` に原因が入る。

`ROKUBAN_S3_CONFORMANCE_EXPECT` に `機能名=status` をカンマで並べると、記録がその期待と違う機能のテストだけが失敗する。機能名は `S3_CONFORMANCE` 行の `feature` で引く。未設定なら照合せず、運用者が任意のサーバーを測る用途になる。CI は固定した MinIO で 4 機能すべてに `supported` を期待し、退行を検知する。

<details><summary>根拠</summary>

- 適合テスト: `internal/objstore/conformance/conformance_test.go`
- CI の MinIO: `.github/workflows/ci.yml` `s3-conformance`
- multipart の上限（部品の大きさと個数）: [S3 multipart upload limits](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html)
- checksum の種類（SHA-256 は multipart で composite だけ）: [S3 object integrity](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html)
- abort の確認方法（`ListParts` の parts が空）: [AbortMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html)
- 不完全な upload のライフサイクル規則: [Abort multipart upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/abort-mpu.html)

</details>

### disaster recovery（catalog + rescue）

rescue は、DB を失った後にだけ使う。live DB を catalog の内容で上書きするので、健全性の確認には使わない。

確認は `rokuban catalog verify` で行う（[DB 運用](database.md) のバックアップ節）。`rokuban rescue` はストレージを走査して復元する。

#### rescue が復元するもの

完成世代が catalog にあれば照合してメタデータごと復元する。catalog に無いファイルは、サイトの前置が付いたものに限り、推定できる範囲で素の asset として登録する。

`catalog/` に完成世代（manifest まで書き終えた世代ディレクトリ。[storage.md](../storage.md) §8）があれば、照合してフルメタデータ（番組情報・ドロップ統計・保持ポリシー）ごと復元する。最新世代が不完全なら 1 世代前へ落ち、飛ばした世代を理由付きで出力する。

catalog に無い `sites/{site}/` 前置付きファイルは、ディレクトリ規約・ファイル名から推定できる範囲で「素の asset」として登録する。UI から見えて再生できる状態に戻す。前置の無い動画ファイルは Warn を出して登録しない。

#### 復旧中の運用

復旧中はメディアを変更する worker を止め、終了後に再開する。ファイルと asset row は共通 lock で守るが、catalog は録画・保持ポリシーも上書きするためである。欠損ファイルの扱いと共有ストレージの lock 要件は [rescue の契約](../storage/rescue.md#rescue-の世代選択とフォールバック) を参照する。

実装は `rokuban import epgstation` の in-place 登録機構と同型なので共有する。

#### DB 喪失時も維持される不変条件

**「放送データのコピーが常に 1 つ以上」は DB 喪失時も維持される。** エッジ record の削除は ingest の DB コミット後に起きる。コミット直後に DB を失ってもファイルはアーカイブに残る。canonical orphan は報告だけなので、rescue が再登録できる。

cleanup は mirakc の basedir に絶対に触らない（エッジ側削除は ingest の検証済み削除のみ）。
