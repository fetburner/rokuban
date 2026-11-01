> [storage.md](../storage.md) §1〜5 の一部。索引から辿る

## 1. 原本と派生物の置き場

原本は、強い POSIX のファイルシステムに置く。派生物は置き場のインターフェースを通して読み書きする。その実装は、強い POSIX のファイルシステムか S3 API のどちらかである。S3 SDK を import するのは S3 API 実装だけにする。

## 2. FUSE 越し S3 の制約

FUSE 越しの S3 は、原本にも派生物にも使わない。atomic rename、ランダムライト、fsync、ロックの意味論を信頼できず、ファイルを公開する根拠にできないためである。S3 は API で使えば、直接配信もできる。

S3 マウント（k8s-csi-s3 の geesefs/s3fs、AWS Mountpoint 等）では、次の 3 点は、原本 root（`storage.media_dir`）でも派生物の置き場でも、契約として信頼できない。

- 同一 FS 内の atomic rename がない。コピー + 削除になり、数十 GB の録画で実コピーが走る。Mountpoint は rename 自体に非対応である
- ランダムライトができないか遅い。特に ffmpeg は MP4 出力時にヘッダ（moov atom）を書き戻すためシークするので、S3 マウント上への直接エンコード出力は正しく書けないか激遅になる
- ファイル `fsync`、`Close` のエラー報告、親ディレクトリ `fsync`、ファイルロックの意味論が、ファイルを公開する根拠として信頼できない

したがって FUSE S3 は置き場として使わない。派生物を S3 に置くときは S3 API 実装を使う。

## 3. ストレージ契約

この節は、原本と派生物の保存・公開の手順と、派生物の置き場の約束を定める。

原本と派生物では公開方法が異なる。原本は強い POSIX FS 上のファイルとして保存し、派生物は `internal/objstore` の `Store` 契約を通して読み書きする。どちらも DB には置き場内の相対キーだけを保存する。

原本の `storage.media_dir` とローカル `scratch_dir` には、強い POSIX FS を使う。ingest はファイル `fsync`、`Close` のエラー報告、hard link、親ディレクトリ `fsync` に依存する。ローカル FS、JuiceFS、条件を満たす NFS は対象内で、FUSE S3 は対象外とする。派生物は次の共通契約で扱い、呼び出し側は POSIX rename や lock を前提にしない。

1. ffmpeg などの生成処理はワーカーのローカル scratch で行い、完成した出力だけを `objstore.Store.Put` に渡す。書き込みはシーケンシャルにし、Put には正確なサイズと SHA-256 を渡す
2. `Store.Put` は全バイトを検証し、同じキーを上書きせず、完全なオブジェクトが durable になった後にだけ成功を返す。キーが無いことは `Head`、`Get`、`Open` が `ErrNotFound` で返し、一時的なエラーと区別する。`Delete` は冪等で、無いキーの削除も成功にする。`List` は完成したキーだけを返し、載るのが遅れてもよい。`Get`・`Open`・署名付き URL は、実装が持つ能力に応じて `objstore.Reader`・`Local`・`SignedURL` の別のインターフェースとして提供する
3. **派生物の公開点は DB commit** とする。キーへの Put が成功した後、同じ transaction でコピーを `active` にして論理メタデータを記録する。DB commit 前のキーは配信対象にせず、失敗した `pending` のコピーと孤立したキーは reconcile が回収する
4. 派生物のキーは世代ごとに一度だけ使う。差し替えは新世代のキーを作り旧世代を保持する。置き場間転送のコピーにも INSERT 時に新しい `rel_path` を割り当て、pending の再試行ではその値を保つ。DB の現行 active 行だけが配信を許し、旧世代は `storage.transfer.overlap` 後に削除する

`Put` は 1 回の呼び出しで送り切る契約である。1 回の Work では送り切れない転送は、再開できる送信を使う。再開できる送信は `objstore.Resumable` の `StartUpload` / `ContinueUpload` / `CompleteUpload` / `AbortUpload` で行う。送信状態は置き場の実装が marshal した不透明なバイト列である。呼び出し側は進捗の行に保存してそのまま返す。

`ContinueUpload` が返す offset は置き場に残ったバイトだけを表す。`CompleteUpload` は送りながら計算した SHA-256 が宣言と一致したときだけ公開する。ルール 2 の「全バイトを検証し、完全なオブジェクトだけを成功とする」は、再開できる送信でも同じである。送信状態に何を入れるかは実装が決める（S3 は UploadId とパートの一覧、FS は staging の相対パス）。手順は [置き場間の転送](transfer.md) に従う。

配信側も置き場の能力で経路を選ぶ。seekable な `objstore.Local` は Range 対応のローカル配信に使い、`storage.accel_location` が設定されていれば `Open` で存在を確認してから `X-Accel-Redirect` を返す。そうでなければ `objstore.SignedURL` で署名付き URL へ redirect する。原本の配信は引き続き POSIX file を開き、同じ `X-Accel-Redirect` 設定を使う。

### scratch と temp の置き場所

生成物は一時領域で完成させ、公開先の名前を直接書き換えない。

ffmpeg の出力はワーカーのローカルスクラッチへ書く。原本の ingest temp は最終配置と同じ root・同じディレクトリに作る。派生物は scratch から `Store.Put` で公開するため、呼び出し側から canonical キーのディレクトリへの rename は行わない。

scratch から rename すると `EXDEV` になり、コピーへの劣化を許すことになる。

scratch directory 名は Worker の復旧単位から決める。encode は `(recording_id, profile, attempt_count)`、CM detect は `(recording_id, attempt_count)` を使う。CM logo candidate は `(recording_id, attempted_at)` を使う。thumbnail と seek tiles は recording ID を接頭辞にして毎回 `MkdirTemp` で作る。

CM detect と CM logo candidate はプロセス死の残骸を回収しない。CM 検出は死後の次の開始で `attempt_count` が進み同じ名前に戻らず、ロゴ候補は `MkdirTemp` だからである。中身は原本への symlink と小さいファイルだけなので、残っても実害は小さいとして許容する。

ingest は同じ root・同じディレクトリの record 固有 temp へ書く。temp 名は `.rokuban-ingest-{site}-{record_id}-g{writer_generation}` である。writer 世代は DB lease で採番する。HLS の `temp_generation` は別に管理する。新 writer が以前の表示中の offset に追いつくまでは旧 path を指し、追いついた後は path だけを切り替える。`temp_generation` は維持する。

予約 basename は、mirakc の contentPath には使わない。次の 4 つの prefix で始まる名前である。

`.rokuban-ingest-` は ingest temp、`.rokuban-rel-path-lock-` は旧形式 lock file の prefix である。`.rokuban-media-asset-` は fsstore が `Put` の途中に作る staging file の prefix で、`List` は返さない。`.rokuban-encode-` は旧 worker が使った prefix で、いまの実装は作らず、残骸としてだけ現れる。どちらも孤児回収が消す。

rel_path lock は media root の `.rokuban-locks/` に置く。この directory 名も予約し、DB の `rel_path` として受け入れない。置き場の目印の root の `.rokuban-store` も同じく予約する。ネストした同名ファイルは予約しない。

ingest は canonical path を転送中に触らない。HEAD の長さと、存在する場合は `content.sha256` を照合してから、temp を `fsync` して `Close` する。次に DB transaction 内で original の論理資産とコピー行を INSERT して rel_path を予約し、`link(temp, canonical)` で公開して親ディレクトリを `fsync` する。DB commit が成功したら、置き場の目印を照合して temp を unlink する。temp の直列化には record 単位の DB lease を使い、canonical の公開・削除だけは `rel_path` lock を使う。

公開に `os.Rename` を使わないのは、宛先が既存でも無言で置き換えるからである。この契約は、DB に行の無い canonical 名のファイルを自動で消さないと決めている。rename はそのファイルを無言で置き換えて失わせる。`link` は宛先が既存なら `EEXIST` で失敗するので上書きしない。

`renameat2(RENAME_NOREPLACE)` も使わない。Linux の NFS クライアントは flag 付きの rename を `EINVAL` で断る（`fs/nfs/dir.c` の `nfs_rename`）ので、NFS では必ず失敗する。この契約は条件を満たす NFS を置き場に含める。link なら全 OS で同じ経路になる。

宛先に既にファイルがあるときは、temp と canonical が同じ inode かを `os.SameFile` で判定する。同じなら前回の試行が link した後に commit で失敗した状態なので、公開済みとして commit へ進む。別の inode なら regular file・サイズ・SHA-256 を照合し、全て一致するときだけ既存内容を採用する。それ以外は他人のファイルとして temp を残し、mirakc の record を消さず、`media_assets` 行も作らずにエラーで止める。この照合はストレージ間転送の既存宛先採用と同じ helper を使う。

未解決: この照合は canonical 全体を読んで SHA-256 を計算する。報告された読み直し速度（約 9.8 MB/s）では、地デジ 30 分（約 3.8 GB）の照合は 5 分の Work 時限を超える見込みである（未測定）。

`link(2)` の BUGS 節は、NFS では link が成功しても戻り値が失敗になりうると書く。そのためどの失敗でも `SameFile` で確かめる。JuiceFS での hard link の動作は未検証である。

link 後の temp は canonical と同じ inode である。新しい writer 世代は別の temp path を使うため、公開済みの canonical を追記しない。link の前に、全量の最終 handoff（offset と SHA-256 state）を DB に保存する。次の試行は writer 世代の temp と canonical が `SameFile` で、サイズが保存 offset と一致し、state を復元できるときだけ、保存した SHA-256 から commit をやり直す。temp は読み直さない。最終 handoff が無い link 済み temp は全量を読んで SHA-256 を計算する（未解決: 上の読み直しの費用と同じ）。

<details><summary>根拠</summary>

- `.rokuban-locks/` を `rel_path` として受け入れないこと: `mediapath.Resolve`
- root の `.rokuban-store` だけを拒むこと: `TestResolve_ReservesRootStoreMarkerOnly`
- 異なる inode の一致採用: `TestIngestWorker_AdoptsMatchingCanonicalWithDifferentInode`
- 同じサイズの異なる内容を拒むこと: `TestIngestWorker_RefusesCanonicalWithSameSizeButDifferentContent`

</details>

### 公開順序と失敗時の扱い

失敗した公開は DB に反映せず、再試行や reconcile で回復する。

原本の公開点は DB commit であり、link と親ディレクトリの fsync の成功が先に来る。名前だけでは公開済みか判定できないので、canonical file は自動で消さない。派生物は、`Store.Put` が成功した後に DB を commit して公開する（[派生物の公開（encode）](publish.md)）。

原本は link 前の失敗では temp を残して次の試行へ渡す。中身の不一致、または record の cancel / fail が分かった場合だけ temp を消す。

原本の link 後に fsync か commit が失敗したら、transaction を rollback する。temp は canonical と同じ inode のまま残り、次の試行は保存済みの最終 handoff から SHA-256 を復元して commit をやり直す。canonical が行の無いまま残るのは、この再試行が成功しない場合である。孤児回収が先に temp 名を消した後でも、次の ingest は既存 canonical のサイズと SHA-256 が一致すればその内容を採用できる。異なる内容なら canonical は他人のファイルとして扱われ、ingest は止まり、エイジング後に orphan として報告される。

mirakc record は、commit が成功するまで削除しない。commit 後の temp の unlink が失敗しても、ログだけで成功扱いにする。

**この順序を反転させない**。DB commit 後に link すると、行が指す実体の欠落を作る。

原本 ingest のコピー完了には、fsync と Close のエラー確認まで含める。転送中の fsync は、4 分の区切りで checkpoint を保存する直前と転送完了後に限り、バッファごとには行わない。区切りでは `fsync` と `Close` の両方が成功してから、lease を検証して offset と SHA-256 state を DB に保存する。clean resume は temp サイズが保存 offset と一致するときだけ許し、サイズ不一致時は新しい writer 世代の temp を Range 0 から作る。

区切りごとの fsync のコストは NFS / JuiceFS で未測定である。FUSE S3 は原本の置き場に使わないので、途中 fsync で fd に書けなくなる実装は考慮しない。Linux では遅延した書き込みエラー（ENOSPC / I/O エラー）は `Close` では報告されず `fsync` でしか上がらない。link 後の親ディレクトリ `fsync` は新しい directory entry の永続化を確定する。いずれかが失敗したら DB 登録と record 削除をせず再試行する。

ingest temp の回収は DB の writer 世代と表示中の `temp_rel_path` を照合する。現在の世代または表示中の path を指す temp は残し、より古い writer 世代で表示中でもない temp は回収できる。writer 世代を判定できない旧形式の名前は mtime 猶予とエイジングの後に回収する。

canonical orphan はエイジング後に報告するだけで unlink しない。通常の canonical 公開と削除は `rel_path` filesystem lock を DB commit まで保持し、通常削除との競合を防ぐ。

<details><summary>根拠</summary>

- temp 世代回収: `TestDeleteReconcileWorker_KeepsVisibleIngestWriterAndDeletesSupersededGeneration`

</details>

### 置き場間の転送

転送 worker の再開、公開、配置削除、破損時の扱いは [置き場間の転送](transfer.md) に従う。

### rel_path lock file の寿命

パスごとの lock file は、通常終了のときに削除してから解放するので、何度処理しても残らない。異常終了で残った lock file は、次の lock 取得時の掃除が回収する。

rel_path lock は `.rokuban-locks/<prefix><sha256(rel_path)>.lock` に置く。canonical file と同じ directory entry ではないため、canonical の rename / unlink で lock 対象が置き換わらない。通常終了時は per-rel_path lock を保持したまま gate を排他し、lock file を unlink してから flock と fd を解放する。したがって複数回の ingest / encode / delete の後も per-rel_path file は残らない。`.gate.lock` は lock directory の調停に使う固定 file として残る。

取得側は gate を共有して lock file を開き、`LOCK_NB` で試す。busy ならその fd を閉じて gate を解放してから待ち、再度 path を開く。release と GC は gate を排他するため、unlink と同時に古い inode を待つ fd は作られない。これが「A が unlink、B が古い inode を取得、C が新 inode を取得」という二重 lock を防ぐ規則である。active lock は残して次回に回す。lock 取得や cleanup に失敗したときは、ファイル操作を進めずにエラーを返す。

この GC は lock file の directory entry だけを扱い、メディアファイルは扱わない。

プロセスが異常終了すると kernel が gate と per-rel_path flock を解放する。unlink 前なら lock file の directory entry は残るが、次の lock 取得時の GC が gate 排他下で stale file を回収する。GC 中に active owner がいる file は flock が取れず削除されない。複数プロセスは同じ media root とこの gate protocol を使う必要がある。POSIX `flock` の前提はこの文書のルール 4 と同じで、JuiceFS / NFS 越しの実効性は未検証。

lock directory は mode `0777`、gate / per-rel_path file は `0666` で作り、通常どおり umask を適用する。異なる uid の worker を同じ media root で動かす場合は、共有 group / ACL が directory の作成・削除と lock file の read-write を許すことを先に確認する。

#### 旧形式の lock file と更新時の並走

旧形式の lock file は、移行時に自動削除しない。旧 worker が新しい排他の手順に参加しないため、消すと旧プロセスの待機側が古い inode を握る可能性がある。旧 worker と新 worker が並走している間は、消す操作も実行しない。

旧形式とは、canonical と同じ directory の `.rokuban-rel-path-lock-*.lock` である。

新形式も同じ接頭辞を `.rokuban-locks/` の下で使う。そのため接頭辞だけで消す `find -name '.rokuban-rel-path-lock-*' -delete` は、稼働中の新形式 lock まで消す。旧形式だけを消すときは `.rokuban-locks/` を除外する。`-delete` は `-prune` と併用できない（`-depth` を暗黙に有効にする）ので `-exec rm` を使う。

```bash
find "$MEDIA_ROOT" -type d -name .rokuban-locks -prune -o \
  -type f -name '.rokuban-rel-path-lock-*.lock' -exec rm -- {} +
```

旧 worker と新 worker が同じ media root に並走している間は、この削除も実行しない。

k8s の `worker-scaledjobs.yaml` は全 ScaledJob が `rollout.strategy: gradual` なので、更新中は旧イメージの実行中 Job が完走するまで新しい Job と並走する。並走の原因はこの設定であり、ScaledJob 一般の性質ではない。この窓では旧 worker が新 worker の gate を知らないまま、同じ rel_path を別の inode で lock しうる。

更新前に `autoscaling.keda.sh/paused=true` を全 worker ScaledJob に付け、CronJob を suspend し、watcher Deployment を 0 replica にする。この annotation は[KEDA v2.20 の公式文書](https://keda.sh/docs/2.20/concepts/scaling-jobs/#pausing-autoscaling)にある。pause は新しい Job の起動を止める。実行中の Job は止めない。

全 Job の `ACTIVE` が 0 または `<none>` になってから新しいイメージを適用する。確認コマンドは[更新手順](../runbook/setup.md)にある。

pause 中も River 行は DB に残る。pause 解除後は KEDA が Job を起こす（[runbook](../runbook/setup.md)「旧版からの更新」）。

この操作は kind v1.36.1 / KEDA v2.20.2 で測定した（2026-11-02）。試験では media PVC を mount した PostgreSQL fixture consumer を使った。

5 秒 polling の ScaledJob で 20 秒動く Job を作り、実行中に pause して River に 2 件目を追加した。既存 Job は完了した。その後 16 秒（3 polling intervals）見ても Job は 1 件のままで、2 件目は `available` に残った。

pause 解除後は Job が 1 件増え、最終状態は `completed:2`、待ち行列 0 件だった。単一プロセス構成では migration 前にそのプロセスを正常停止する。

止められない場合は、この並走窓で同じ rel_path を触る ingest / encode / 削除が二重に lock を取りうるリスクを受け入れる。旧形式の残置 file は、全旧 worker の停止後に上のコマンドで消す。

### 字幕サイドカー

字幕を有効にしたエンコードは、エンコード済みファイルの隣に同じ basename の字幕サイドカーを置く。

サイドカーの拡張子は `.vtt` で、字幕は `media_assets.kind` を増やさず、`/file?profile=X&track=subtitles` で encoded アセットから解決する。ARIB 外字・ルビは libaribcaption のテキスト化により Unicode 私用領域または近似字へ落ちるため、画像字幕と同じ見た目を保証しない。

### thumbnail / seek tiles の公開

サムネイルとシークタイルは、完成してから置き場へ保存し、DB commit で公開する。

thumbnail と seek tiles も、完成したファイルを scratch で作って `Store.Put` し、その後の DB transaction で `active` にする。DB で active のコピーだけが配信を許す。同じ録画の job が重なっても、先行が公開済みか原本が active でなければ、後続は commit を飛ばして成功扱いにする。

thumbnail と seek tiles の ffmpeg 出力先は recording ID を接頭辞にして `MkdirTemp` で作る scratch directory とする。毎回 suffix が異なるため、同じ recording の実行が重なっても scratch file を共有しない。

生成後は `pending` のコピーを準備し、`Store.Put` で canonical なキーを一度だけ作る。`active` な派生物の行と原本の生存を再確認してから DB transaction を commit する。

thumbnail と seek tiles の次世代は、論理行と削除済みを含む copy 履歴の大きい方から採る。古いキーの削除が遅れて完了しても、再公開したキーを消さないためである。

先行 job がすでに `active` の行を commit していた場合や、ffmpeg 実行中に録画が削除され原本が active でなくなった場合、後続 job は公開を飛ばして成功扱いにする。

FS store 内の staging file は予約 prefix 付きで `List` から隠し、mtime 猶予とエイジングの後に回収する。S3 store の multipart upload は Put の中で完了または abort する。canonical を直接書き換えないため、処理中に配信側が途中の画像を観測しない。

<details><summary>根拠</summary>

- 同じ recording の並行実行で scratch directory を共有しないこと: `TestNewWorkerScratchDirIsUniqueForSameRecording`

</details>

### catalog 世代の公開

catalog の世代は、最後に書く manifest が完成の判定点である。rename や DB 行は使わない。不完全な世代は 7 日間保持し、より新しい完成世代があるときだけ prune する。

catalog export は `catalog/` の新しい世代 directory を `Mkdir` で原子的に予約する。同じ `ExportedAt` を持つ並列 export は `-02` 以降の別 directory を取得する。世代内では `catalog.json` を一度書いて file `fsync` し、sha256 とサイズを持つ `manifest.json` を最後に書いて `fsync` する。

別 export の prune が書き込み途中の世代を消さないよう、不完全世代は保持する。catalog directory 以外へは触れない。

### カット版・サムネイルの置き換え（「置くのは一回」の 2 つの例外）

カット版とサムネイルを作り直すときは、同じパスへ上書きせず、新しい世代のコピー行を追加して公開する。旧世代の物理行は残し、配信・派生物の有無を調べる読み手からは世代カウンターで隠す。旧世代の unlink は削除 reconcile が overlap の後に行う。

カット版（`encode.profiles[].cut`）は、チャプターを直した後に同じ出力を作り直す。同じパスへ上書きしない。ルール 2 の「置くのは一回」に反し、`(store, rel_path)` の全体一意索引とも衝突するからである。

- 世代は `media_assets.derived_generation` から採る。cut は `…_{profile}.g{n}.{container}`、uncut の tombstone 再生成は `…_{profile}.u{n}.{container}` に置く。旧世代付きパスは移行時にコピーの世代へ読み込む
- 新しいキーへ `Store.Put` し、1 つの tx で論理サイズ・`derived_generation`・`media_asset_cuts` と新しい物理コピー行を active にする。コピー行の世代は後から書き換えない。旧世代は同じ置き場に残り、削除 reconcile が overlap 後に回収する
- 置き換え時に旧コピー行を書き換えない。書き換えると、旧キーの所在と世代を削除 reconcile が追えなくなる。公開した worker は旧キーを消さないので、commit が失敗しても旧キーと旧行は残る

CM 検出の後にサムネイルを選び直す場合も、同じパスへ上書きしない。

- 初回の `thumbnails/{recording_id}.jpg` は世代なしのままにし、差し替え・tombstone 再生成は `thumbnails/{recording_id}.g{n}.jpg` へ置く。n は回収済みコピーを含む最大割当済み世代から進める
- 新しいキーへ `Store.Put` し、1 つの tx で論理サイズ・`derived_generation`・新しい物理コピー行と `media_asset_thumbnail_seeks` を active にする。旧コピー行は保持し、旧キーは overlap 後に削除 reconcile が回収する
- 公開した worker は旧コピーをその場で削除しない。commit 直後に落ちても、定期の削除 reconcile が旧世代を見つけ、`Store.Delete` と DB の deleting/deleted 状態遷移で削除を再開する

<details><summary>根拠</summary>

- 旧世代パスからの世代引き継ぎ: `TestMediaAssetCopiesMigrationBackfillsCopiesAndGenerations`

</details>

ポイントはルール 3。DB commit を公開点にする。原本は link と親ディレクトリの fsync が成功してから DB に登録し、派生物は `Store.Put` の完了後に DB で `active` のコピーを確定する。起動時 probe は原本 ingest の操作列を確認し、FS の種類や atomic rename の実装品質をパス文字列から推測しない。

## 4. クラウド側のマウント選択肢

クラウドで原本を置く先は、ローカル FS が第一候補で、JuiceFS と条件を満たす NFS も対象にする。FUSE S3 は原本にも派生物にも使わない。JuiceFS のメタデータストアに Postgres を使うなら、Rokuban とは別のインスタンスにする（推奨）。

| 選択肢 | 特徴 |
|---|---|
| ローカル FS | file fsync / Close / atomic rename / 親 directory fsync を通常の POSIX 意味論で満たす。第一候補 |
| JuiceFS（対象内） | メタデータを DB に、データを S3 に置く FS。atomic rename を含む POSIX 意味論を信頼できる構成で使う。hard link の動作は未検証。メタデータストアに PostgreSQL を使う場合は別インスタンスを推奨 |
| NFS（対象内） | export は `sync`、client mount は `hard` を推奨。ingest の公開に `RENAME_NOREPLACE` は使わず、link の誤った戻り値は `SameFile` で回収する。`.nfsXXXX` の silly rename が canonical orphan として報告される場合は、file を閉じた後に状態を確認する。自動削除はしない |
| k8s-csi-s3（geesefs / s3fs）・AWS Mountpoint | 原本・派生物の置き場には使わない。派生物の S3 利用は API から行う |

**注意**: JuiceFS のメタデータストアに Rokuban と同じ Postgres インスタンスを使うと、DB 障害がストレージ障害に連鎖し「DB が詰まっても仕事は失われない」の前提を成り立たなくする。使うなら別インスタンスを明記すること。

## 5. 2 階層: 録画バッファとアーカイブ

「mirakc が直接書くストレージは高速に、録画後の保存先はアーカイブ用途で低速に」という分離は、ingest の設計（[録画エンジン](../recording.md) 参照）が既に実現している。新機能は不要で、「録画後のファイル移動」= ingest そのもの。原本 ingest root は上の強い FS 契約を満たす。

### 2 階層の対応関係

録画バッファは mirakc が書くエッジの高速なディスクで、アーカイブは Rokuban のメディアストレージで低速でもよい。mirakc に最終保存先を直接書かせないのは、録画がやり直せないリアルタイム処理で、遅いストレージのストールが放送の欠損に直結するからである。

| 階層 | 実体 | 要件 | 寿命 |
|---|---|---|---|
| 録画バッファ | mirakc `recording.basedir`（エッジのローカルディスク） | 高速・低レイテンシ（I/O 飽和 = ドロップ直結） | ingest コミット後に record 削除（リングバッファ） |
| アーカイブ | Rokuban のメディアストレージ（ローカル FS / 条件付き NFS / JuiceFS） | 低速可（書き込みはリトライ可能な転送のみ。ただし原本 root の強い FS 契約は必要） | 保持ポリシーに従う |

録画はシステム内で唯一のリアルタイム・リトライ不能な操作である。monolith モードでも basedir を NVMe、メディアストレージを HDD/NAS に置くだけで同じ分離が機能する（設定レベルの話でコードは変わらない）。

### 録画バッファのサイジング指針

録画バッファの容量は、取り込みが詰まったときの滞留分で決まる。同時録画数は支配項にならない。速度で怖いのは、他の I/O との競合によるレイテンシのスパイクで、絶対帯域は問題にならない。

回線断・クラウド側障害時は未 ingest の record が溜まり続ける。推奨値は「N 日分の全録画を保持できる容量」（地デジ約 7 GB/時で見積り）とし、既定の「未 ingest record 総量メトリクス + エッジディスク残量アラート」と対にする。**ただしそのメトリクスは回線断の滞留を数えない**（`record_sync` は watcher の観測でしか増えないため。[運用](../operations.md) §4「N 日は容量だけでは決まらない」）。同節のとおり、N の上限を決めるのは `epg.retention_grace` で、容量では決まらない。

書き込みは 1 録画あたり約 2 MB/s（地デジ 17 Mbps）で、同時 8 本でも 16 MB/s に過ぎない。ingest pull のサイト単位 1〜2 本キャップは、レイテンシスパイクを避けるための決定でもある。

### アーカイブの速度要件

アーカイブに求める速度は、平均スループットが 1 日の録画総量を 24 時間で割った値以上であること。瞬間的な変動は録画バッファが吸収するので、リアルタイム性は一切要求されない。エンコードの読み出しもバッチなので遅くてよい。

唯一レイテンシが人間に見えるのは**再生時のシーク**である。原本削除ポリシーと組み合わせた「視聴は H.265 派生物、原本は消すか別の低頻度領域」という運用が前提なら、実用上問題にならない見込みである。

### 派生物の置き場

論理資産と物理コピーは、派生物に限らず原本でも分ける。コピーを置き場の間で移しても、論理資産の identity は変わらない。派生物の置き場は、全 site 共通で物理的に 1 か所にする。どの worker からも読めるようにするためである（S3 なら scratch へ取り寄せて読む）。

論理資産は `media_assets` の行で、派生物は録画・種別・profile・`derived_generation` で識別する。

### 残量の観測

ストレージの残量は、ファイルシステムを持つ worker が定期的に観測して射影し、API から読めるようにする。対象はアーカイブとローカルのスクラッチだけで、mirakc の録画バッファは対象外である。

`storage.media_dir`（アーカイブ）と `storage.scratch_dir`（ローカルスクラッチ）の容量は、worker が定期的に statfs 相当で観測して `storage_sync` に射影する。`GET /api/storage` で読める。api ロールはファイルシステムに依存しない（不変条件 1）。そのため観測は、ファイルシステムを持つ worker の仕事に限る。mirakc の recording.basedir（録画バッファ、上記「2 階層」表のエッジ側）は対象外である。Rokuban 自身が直接読み書きする 2 つのローカルパスだけを見る。

`tuner_sync`（[docs/data.md](../data.md) §6.5）と同じ「使い捨てプロジェクション」の形を採る。正しい状態は常にファイルシステム側にあり、毎パス全量を作り直せる観測値である。そのため全行 upsert で常に最新観測だけを保持する（過去の観測を積むログにはしない。不変条件 9）。`observed_at` の鮮度が「観測ループが止まっている」ことを示す唯一の手がかりになる（警告が出ないことは保証ではない。docs/data.md §6.5 の同じ姿勢である）。

### rel_path の名前空間

原本の保存先には、録画ごとに再利用されない相対パスを使う。判定基準は次のとおり。

- 同じ名前の再録画でも、別の path になる
- 永久に一意である。DB を失って ID 列がリセットされても衝突しない。recording_id だけで作らない
- rescue が site を読み取れるよう `sites/{site}/` を保つ
- 派生物の `EncodedRelPath` が同じディレクトリを引き継げる

recording_id だけで名前を作ると、DB の再構築後に rescue が採番した録画と新しい ingest が同じ path を使う可能性がある。

ingest は、mirakc の `Record.Content.Path` の basename だけを使い、`sites/{site}/{uuid}/{basename}` を候補にする。contentPath のディレクトリ部分は使わない。uuid は乱数（UUID v4）なので、ID 列がリセットされても衝突しない。uuid は最初の Work でチェックポイントに保存し、再試行と commit は保存済みの `rel_path` を使う。同じ basename の録画を再取得しても uuid のディレクトリが異なり、最初の Work で保存したパスは失敗後も変わらない。

#### 予約ディレクトリと前置の規則

この節は、トップレベルの予約ディレクトリの用途と、原本の前置の形を定める。

トップレベルの予約ディレクトリは `catalog/` / `thumbnails/` / `sites/` である。`catalog/` は削除 reconcile の孤児回収と rescue スキャンが SkipDir する。`thumbnails/` はサムネイルの名前空間（§5.1）で、`sites/` は rescue が site を読み取る原本の名前空間になる。

サムネイルは `thumbnails/{recording_id}.jpg` のまま（§5.1）。派生物は原本の dir を引き継ぐ（[retention.md](retention.md) §6 参照）。原本が `sites/tokyo/{uuid}/recording.m2ts` なら、派生物は同じ directory の `recording_h264.mp4` になる。

原本と encoded の全行は `sites/{site}/` 前置済みである。前置なしの行は worker の起動時検査で拒否する。

#### リンクの禁止と root の symlink

メディアディレクトリの配下には、録画の実体を指すリンクを作らない。メディアディレクトリ自身が symlink であることは許す。孤児回収と rescue が root を解決してから走査するからである。

`media_dir` 配下に、録画の実体を指すリンクを作らない（symlink / hard link）。孤児回収は symlink かどうかを見ずに DB の行と突き合わせる。未知の canonical 名になったリンクは孤児として報告するが、自動削除しない（`mediapath.Resolve` は字句判定のみで symlink を評価しない）。

hard link は regular file と区別できず、同じ実体に live な録画が 2 行並ぶ。例外は ingest が公開中に持つ予約名 temp と canonical の hard link だけである。temp は writer lease が所有し、unlink はリンク数を減らすだけで canonical を消さない。`(dev, ino)` による検出は、スキャンをまたぐと inode がバックアップ復元で変わるため実装しない。rescue の走査と in-place 登録は symlink だけを弾く。

ディレクトリへの symlink も作らない。rescue と孤児回収の走査は symlink を辿らないため、配下のファイルは孤児候補にも rescue にも見えず、災害復旧で救えない。symlink エントリ自身は孤児として報告するが、自動削除しない。リンク先が `media_dir` 内を指す構成では、配下の active 行が実体無しとして誤報され続ける。

`media_dir` 自身が symlink であることは許す（`/var/lib/rokuban/media -> /mnt/disk1/media`）。rescue と削除 reconcile の両方が root を `filepath.EvalSymlinks` で解決してから walk するためである。

この解決を外すと、両方とも気づかないうちにおかしくなる。`filepath.Walk` / `WalkDir` は symlink root に `Lstat` し、`IsDir()` が false なら callback を 1 回呼んで終わる。rescue は 0 件のまま「成功」する（災害復旧が最も要る場面だけが使えなくなる）。削除 reconcile は `seenOnDisk` が `.` の 1 件になるため、全損セーフガード（走査結果が 0 件なら記録を見送る）が働かない。その結果 `active` 行を全件「実体無し」と誤報する。

解決した値は root だけでなく、`catalog/` の除外判定と `rel_path` の基準にも同じものを使う。片方だけ解決すると、相対パスは `../` を含み DB の `rel_path` と一致しなくなる。root を解決することと配下にリンクを作らないこと（上の 2 つ）は別の話であり、片方をもう片方の根拠にしない。

#### 予約名の 2 種類

トップレベルディレクトリ名の予約は、一般のディレクトリ名として使うと正しく動かなくなるので外せない。site 名の禁止は、パス衝突を防ぐ理由では残しておらず、緩める自由度が変更コストに見合わないために残してある。

トップレベルディレクトリ名の予約と site 名の禁止は、根拠が異なる。

1. トップレベルディレクトリ名の予約は `catalog` / `thumbnails` / `sites` の 3 つである。`catalog/` は削除 reconcile の孤児回収と rescue スキャンが読み飛ばす対象、`thumbnails/` はサムネイルの名前空間、`sites/` は本節の原本の名前空間である。いずれも一般のディレクトリ名として使うと正しく動かなくなるため、この予約は外せない。
2. site 名としての `catalog` / `thumbnails` は禁止する。導入時は `{site}/` を先頭成分にしており、この 2 つはトップレベル予約ディレクトリと直接衝突していた。`sites/` を挟んだ今は、site 名が `sites/{site}/...` に閉じるため、トップレベルの `catalog/` / `thumbnails/` と構造的に衝突しない。

この禁止を残すのはパス衝突を防ぐためではない。`catalog` / `thumbnails` を site 名にしたい運用要求がなく、禁止を緩める自由度がバリデーションとテストの変更コストに見合わないためである。`sites` 自体を site 名にしても `sites/sites/...` となるだけで衝突しない。

<details><summary>根拠</summary>

- `.rokuban-locks/` を `rel_path` に使わない検査: `mediapath.Resolve`
- record filename から作る UUID directory 付き path: `internal/worker/ingest.go` の `determineRelPath`
- `sites/{site}/` 前置と basename の検査: `internal/catalog/rescue_scan.go` の `SiteRelPath`
- 孤児回収の file walk と root symlink の解決: `internal/worker/storage_audit.go` の `walkMediaFiles`
- rescue が root symlink を解決し、in-place 登録時に symlink を拒否すること: `rescueStorage`、`inplace.Register`
- `catalog/` の除外と相対パスの基準に同じ root を使うこと: `filepath.Rel`
- 派生物の `rel_path` を作る関数: `EncodedRelPath`
- 予約ディレクトリと site 名の定義: `internal/config`, `internal/config.reservedSiteNames`

</details>

## 5.1 サムネイル

サムネイルは、有効な原本があり、有効なサムネイルが無く、ごみ箱に入っていない録画だけに、定期的な突き合わせで作る。抽出位置は初回は尺の 1 割か 30 秒の短い方で、CM 検出後は CM を除いた区間の合計尺に同じ方針を適用して選び直す。

録画 1 本につき `kind = 'thumbnail'` の media_asset を 1 つ作る（`UNIQUE (recording_id, kind, profile)`）。

初回の抽出位置（仮サムネイル）は `seek = min(duration × 10%, 30s)` である。duration は原本の ffprobe が読む実ファイル長で、取れなければ 0 秒（先頭フレーム）にする。初回は CM 検出を待たず、従来どおり原本から作る。設定キーは設けない。

- 時間軸: カット境界は入力の最早 `start_time` を 0 とする秒で適用する。thumbnail の `-ss` も `-i` より前の入力シークであり、同じ数値の位置を渡す。chapter ranges と `seek_ms` は `recording_chapter_spans` / `media_asset_cuts.keep_ranges` と同じ ms を使う。放送 TS は音声が映像より先に始まることがあり、章の原点（最初の映像フレーム）と入力の最早 `start_time` の差は未測定。そのため、この値が同じ画面を指すことと、原本 / encoded 版間で同じ `seek_ms` が揃うことは実録画で確認する
- 画素縦横比: ffmpeg で入力の SAR を偶数幅の正方形ピクセルへ焼き込んでから JPEG 化する。JPEG の SAR を解釈しないブラウザでも anamorphic 映像を歪ませない
- ストレージ契約: ffmpeg は `storage.scratch_dir` に JPEG を書き、完成後に `Store.Put` で派生物の置き場へ確定する。DB transaction は `active` のコピーと `media_asset_thumbnail_seeks` を同時に commit する（差し替え規約は §3）
- 相対パス: 初回は `thumbnails/{recording_id}.jpg`。差し替えは `thumbnails/{recording_id}.g{n}.jpg`（n は `derived_generation` から進めた世代）。原本の contentPath に依存しないので、原本削除後もパスが安定する
- 位置の事実: `media_asset_thumbnail_seeks` に、その JPEG を切り出した原本時間軸の `seek_ms` を 1 行記録する。旧サムネイルで行が無い場合は位置不明として扱う
- 配信: streamer の `GET /api/media/recordings/{id}/thumbnail`（openapi 外。api はファイルを開かない）

#### 投入の条件

レベルトリガーで投入するのは、有効な原本があり、有効なサムネイルが無く、ごみ箱に入っていない録画だけである。原本の実体が無いと確認された録画も除く。ingest 後の限定パスと定期の全件パスは、同じ条件を使う。

投入はレベルトリガーで、River `thumbnail` キューへ unique ジョブ（`recording_id`）を積む対象は、次の条件を満たす録画だけである。active な original があり、active な thumbnail が無く、かつごみ箱（`recordings.deleted_at IS NOT NULL`）に入っていないこと。

ごみ箱の録画を除外するのは、配信側（`GetThumbnailMediaAssetForServing`）が `deleted_at IS NULL` を要求するためである。生成しても誰にも配られず、猶予期間ぶん ffmpeg を無駄打ちするだけになる。

ingest 後の対象限定パスと `thumbnail_reconcile` の全件パスは同じ候補 query を使う。delete reconcile が原本の実体無しを確認した `missing_media_assets` の原本は、両方のパスで除外する。ファイル復旧後にマーカーが消えれば、次のパスで再び候補になる。命令的チェーン（「ingest 成功 → 必ず thumbnail」）は採らない。

<details><summary>根拠</summary>

- ごみ箱の録画をサムネイル配信対象から除くこと: `GetThumbnailMediaAssetForServing`
- ingest 後の対象限定パスと全件パスが同じ候補を使うこと: `thumbnail_reconcile`
- 原本の実体が見つからない状態の除外: `missing_media_assets`

</details>

#### 抽出位置の選び直し

CM 検出後は、CM 区間を除いた残り区間の合計尺に同じ 1 割か 30 秒の方針を適用して選び直す。記録済みの位置が現在の残り区間の外にあり、新しい計画の位置とも違うときだけ作り直す。

選び直しでは、同じ `min(尺 × 10%, 30s)` ポリシーを、CM 区間を除いた keep の合計尺に適用する。原本またはカットしない encoded 版ではその位置を原本時間軸へ写して抽出する。カット版では凍結した `media_asset_cuts.keep_ranges` の合計尺で位置を決め、抽出位置を同じ凍結 keep から原本時間軸へ戻す。カット版の尺は ffprobe せず凍結 keep の合計を使う。

再選択の入力は、active かつ `missing_media_assets` に無い原本を優先し、次にプロファイル名昇順のカットしない encoded、最後に同じ順のカット版を選ぶ。未確認の自動 CM 検出も使う。チャプターがない録画は再選択せず、keep が空なら再選択を繰り返さない。

S3 上の encoded 版を入力に選んだ場合は、scratch へ取得してから抽出する。

作り直す条件は、記録済み位置が現在の keep 外にあり、同じ入力選択で計画した位置とも異なる場合だけである。`seek_ms` が無い旧サムネイルは、チャプターと使える入力があれば一度選び直す。位置が keep 内なら、望ましい位置との違いだけでは作り直さない。

再選択 reconcile は、active なサムネイルと CM 検出または所有済みチャプターがある録画を recording ID の窓で巡回し、Go の共通判定で再選択の対象を投入する。ごみ箱は除外し、`seek_tiles` や初回生成とは別の再開位置を持つ。窓の再開位置は継続ジョブの引数で引き継ぐ（[ingest-followup.md](../recording/ingest-followup.md) §5.5）。再選択が要らない録画も窓に入るので、対象が上限を超えると、通常運転でも周期ごとに継続ジョブが連鎖する。CM 検出の保存後に 1 件ヒントを出すが、ヒントはベストエフォート。手動編集は次の窓で拾う。

<details><summary>根拠</summary>

- keep range の位置を原本時間へ写すこと: `UnmapMs(keep, seek)`
- 入力の時間原点を 0 にしてカット境界を適用すること: `ffargs.CutFilterComplex`
- S3 上の encoded 版からサムネイルを再選択すること:

  ```text
  TestS3DerivedObjectPublicationAndRecovery/thumbnail_reselection_reads_encoded_input_from_S3
  ```

</details>

## 5.2 シークプレビュー用タイル

シークプレビュー用タイルは、録画 1 本につき 1 枚の JPEG で、形は固定値にしてあり設定キーは設けない。生成に失敗したらスクラッチを捨ててやり直し、部分的な成果はコミットしない。

録画 1 本につき `kind = 'seek_tiles'` の media_asset を 1 つ作る（`UNIQUE (recording_id, kind, profile)`）。初回は `thumbnails/{recording_id}_tiles.jpg`、再生成は `thumbnails/{recording_id}_tiles.g{n}.jpg` に置く。n は `derived_generation` から進め、tombstone と未 commit の出力パスを再利用しない。poster（§5.1）と同じ `thumbnails/` の名前空間なので、`rel_path` の名前空間の検査は増えない。

形は固定値である（設定キーは設けない。poster と同じ流儀）。間隔 10 秒・1 枚 160x90・10 列・上限 1080 枚（3 時間）。3 時間を超える部分にはタイルが無く、クライアントはプレビューを出さない。尺に応じて間隔を変える方式は採らない。ffprobe の長さと `<video>` の長さのずれが、境界でタイル位置を狂わせるからである。

- 投入: `thumbnail_reconcile` の全件パスと ingest 後の対象限定パスが seek_tiles を投入する。両者は poster と同じ候補 query を使い、`missing_media_assets` の原本を除外する。対象限定パスも seek_tiles を拾うため、タイルは ingest 直後に投入される。パス内では poster と再選択を先に投入してからタイルを積む。priority は poster より下げる。同じ priority だと既存録画ぶんのタイルが片付くまで新しい録画の poster が一覧に出ない。thumbnail キューは既定で同時実行数 1 なので、タイルの処理中に来た poster はその 1 件を待つ
- 原本が無い録画には作らない。タイルは原本からしか作らないので、タイル導入前に `until_encoded` で原本を消した録画にはタイルが付かない
- 失敗したら scratch を捨ててやり直す。部分成果はコミットしない（行の存在 = 全部そろっている）。poster と違い、長さが取れないとき（0 秒を含む）は 1 枚で続行せずに失敗する。1 枚の格子をコミットすると定期パスが二度と作り直さず、`until_encoded` の原本削除の条件まで満たしてしまう。バージョンによっては ffmpeg が 0 フレームでも終了コード 0 で終わるかもしれない（未検証。ffmpeg 9.0.2 は非 0）。そのため 1 枚ごとに出力の有無を確かめる
- 画素縦横比: poster と同じく SAR を正方形ピクセルへ焼き込んでから 16:9 の枠へ収め、余白は pad で埋める（4:3 の映像は左右が黒帯になる）
- 列数は固定で 10。枚数が列数の倍数でないときの余りは黒で埋まる。クライアントは列数と 1 枚の大きさだけを知っていれば位置を計算できる（行数を知らなくてよい）
- `internal/worker/seek_tiles.go` と `web/src/lib/seek-tiles.ts` に同じ値が 2 つある。メディア配信は `openapi.yaml` の対象外なので値の伝達経路が無い。**値を変えたら既存のタイルは位置がずれるので再生成が要る**（`rel_path` も行も同じまま中身だけが変わる）
- 配信: streamer の `GET /api/media/recordings/{id}/seek-tiles`（openapi 外。api はファイルを開かない）

#### 生成方式と最後のタイル

最後のタイルは映像の終端から 1 秒手前より後ろに置かない。GOP の長い映像でそれでも取れないときは、最後の 1 枚に限り直前のタイルで埋め、途中のタイルが取れないときは失敗させる。生成は 1 枚ずつ入力シークで取り、読む量は枚数にだけ比例する。

生成は、タイルごとに入力シーク（`-ss` を `-i` の前）で 1 枚ずつ取り、最後に `tile` フィルタで 1 枚に並べる。読む量が枚数にだけ比例し、番組長に比例しない（合成 TS 10 分・60 枚の実測で、全デコード方式の 23.6 秒に対し 2.7 秒）。そのため「先頭 N 分に限る」打ち切りは要らず、上限は枚数だけで決まる。実放送・J4125 での生成時間とサイズは未検証。合成 TS・M3 Max の実装 PR 測定では、10 分・60 枚で生成 1.865 秒、成果物 221.8 KB（3.70 KB/枚）だった。

最後のタイルは映像の終端から 1 秒手前より後ろに置かない。枚数は長さの切り上げで決めるので、長さが 10 秒の倍数をわずかに超える録画では最後のタイルが映像の終端を指す。そこへの入力シークは 1 フレームも出せずに失敗し、毎回同じ枚で落ちて原本が消えなくなる。映像ストリームの長さちょうどでも失敗し、1 秒手前なら成功した（合成 TS 30 秒・ffmpeg 9.0.2）。長さは映像ストリームから取る。format の長さは音声などの最も長いストリームで決まるので使わない。

1 秒の余白で足りるのは MPEG-2 と GOP 1 秒程度の H.264 まで。GOP の長い H.264 は最後のキーフレームより後ろが取れず、x264 GOP 5 秒では終端の 2 秒手前でも失敗した。何秒戻れば足りるかは GOP で決まるので、最後の 1 枚に限り、取れなければ直前のタイルで埋める。x264 GOP 5 秒・40 秒の合成 TS を実 ffmpeg で回すと、埋めなければ最後の 1 枚で落ち、埋めればコミットまで通った。途中のタイルと、1 枚しか無いときの失敗は埋めずに失敗させる。
