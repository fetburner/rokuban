> [storage.md](../storage.md) §1〜5 の一部。索引から辿る

## 1. 方針: S3 SDK を持たない

アプリのコードに S3 SDK やオブジェクトストレージの抽象化を持ち込まない。ローカルディスクとクラウドストレージの差は、OS / CSI / FUSE のレイヤーで吸収する。将来ネイティブ S3 API が必要になっても、DB の相対パス = キーなので、その時点で薄い抽象を後付けする余地は残る。

## 2. FUSE 越し S3 の制約

S3 マウント（k8s-csi-s3 の geesefs/s3fs、AWS Mountpoint 等）では以下を
`storage.media_dir` の原本 root の契約として信頼できない:

- **同一 FS 内の atomic rename がない**（コピー + 削除になる。数十 GB の録画で
  実コピーが走る。Mountpoint は rename 自体非対応）
- **ランダムライトができない/遅い**。特に **ffmpeg は MP4 出力時にヘッダ
  （moov atom）を書き戻すためシークする**ので、S3 マウント上への直接エンコード
  出力は壊れるか激遅になる
- ファイル `fsync`、`Close` のエラー報告、親ディレクトリ `fsync`、ファイル
  ロックの意味論が、原本を公開する根拠として信頼できない

したがって FUSE S3 は原本の ingest 先には使わない。派生物を置く領域としての
利用可能性と、原本 root の契約を混同しない（実機検証の対象範囲もこの境界に
従う）。

## 3. ストレージ契約（4 つのルール）

`storage.media_dir` は、ファイル `fsync`、`Close`、同一 FS 内の atomic rename、
rename 後の親ディレクトリ `fsync` を信頼できる通常の POSIX FS に限る。ローカル
FS / JuiceFS / 条件を満たす NFS は対象内で、FUSE S3 は原本 ingest の対象外である。
以下のルールは、原本 root とローカル `scratch_dir` の組み合わせに適用する:

1. **書き込みは常にシーケンシャル**。初回は一発書き、プロセス再起動後は同じ
   temp の EOF へ追記する。ランダムライトはしない
2. **「作業はローカル、置くのは一回」**: ffmpeg の出力は必ずワーカーのローカルスクラッチ（k8s では emptyDir）に書き、完成したファイルをストレージへストリームコピーして fsync。MP4 のシーク問題と書きかけファイル問題が同時に消える。
	scratch の directory は River の `Timeout` で選ぶ。正の値なら timeout 後の再試行が前の実行と
	重なりうるため試行ごとに一意にし、`-1` なら job ID 固定にして開始時に前回の残骸を消す。
	**ingest は同じ root・同じディレクトリの record 固有 temp へ書く**。temp 名は
	`.rokuban-ingest-{site}-{record_id}` と決め、プロセス再起動後も同じファイルを
	開く。scratch から rename すると `EXDEV` になり、コピーへの劣化を許すため
	確定操作には使わない。
	`.rokuban-ingest-` / `.rokuban-rel-path-lock-` / `.rokuban-encode-` /
	`.rokuban-media-asset-` で始まる
	basename は予約名であり、mirakc の contentPath には使わない。1 つ目は ingest
	temp、2 つ目は旧形式 lock file の予約名、3 つ目は encode の公開前 staging file、
	4 つ目は thumbnail / seek tiles の公開前 staging file（後述）である。rel_path lock
	は media root の `.rokuban-locks/` に置く。
	この directory 名も予約し、`mediapath.Resolve` は DB の `rel_path` として拒否する。
	encode の staged 出力も
	同じディレクトリに置く --- scratch から rename すると `EXDEV` になる。
	canonical path は転送中に触らず、HEAD の長さ照合と、存在する場合の
   `content.sha256` 照合 → temp の `fsync` → `Close`
   → DB transaction 内の original 行 INSERT（rel_path の一意 reservation）→ temp
   を canonical へ atomic rename → 親ディレクトリ `fsync`、の順で進める。
3. **公開点は DB commit**: DB transaction 内の INSERT は一意性を予約するが、
   他セッションから見える公開ではない。rename と親ディレクトリ `fsync` が成功して
   から transaction を commit し、commit が成功した時点で `media_assets` 行と
   canonical file の組を公開する。rename 後に fsync または DB commit が失敗した
   場合は transaction を rollback し、canonical file は orphan として aging 回収
   に委ねる。mirakc record は削除しない。中身の不一致または record の cancel / fail
   が分かった場合だけ temp を消し、それ以外の失敗では次の試行へ残す。
   **この順序を反転させない**: DB commit 後に rename すると、行が指す実体の
   欠落を作る。
   **ingest のコピー完了には fsync と Close のエラー確認まで含める**。転送中の fsync は、4 分の区切りで checkpoint を保存する直前と転送完了後に限り、バッファごとには行わない。区切りで fsync してから checkpoint を書く順序は、電源断でサイズだけ進んだ temp と checkpoint を突き合わせないために必要である。区切りごとの fsync のコストは NFS / JuiceFS で未測定である。S3 系 FUSE は原本 ingest 先から外れているので、途中 fsync で fd に書けなくなる実装は対象外である。Linux では
   遅延した書き込みエラー（ENOSPC / I/O エラー）が `Close` では報告されず `fsync`
   でしか上がらない。rename 後の親ディレクトリ `fsync` は新しい directory entry
   の永続化を確定する。いずれかが失敗したら DB 登録と record 削除をせず再試行する。
   orphan 回収が record 固有 temp を削除するときも同じ `flock` に参加し、ロック取得後に
   inode と mtime を再確認する。canonical orphan の回収は ingest commit と共有する
   `rel_path` lock を rename / unlink から DB commit または orphan 行の整理まで保持する。
   DB の transaction-level advisory lock は一意性と live 行の再確認に使う。filesystem lock は
   DB セッションの切断後も fd が保持するため、古い cleanup が公開済み canonical を消さない。
   実行中の ingest や公開済み canonical は削除せず、次の回収 pass に延期する。
4. **DB には相対パスのみ保存**。ルートは設定で与える。DB にロック・xattr・パーミッション
   の状態は保存しない。temp の同時実行排他は、対象 FS 上の協調的な POSIX `flock` に依存する

### rel_path lock file の寿命

rel_path lock は `.rokuban-locks/<prefix><sha256(rel_path)>.lock` に置く。
canonical file と同じ directory entry ではないため、canonical の rename / unlink で
lock 対象が置き換わらない。通常終了時は per-rel_path lock を保持したまま gate を排他し、
lock file を unlink してから flock と fd を解放する。したがって複数回の ingest / encode /
delete の後も per-rel_path file は残らない。`.gate.lock` は lock directory の調停に使う
固定 file として残る。

取得側は gate を共有して lock file を開き、`LOCK_NB` で試す。busy ならその fd を閉じて
gate を解放してから待ち、再度 path を開く。release と GC は gate を排他するため、unlink と
同時に古い inode を待つ fd は作られない。これが「A が unlink、B が古い inode を取得、C が
新 inode を取得」という二重 lock を防ぐ規則である。canonical orphan の GC も同じ gate を
排他し、per-rel_path flock を取得できた file だけを unlink する。active lock は残して次回に
回す。lock 取得や cleanup に失敗したときはファイル操作を進めず、安全側に倒す。

プロセスが異常終了すると kernel が gate と per-rel_path flock を解放する。unlink 前なら
lock file の directory entry は残るが、次の lock 取得時の GC が gate 排他下で stale file を
回収する。GC 中に active owner がいる file は flock が取れず削除されない。複数プロセスは
同じ media root とこの gate protocol を使う必要がある。POSIX `flock` の前提はこの文書の
ルール 4 と同じで、JuiceFS / NFS 越しの実効性は未検証。

lock directory は mode `0777`、gate / per-rel_path file は `0666` で作り、通常どおり umask
を適用する。異なる uid の worker を同じ media root で動かす場合は、共有 group / ACL が
directory の作成・削除と lock file の read-write を許すことを先に確認する。

旧形式（canonical と同じ directory の `.rokuban-rel-path-lock-*.lock`）は移行時に自動削除
しない。旧 worker は gate に参加しないため、旧形式 file を unlink すると旧プロセスの waiter
が古い inode を握る可能性がある。

新形式も同じ接頭辞を `.rokuban-locks/` の下で使う。そのため接頭辞だけで消す
`find -name '.rokuban-rel-path-lock-*' -delete` は、稼働中の新形式 lock まで消す。
旧形式だけを消すときは `.rokuban-locks/` を除外する。`-delete` は `-prune` と併用できない
（`-depth` を暗黙に有効にする）ので `-exec rm` を使う。

```bash
find "$MEDIA_ROOT" -type d -name .rokuban-locks -prune -o \
  -type f -name '.rokuban-rel-path-lock-*.lock' -exec rm -- {} +
```

旧 worker と新 worker が同じ media root に並走している間は、この削除も実行しない。
k8s の `worker-scaledjobs.yaml` は全 ScaledJob が `rollout.strategy: gradual` なので、更新中は
旧イメージの実行中 Job が完走するまで新しい Job と並走する。この窓では旧 worker が
新 worker の gate を知らないまま同じ rel_path を別の inode で lock しうる。
並走を避けるには、更新の前に media を mount する ScaledJob の新規起動を止め、
`kubectl get jobs` で旧 Job が全て終わったことを確認してから新しいイメージを適用する。
新規起動を止める具体的な操作（KEDA の pause 等）と、その間に積む queue の扱いは未検証。
止められない場合は、この並走窓で同じ rel_path を触る ingest / encode / 削除が二重に
lock を取りうるリスクを受け入れる。旧形式の残置 file は、全旧 worker の停止後に上の
コマンドで消す。

### 派生物の公開（encode）は既存の canonical を上書きする

encode の出力は原本と違って**既にある行の `rel_path` を指す**（プロファイルごとに
1 つ。カット版は世代番号で新しいパスになる）。締切後に River が同じ job ID を
再試行した時、古い Work が ctx cancellation に従わなければ、同じ `(recording, profile)`
の試行が一時的に並走しうる。そのため:

- scratch は**job ID と domain attempt ごと**に分ける。同じ job ID の再試行も
  `<jobID>-<attempt>-<random>` の別ディレクトリを使い、encode は新しい scratch を作る前に同じ job の `<jobID>-*` を消す（古い試行は fencing で公開できない）。各試行は JobRescuer の締切後に
  起動し、開始時に増えた attempt count を fencing token として公開時に照合する。
  古い token の試行は canonical を公開できない。ctx cancellation に従わない古い
  ffmpeg は CPU を使い続けうるため、k8s liveness がプロセスごと停止させる
- staging は **canonical と同じディレクトリの staging file（`.rokuban-encode-`）へ、
  rel_path lock の外でストリームコピー + `fsync`** する。
  公開は lock（filesystem lock → tx → advisory xact lock）の中で、次の順に行う。
  **判定 → rename（サイドカー → 本体）→ 親ディレクトリ `fsync` → `media_assets` の
  Upsert → commit**。
  canonical を `O_TRUNC` で直接開くと、読者が切り詰められた内容を観測しうる。
  孤児回収は同じ lock を非 blocking で取ってから unlink するので、公開と commit の間で
  lock を離すと、commit 前の行と消えた実体が組み合わせになりうる（ルール 3 と同じ理由）
- tx 内では最初に `recording_encode_attempts.attempt_count` がこの試行の token と
  一致することを確かめる。続けて行を読み直し、**(a) `rel_path` が計画時と違う、(b) 既に active で
  （カット版は凍結区間も）この試行と一致する、(c) profile が desired に無い、のどれかなら
  公開しない**。
  (b) が無いと、先発の commit の後に後発が rename で上書きする。後発の commit が
  失敗すると、ファイルは後発の中身で行は先発のサイズになる。
  (a) が無いと、行が先の世代へ進んだ後に古い計画の実行が行を巻き戻す。
  (b) は成功で飛ばす。(a) は「誰かが済ませた」ではなく「自分の計画が古い」を意味する。
  行が active のまま (a) だけが立つ（カット版で区間が違う）ときは、成功で飛ばすと新しい
  チャプター編集が黙って消える。公開せずに River の snooze で戻し、現在の keep で
  計画をやり直す。snooze は attempt を消費せず、失敗通知も出さない。
  行が active でない（ごみ箱など）ときは成功で飛ばす
- (c) は成功で飛ばす。ユーザーが外した版（[retention.md](retention.md) §6「凍結の 3 つ目の例外」）を、
  外す前に積まれた実行中・再試行待ちのジョブが公開して復活させるのを止める。ジョブの cancel では
  塞げない（job lock は ffmpeg の排他ではない）。desired は `FOR SHARE` で読み、版を外す tx の
  policy 行ロックと直列化する
- advisory xact lock が排他するのは ingest commit と孤児回収に対してだけである。
  通常削除（`deleteMediaAsset`）とは filesystem lock でしか排他されない。RWX 越しに
  `flock` が効くかは未検証（ルール 4 と同じ前提）
- 置き忘れた staging file は孤児候補になる。`walkMediaFiles` は `.rokuban-locks/`
  と catalog directory を飛ばし、旧形式 lock filename も候補にしない。
  7 日の mtime 猶予（`defaultOrphanMTimeGrace`）の後に、`deleteOrphanFile` が
  canonical と同じ手順で消す。rel_path lock file は Close または次回 GC で消える。
  拡張子が無いので catalog 無し rescue の対象にはならず、原本へ昇格しない

### thumbnail / seek tiles の公開

thumbnail と seek tiles の ffmpeg 出力先は試行ごとに `MkdirTemp` で作る scratch
directory とする（Timeout が正なので、§3 ルール 2 の選択規則で試行ごとになる）。
同じ recording の River job や、同じ job の試行が重なっても scratch file を共有しない。
完成後は canonical と同じ directory の `.rokuban-media-asset-` staged file にコピーして
file `fsync` する。次に rel_path filesystem lock → transaction → advisory xact lock の順に取る。
transaction 内で active な派生行と原本の生存を再確認し、派生行がまだ無く原本も active なら media asset row を予約する。その後 staged
file を canonical へ rename して親 directory を `fsync` し、最後に commit する。先行 job が
すでに active row を commit していた場合や、ffmpeg 実行中に録画が削除され原本が active でなくなった場合、後続 job は公開を飛ばして成功扱いにする。

staged file は通常の orphan 候補として aging 回収に委ねる。拡張子によらず catalog 無し
rescue から除外する。canonical を `O_TRUNC` で直接開かないため、処理中に配信・削除側が
途中の画像を観測する窓を作らない。

### catalog 世代の公開

catalog export は `catalog/` の新しい世代 directory を `Mkdir` で原子的に予約する。
同じ `ExportedAt` を持つ並列 export は `-02` 以降の別 directory を取得する。世代内では
`catalog.json` を一度書いて file `fsync` し、sha256 とサイズを持つ `manifest.json` を最後に
書いて `fsync` する。manifest が完成世代の判定点で、rename や DB row は使わない。

別 export の prune が書き込み途中の世代を消さないよう、不完全世代は 7 日間保持する。
7 日より古い不完全世代は、より新しい完成世代がある場合だけ prune する。catalog directory
以外へは触れない。

### カット版・サムネイルの置き換え（「置くのは一回」の 2 つの例外）

カット版（`encode.profiles[].cut`）は、チャプターを直した後に同じ出力を作り直す。**同じパスへ上書きしない** --- ルール 2 の「置くのは一回」に反し、生きている行の `rel_path` 部分一意索引とも衝突する。

- パスに世代番号を入れる（`…_{profile}.g{n}.{container}`。1 世代目から付ける）。n は旧 `rel_path` から +1 で導出する
- 新しいファイルを置く → 1 つの tx で `media_assets` の `rel_path` / `size_bytes` を UPDATE し、`media_asset_cuts` を差し替える → commit 後に旧パスを unlink する
- **unlink を commit の前にしない**。commit が失敗すると、生きている行が指すファイルを失う（ルール 3 の「DB commit が公開点」と同じ向き）。unlink に失敗した場合だけ孤児回収に委ねる（旧行はもう存在しないので、`media_assets` に載っていないファイルとして拾われる）
- **行は消さずに UPDATE する**。消して作り直すと `rel_path` の部分一意索引から一瞬外れ、その隙間に別の行が同じパスを取れる

CM 検出の後にサムネイルを選び直す場合も、同じパスへ上書きしない。

- 初回の `thumbnails/{recording_id}.jpg` は世代なしのままにし、差し替えは
  `thumbnails/{recording_id}.g{n}.jpg`（最初の差し替えは `.g1`）へ置く
- 新しい JPEG を置く → 1 つの tx で同じ `media_assets` 行の `rel_path` /
  `size_bytes` を UPDATE し、`media_asset_thumbnail_seeks` を差し替える → commit 後に
  旧パスを unlink する
- **行は消さずに UPDATE する**。行 id と配信対象を保ったまま世代を進める。unlink は
  commit 前にしない。失敗した旧ファイルは孤児回収に委ねる

ポイントはルール 3。DB commit を公開点にしつつ、公開前のファイル操作は強い FS
契約で確定させる。起動時 probe はこの操作列が実行できることだけを確認し、FS の
種類や atomic rename の実装品質をパス文字列から推測しない。

## 4. クラウド側のマウント選択肢

| 選択肢 | 特徴 |
|---|---|
| ローカル FS | file fsync / Close / atomic rename / 親 directory fsync を通常の POSIX 意味論で満たす。第一候補 |
| **JuiceFS**（対象内） | メタデータを DB に、データを S3 に置く FS。atomic rename を含む POSIX 意味論を信頼できる構成で使う。**メタデータストアに PostgreSQL を使う場合は別インスタンスを推奨** |
| **NFS**（対象内） | export は `sync`、client mount は `hard` を推奨。`.nfsXXXX` の silly rename が一時的な orphan 候補に見えても、通常の aging 回収で無害に扱う |
| k8s-csi-s3（geesefs / s3fs）・AWS Mountpoint | 原本 ingest 先には使わない。実機検証の範囲は派生物専用の領域に限る |

**注意**: JuiceFS のメタデータストアに Rokuban と同じ Postgres インスタンスを使うと、DB 障害がストレージ障害に連鎖し「DB が詰まっても仕事は失われない」の前提を崩す。使うなら別インスタンスを明記すること。

## 5. 2 階層: 録画バッファとアーカイブ

「mirakc が直接書くストレージは高速に、録画後の保存先はアーカイブ用途で低速に」という分離は、ingest の設計（[録画エンジン](../recording.md) 参照）が既に実現している。新機能は不要で、「録画後のファイル移動」= ingest そのもの。Rokuban の原本 ingest root は上の強い FS 契約を満たす必要があり、FUSE S3 は派生物専用の別領域でのみ検討する。

### 2 階層の対応関係

| 階層 | 実体 | 要件 | 寿命 |
|---|---|---|---|
| 録画バッファ | mirakc `recording.basedir`（エッジのローカルディスク） | 高速・低レイテンシ（I/O 飽和 = ドロップ直結） | ingest コミット後に record 削除（リングバッファ） |
| アーカイブ | Rokuban のメディアストレージ（ローカル FS / 条件付き NFS / JuiceFS） | 低速可（書き込みはリトライ可能な転送のみ。ただし原本 root の強い FS 契約は必要） | 保持ポリシーに従う |

「mirakc に最終保存先を直接書かせない」根拠はまさにこの要件: 録画はシステム内で唯一のリアルタイム・リトライ不能な操作であり、遅いストレージのストールが放送の欠損に直結する。monolith モードでも basedir を NVMe、メディアストレージを HDD/NAS に置くだけで同じ分離が効く（設定レベルの話でコードは変わらない）。

### 録画バッファのサイジング指針

- **容量の支配項は同時録画数ではなく「ingest が詰まったときの滞留分」**。回線断・クラウド側障害時は未 ingest の record が溜まり続ける。推奨値は「N 日分の全録画を保持できる容量」（地デジ約 7 GB/時で見積り）とし、既定の「未 ingest record 総量メトリクス + エッジディスク残量アラート」と対にする。**ただしそのメトリクスは回線断の滞留を数えない**（`record_sync` は watcher の観測でしか増えないため。[運用](../operations.md) §4「N 日は容量だけでは決まらない」）。同節のとおり **N の上限は容量ではなく `epg.retention_grace` が決める**
- **速度要件は絶対帯域ではなくレイテンシ**。書き込みは 1 録画あたり約 2 MB/s（地デジ 17 Mbps）で、同時 8 本でも 16 MB/s に過ぎない。怖いのは他 I/O との競合によるレイテンシスパイクで、ingest pull のサイト単位 1〜2 本キャップはこのための決定でもある

### アーカイブの速度要件

- 「低速で良い」の正確な意味: **平均スループット >= 1 日の録画総量 / 24 時間**。瞬間的な変動は録画バッファが吸収するので、リアルタイム性は一切要求されない。エンコードの読み出しもバッチなので遅くて良い
- 唯一レイテンシが人間に見えるのは**再生時のシーク**（S3 + FUSE の range read）。原本削除ポリシーと組み合わせた「視聴は H.265 派生物、原本は消すか S3 の奥」という運用が前提なら実用上問題にならない見込み

### 保留: アセット種別ごとのストレージルート分離

派生物（視聴用）だけ速いストレージに置きたくなった場合、originals / derivatives で 2 つのストレージルートを持つ小さな拡張で対応できる。現時点では単一ルートで始め、シークの体感が問題になったら足す（YAGNI）。

### 残量の観測

`storage.media_dir`（アーカイブ）と `storage.scratch_dir`（ローカルスクラッチ）の
容量は、worker が定期的に statfs 相当で観測して `storage_sync` に射影する。
`GET /api/storage` で読める。api ロールはファイルシステムに
依存しない（不変条件 1）。そのため観測は、ファイルシステムを持つ worker の仕事に
限る --- mirakc の recording.basedir（録画バッファ、上記「2 階層」表のエッジ側）は
対象外である。Rokuban 自身が直接読み書きする 2 つのローカルパスだけを見る。

`tuner_sync`（[docs/data.md](../data.md) §6.5）と同じ「使い捨てプロジェクション」の
形を採る。真実は常にファイルシステム側にあり、毎パス全量を作り直せる観測値である。
そのため全行 upsert で常に最新観測だけを保持する（過去の観測を積むログにはしない。
不変条件 9）。`observed_at` の鮮度が「観測ループが止まっている」ことを示す唯一の
手がかりになる（沈黙は保証ではない --- docs/data.md §6.5 の同じ姿勢）。

### rel_path の名前空間

アーカイブ（`media_assets`）は `site` 列を持たず単一だが、原本の `rel_path` は mirakc の contentPath 由来でサイトスコープの名前である。2 サイトが同じ contentPath で録ると、同じ実ファイルを取り合う。DB は `rel_path` の一意索引で片方の commit を落とすが、実ファイルは先に書いた方が上書きされて壊れる。そのため**原本は `sites/{site}/` を前置する**。

- **トップレベルの予約ディレクトリは `catalog/` / `thumbnails/` / `sites/` の 3 つ**。`catalog/` は削除 reconcile の孤児回収と rescue スキャンが SkipDir する予約ディレクトリである。`thumbnails/` はサムネイルの名前空間（§5.1）、`sites/` が site スコープの原本の名前空間である
- **前置の 1 段目を site 名そのもの（`{site}/...`）にせず、固定の `sites/` を挟む**。当初案（site 名を先頭成分にする）は、前置前に ingest 済みの既存行の先頭成分と site 名が偶然一致すると衝突する。例えば `filename_template` が `"tokyo/..."` のような静的接頭辞を書いていて、かつ site 名が `tokyo` だと、新規 ingest の rel_path が既存行と同じになる。すると一意索引が効く前に、実ファイルが上書きされる。site 名の構文 `^[a-z0-9]([_-]?[a-z0-9])*$` は日付ディレクトリ名や `anime` のような静的な語も許すため、理論上だけの懸念ではない。`sites/` を固定の 1 段目に挟むことで、新規 ingest の rel_path は必ず `sites/` から始まる。それ以前の既存行が `sites/` から始まっていない限り、構造的に衝突しない
- **前置するのは ingest（`internal/worker/ingest.go` の `determineRelPath`）であって、contentPath テンプレートではない**。ingest は原本 `rel_path` の唯一の書き手である。そのためここで前置すれば、入力（reconciler が生成する contentPath の形や、ユーザーが書く `filename_template` の内容）に関わらず名前空間が保たれる
- **前置は空の相対パスを通す前に弾く。** contentPath / Content.Path がどちらも空だと前置前の相対パスは `.`（カレントディレクトリ）になる。前置後は `sites/{site}/.` が `Join`/`Clean` で `.` が消えて `sites/{site}` という一見正当なパスになり `mediapath.Resolve` の脱出検知を通ってしまう。すると一時ファイル作成が `{media_dir}/sites/{site}` を通常ファイルとして作ってしまい、以後その site 配下の ingest が全て `MkdirAll` で「not a directory」になる。`determineRelPath` は前置前に相対パスが `.` であることを明示的に検査して弾く
- **`media_dir` 配下に、録画の実体を指すリンクを作らない（symlink / hard link）**。孤児回収の走査（`internal/worker/delete_reconcile.go` の `walkMediaFiles`）は symlink かどうかを見ずに台帳と突き合わせる。そのため置いた symlink は、未知の rel_path として孤児候補になる。[retention.md](retention.md) §7 のエイジング（mtime 猶予 7 日 + 14 日）後に `os.Remove` でリンクだけ黙って消える（`mediapath.Resolve` は字句判定のみで symlink を評価せず止めない）。hard link は regular file と区別できず、同じ実体に live な録画が 2 行並ぶ（`(dev, ino)` による検出はスキャンをまたぐと inode がバックアップ復元で変わるため実装しない）。rescue の走査と `inplace.Register` は symlink だけを弾く
- **ディレクトリへの symlink も作らない。** rescue と孤児回収の走査はどちらも symlink を辿らないため、配下のファイルは孤児候補にすらならず rescue からも見えない（災害復旧で救えない）。symlink エントリ自身は未知の rel_path として渡り、上と同じ理由でエイジング後にリンクだけ消える。リンク先が `media_dir` 内を指す構成では、配下の active 行が実体無しとして誤報され続ける
- **`media_dir` 自身が symlink であることは許す**（`/var/lib/rokuban/media -> /mnt/disk1/media`）。**成り立つのは、走査 2 本が root を `filepath.EvalSymlinks` で解決してから walk しているからである**。走査 2 本とは、rescue の `rescueStorage` と削除 reconcile の `walkMediaFiles` である。**この解決を外すと両方とも黙って壊れる**。`filepath.Walk` / `WalkDir` は root を `Lstat` して `IsDir()` が false ならコールバックを 1 回呼んで終わる。そのため rescue は 0 件のまま「成功」する（災害復旧が最も要る場面だけが壊れる）。削除 reconcile は `seenOnDisk` が `.` の 1 件になるため、全損セーフガード（走査が 0 件なら記録を見送る）も働かない。その結果 `active` な行が全件「実体無し」と誤報される。解決した値は root だけでなく、`catalog/` の除外判定と `rel_path` の基準にも同じものを使う（片方だけ解決すると `filepath.Rel` が `../` を積んだ rel_path を返し、台帳と一致しなくなる）。root を解決することと、配下にリンクを作らないこと（上の 2 つ）は別の話である。片方をもう片方の根拠にしない
- **サムネイルは `thumbnails/{recording_id}.jpg` のまま**（§5.1）。原本の contentPath に依存しないので `sites/` 前置の影響を受けない（構造的に衝突しない）
- **派生物は原本の dir を引き継ぐので自動的に前置される**（`EncodedRelPath`、[retention.md](retention.md) §6 参照）。原本が `sites/tokyo/20240101/....m2ts` なら、派生物は `sites/tokyo/20240101/...._h264.mp4` になる
- **原本と encoded の全行は `sites/{site}/` 前置済みである。** 前置なしの行は worker の起動時検査で拒否する
- **2 種類の予約を分けて理解する。** どちらも `internal/config` にコードがあるが、根拠が違う:
  1. **トップレベルディレクトリ名の予約**（`catalog` / `thumbnails` / `sites` の 3 つ）。これは**今も load-bearing**: `catalog/` は削除 reconcile の孤児回収と rescue スキャンが SkipDir する対象、`thumbnails/` はサムネイルの名前空間、`sites/` は本節の原本の名前空間。この 3 つのいずれかを一般のディレクトリ名として使うと実際に壊れるので、この予約は外せない
  2. **site 名としての `catalog` / `thumbnails` の禁止**（`internal/config.reservedSiteNames`）。導入時の根拠は「`{site}/` を先頭成分にする前提で、site 名がこの 2 つと一致するとトップレベル予約ディレクトリと直接衝突する」だった。だが `sites/` を挟んだことで、site 名は常に `sites/{site}/...` に閉じ込められる。トップレベルの `catalog/` / `thumbnails/` とは構造的に衝突しなくなった。**この禁止を残しているのはパス衝突を防ぐためではない**。緩めても得られる自由度（`catalog` / `thumbnails` を site 名にしたい運用要求は無い）が、緩めるコスト（`internal/config` のバリデーション・テストの変更）に見合わないためである。`sites` 自体を site 名にすることは禁止する必要がない（`sites/sites/...` になるだけで衝突しない）

## 5.1 サムネイル

録画 1 本につき `kind = 'thumbnail'` の media_asset を 1 つ作る
（`UNIQUE (recording_id, kind, profile)`）。

- **投入（レベルトリガー）**: 次の条件を満たす録画だけ、River `thumbnail` キューへ
  unique ジョブ（`recording_id`）を積む。条件は「active な original があり、active な
  thumbnail が無く、かつごみ箱（`recordings.deleted_at IS NOT NULL`）に入っていない」
  ことである。ごみ箱の録画を除外するのは、配信側（`GetThumbnailMediaAssetForServing`）
  が `deleted_at IS NULL` を要求するためである。生成しても誰にも配られず、猶予期間ぶん
  ffmpeg を無駄打ちするだけになる。ingest コミット後のヒント投入と `thumbnail_reconcile` の定期ギャップ
  埋めは同じ条件を使う。定期パスは、delete reconcile が原本の実体無しを確認した
  `missing_media_assets` の原本を既知の恒久失敗として除外する。ファイル復旧後に
  マーカーが消えれば、次の定期パスで再び候補になる。`EnqueueMissingThumbnails`
  による明示的な復旧投入はこの除外をせず、ファイルを戻した直後などに使える。
  命令的チェーン（「ingest 成功 → 必ず thumbnail」）は採らない
- **初回の抽出位置（仮サムネイル）**: `seek = min(duration × 10%, 30s)`。duration は
  原本の ffprobe が読む実ファイル長。取れなければ 0 秒（先頭フレーム）。初回は
  CM 検出を待たず、従来どおり原本から作る。設定キーは設けない
- **CM 検出後の選び直し**: 同じ `min(尺 × 10%, 30s)` ポリシーを、CM 区間を除いた
  keep の合計尺に適用する。原本またはカットしない encoded 版ではその位置を
  `UnmapMs(keep, seek)` で原本時間軸へ写して抽出する。カット版では凍結した
  `media_asset_cuts.keep_ranges` の合計尺で位置を決め、抽出位置を同じ凍結 keep から
  原本時間軸へ戻す。カット版の尺は ffprobe せず凍結 keep の合計を使う
- **再選択入力**: active かつ `missing_media_assets` に無い原本を優先し、次に
  プロファイル名昇順のカットしない encoded、最後に同じ順のカット版を選ぶ。
  未確認の自動 CM 検出も使う。チャプターがない録画は再選択せず、keep が空なら
  再選択を繰り返さない
- **作り直す条件**: 記録済み位置が現在の keep 外にあり、同じ入力選択で計画した
  位置とも異なる場合だけ。`seek_ms` が無い旧サムネイルは、チャプターと使える入力が
  あれば一度選び直す。位置が keep 内なら、望ましい位置との違いだけでは作り直さない
- **再選択 reconcile**: active なサムネイルと CM 検出または所有済みチャプターがある
  録画を recording ID の窓で巡回し、Go の共通判定で対象だけを投入する。ごみ箱は除外し、
  `seek_tiles` や初回生成とは別の再開位置を持つ。CM 検出の保存後に 1 件ヒントを出すが、
  ヒントはベストエフォート。手動編集は次の窓で拾う
- **時間軸**: `ffargs.CutFilterComplex` は入力の最早 `start_time` を 0 とする秒で
  カット境界を適用する。thumbnail の `-ss` も `-i` より前の入力シークであり、同じ
  数値の位置を渡す。chapter ranges と `seek_ms` は `recording_chapter_spans` /
  `media_asset_cuts.keep_ranges` と同じ ms を使う。放送 TS は音声が映像より先に始まる
  ことがあり、章の原点（最初の映像フレーム）と入力の最早 `start_time` の差は未測定。
  そのため、この値が同じ画面を指すことと、原本 / encoded 版間で同じ `seek_ms` が
  揃うことは実録画で確認する
- **画素縦横比**: ffmpeg で入力の SAR を偶数幅の正方形ピクセルへ焼き込んでから
  JPEG 化する。JPEG の SAR を解釈しないブラウザでも anamorphic 映像を歪ませない
- **ストレージ契約**: ffmpeg は `storage.scratch_dir` に JPEG を書き、完成後に
  メディアへストリームコピー + fsync する。初回は `media_assets` と
  `media_asset_thumbnail_seeks` を同じ tx で INSERT し、差し替えでは両方を同じ tx で
  UPDATE する（差し替え規約は §3）
- **相対パス**: 初回は `thumbnails/{recording_id}.jpg`。差し替えは
  `thumbnails/{recording_id}.g{n}.jpg`（n は現行パスの次の世代）。原本の contentPath に
  依存しないので、原本削除後もパスが安定する
- **位置の事実**: `media_asset_thumbnail_seeks` に、その JPEG を切り出した原本時間軸の
  `seek_ms` を 1 行記録する。旧サムネイルで行が無い場合は位置不明として扱う
- **配信**: streamer の `GET /api/media/recordings/{id}/thumbnail`（openapi 外。api はファイルを開かない）

## 5.2 シークプレビュー用タイル

録画 1 本につき `kind = 'seek_tiles'` の media_asset を 1 つ作る（`UNIQUE (recording_id, kind, profile)`）。
中身は 1 枚の JPEG で、`thumbnails/{recording_id}_tiles.jpg` に置く。poster（§5.1）と同じ
`thumbnails/` の名前空間なので、`rel_path` の名前空間の検査は増えない。

**形は固定値である**（設定キーは設けない。poster と同じ流儀）。間隔 10 秒・1 枚 160x90・
10 列・上限 1080 枚（3 時間）。3 時間を超える部分にはタイルが無く、クライアントは
プレビューを出さない。尺に応じて間隔を変える方式は採らない —— ffprobe の長さと
`<video>` の長さのずれが境界でタイル位置を狂わせる。

- **投入**: `thumbnail_reconcile` の定期パスが、poster と同じ窓（`RowLimit` と
  `missing_media_assets` の除外）で desired − observed の差分を埋める。**ingest 直後の
  ヒントは積まない** —— タイルは一覧の表示に関わらないので、poster のような即時性が要らない。
  **priority は poster より下げる**。thumbnail キューは既定で同時実行数 1 なので、同じ
  priority だと既存録画ぶんのタイルが片付くまで新しい録画の poster が一覧に出ない
- **原本が無い録画には作らない。** タイルは原本からしか作らないので、タイル導入前に
  `until_encoded` で原本を消した録画にはタイルが付かない
- **生成方式**: タイルごとに入力シーク（`-ss` を `-i` の前）で 1 枚ずつ取り、最後に
  `tile` フィルタで 1 枚に並べる。**読む量が枚数にだけ比例し、番組長に比例しない**
  （合成 TS 10 分・60 枚の実測で、全デコード方式の 23.6 秒に対し 2.7 秒）。
  そのため「先頭 N 分に限る」打ち切りは要らず、上限は枚数だけで決まる。
  **実放送・J4125 での生成時間とサイズは未検証**。合成 TS・M3 Max の実装 PR 測定では、10 分・60 枚で生成 1.865 秒、成果物 221.8 KB（3.70 KB/枚）だった。
- **失敗したら scratch を捨ててやり直す。** 部分成果はコミットしない（行の存在 = 全部そろっている）。
  poster と違い、長さが取れないとき（0 秒を含む）は 1 枚で続行せずに失敗する。1 枚の格子をコミットすると
  定期パスが二度と作り直さず、`until_encoded` の原本削除の条件まで満たしてしまう。
  版によっては ffmpeg が 0 フレームでも終了コード 0 で終わるかもしれない（未検証。ffmpeg 9.0.2 は非 0）。
  そのため 1 枚ごとに出力の有無を確かめる
- **最後のタイルは映像の終端から 1 秒手前より後ろに置かない。** 枚数は長さの切り上げで決めるので、
  長さが 10 秒の倍数をわずかに超える録画では最後のタイルが映像の終端を指す。そこへの入力シークは
  1 フレームも出せずに失敗し、毎回同じ枚で落ちて原本が消えなくなる。映像ストリームの長さちょうどでも
  失敗し、1 秒手前なら成功した（合成 TS 30 秒・ffmpeg 9.0.2）。長さは format ではなく映像ストリームから取る。
  format の長さは音声などの最も長いストリームで決まるためである
- **1 秒の余白で足りるのは MPEG-2 と GOP 1 秒程度の H.264 まで。** GOP の長い H.264 は最後の
  キーフレームより後ろが取れず、x264 GOP 5 秒では終端の 2 秒手前でも失敗した。何秒戻れば足りるかは
  GOP で決まるので、**最後の 1 枚に限り、取れなければ直前のタイルで埋める**。x264 GOP 5 秒・40 秒の
  合成 TS を実 ffmpeg で回すと、埋めなければ最後の 1 枚で落ち、埋めればコミットまで通った。
  途中のタイルと、1 枚しか無いときの失敗は埋めずに失敗させる
- **画素縦横比**: poster と同じく SAR を正方形ピクセルへ焼き込んでから 16:9 の枠へ収め、
  余白は pad で埋める（4:3 の映像は左右が黒帯になる）
- **列数は固定で 10。** 枚数が列数の倍数でないときの余りは黒で埋まる。クライアントは
  列数と 1 枚の大きさだけを知っていれば位置を計算できる（行数を知らなくてよい）
- **`internal/worker/seek_tiles.go` と `web/src/lib/seek-tiles.ts` に同じ値が 2 つある。**
  メディア配信は `openapi.yaml` の対象外なので値の伝達経路が無い。**値を変えたら
  既存のタイルは位置がずれるので再生成が要る**（`rel_path` も行も同じまま中身だけが変わる）
- **配信**: streamer の `GET /api/media/recordings/{id}/seek-tiles`（openapi 外。api はファイルを開かない）
