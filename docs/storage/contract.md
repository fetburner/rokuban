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

1. **書き込みは常にシーケンシャル・一発書き**。追記もランダムライトもしない
2. **「作業はローカル、置くのは一回」**: ffmpeg の出力は必ずワーカーのローカルスクラッチ（k8s では emptyDir）に書き、完成したファイルをストレージへストリームコピーして fsync。MP4 のシーク問題と書きかけファイル問題が同時に消える。
   **ingest は同じ root・同じディレクトリの試行固有 temp へ書く**。scratch から
   rename すると `EXDEV` になり、コピーへの劣化を許すため確定操作には使わない。
   canonical path は転送中に触らず、HEAD の長さ照合 → temp の `fsync` → `Close`
   → DB transaction 内の original 行 INSERT（rel_path の一意 reservation）→ temp
   を canonical へ atomic rename → 親ディレクトリ `fsync`、の順で進める。
3. **公開点は DB commit**: DB transaction 内の INSERT は一意性を予約するが、
   他セッションから見える公開ではない。rename と親ディレクトリ `fsync` が成功して
   から transaction を commit し、commit が成功した時点で `media_assets` 行と
   canonical file の組を公開する。rename 後に fsync または DB commit が失敗した
   場合は transaction を rollback し、canonical file は orphan として aging 回収
   に委ねる。mirakc record は削除しない。rename 前の失敗では試行固有 temp だけを
   消す。**この順序を反転させない**: DB commit 後に rename すると、行が指す実体の
   欠落を作る。
   **ingest のコピー完了には fsync と Close のエラー確認まで含める**。Linux では
   遅延した書き込みエラー（ENOSPC / I/O エラー）が `Close` では報告されず `fsync`
   でしか上がらない。rename 後の親ディレクトリ `fsync` は新しい directory entry
   の永続化を確定する。いずれかが失敗したら DB 登録と record 削除をせず再試行する。
4. **DB には相対パスのみ保存**。ルートは設定で与える。ロック・xattr・パーミッションに依存しない

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
容量は、worker が定期的に statfs 相当で観測して `storage_sync` に射影し、
`GET /api/storage` で読める。api ロールはファイルシステムに
依存しない（不変条件 1）ので、観測はファイルシステムを持つ worker の仕事に
限る --- mirakc の recording.basedir（録画バッファ、上記「2 階層」表のエッジ側）は
対象外で、Rokuban 自身が直接読み書きする 2 つのローカルパスだけを見る。

`tuner_sync`（[docs/data.md](../data.md) §6.5）と同じ「使い捨てプロジェクション」の
形を採る: 真実は常にファイルシステム側にあり、毎パス全量を作り直せる観測値
なので全行 upsert で常に最新観測だけを保持する（過去の観測を積むログにはしない。
不変条件 9）。`observed_at` の鮮度が「観測ループが止まっている」ことを示す唯一の
手がかりになる（沈黙は保証ではない --- docs/data.md §6.5 の同じ姿勢）。

### rel_path の名前空間

アーカイブ（`media_assets`）は `site` 列を持たず単一だが、原本の `rel_path` は mirakc の contentPath 由来でサイトスコープの名前である。2 サイトが同じ contentPath で録ると同じ実ファイルを取り合う（DB は `rel_path` の一意索引で片方の commit を落とすが、実ファイルは先に書いた方が上書きされて壊れる）ため、**原本は `sites/{site}/` を前置する**。

- **トップレベルの予約ディレクトリは `catalog/` / `thumbnails/` / `sites/` の 3 つ。** `catalog/` は削除 reconcile の孤児回収と rescue スキャンが SkipDir する予約ディレクトリ、`thumbnails/` はサムネイルの名前空間（§5.1）、`sites/` が site スコープの原本の名前空間
- **前置の 1 段目を site 名そのもの（`{site}/...`）にせず、固定の `sites/` を挟む。** 当初案（site 名を先頭成分にする）は、前置前に ingest 済みの既存行の先頭成分と site 名が偶然一致すると衝突する --- 例えば `filename_template` が `"tokyo/..."` のような静的接頭辞を書いていて、かつ site 名が `tokyo` だと、新規 ingest の rel_path が既存行と同じになり、一意索引が効く前に実ファイルが上書きされる（site 名の構文 `^[a-z0-9]([_-]?[a-z0-9])*$` は日付ディレクトリ名や `anime` のような静的な語も許すため、理論上だけの懸念ではない）。`sites/` を固定の 1 段目に挟むことで、新規 ingest の rel_path は必ず `sites/` から始まり、それ以前の既存行が `sites/` から始まっていない限り構造的に衝突しない
- **前置するのは ingest（`internal/worker/ingest.go` の `determineRelPath`）であって、contentPath テンプレートではない。** ingest は原本 `rel_path` の唯一の書き手なので、ここで前置すれば入力（reconciler が生成する contentPath の形や、ユーザーが書く `filename_template` の内容）に関わらず名前空間が保たれる
- **前置は空の相対パスを通す前に弾く。** contentPath / Content.Path がどちらも空だと前置前の相対パスは `.`（カレントディレクトリ）になる。前置後は `sites/{site}/.` が `Join`/`Clean` で `.` が消えて `sites/{site}` という一見正当なパスになり `mediapath.Resolve` の脱出検知を通ってしまう。すると一時ファイル作成が `{media_dir}/sites/{site}` を通常ファイルとして作ってしまい、以後その site 配下の ingest が全て `MkdirAll` で「not a directory」になる。`determineRelPath` は前置前に相対パスが `.` であることを明示的に検査して弾く
- **`media_dir` 配下に、録画の実体を指すリンクを作らない（symlink / hard link）。** 孤児回収の走査（`internal/worker/delete_reconcile.go` の `walkMediaFiles`）は symlink かどうかを見ずに台帳と突き合わせるため、置いた symlink は未知の rel_path として孤児候補になり、[retention.md](retention.md) §7 のエイジング（mtime 猶予 7 日 + 14 日）後に `os.Remove` でリンクだけ黙って消える（`mediapath.Resolve` は字句判定のみで symlink を評価せず止めない）。hard link は regular file と区別できず、同じ実体に live な録画が 2 行並ぶ（`(dev, ino)` による検出はスキャンをまたぐと inode がバックアップ復元で変わるため実装しない）。rescue の走査と `inplace.Register` は symlink だけを弾く
- **ディレクトリへの symlink も作らない。** rescue と孤児回収の走査はどちらも symlink を辿らないため、配下のファイルは孤児候補にすらならず rescue からも見えない（災害復旧で救えない）。symlink エントリ自身は未知の rel_path として渡り、上と同じ理由でエイジング後にリンクだけ消える。リンク先が `media_dir` 内を指す構成では、配下の active 行が実体無しとして誤報され続ける
- **`media_dir` 自身が symlink であることは許す**（`/var/lib/rokuban/media -> /mnt/disk1/media`）。**成り立つのは、走査 2 本 --- rescue（`rescueStorage`）と削除 reconcile（`walkMediaFiles`）--- が root を `filepath.EvalSymlinks` で解決してから walk しているからであって、この解決を外すと両方とも黙って壊れる**: `filepath.Walk` / `WalkDir` は root を `Lstat` して `IsDir()` が false ならコールバックを 1 回呼んで終わるので、rescue は 0 件のまま「成功」し（災害復旧が最も要る場面だけが壊れる）、削除 reconcile は `seenOnDisk` が `.` の 1 件になるため全損セーフガード（走査が 0 件なら記録を見送る）も働かず `active` な行が全件「実体無し」と誤報される。解決した値は root だけでなく `catalog/` の除外判定と `rel_path` の基準にも同じものを使う（片方だけ解決すると `filepath.Rel` が `../` を積んだ rel_path を返し、台帳と一致しなくなる）。root を解決することと、配下にリンクを作らないこと（上の 2 つ）は別の話であって、片方をもう片方の根拠にしない
- **サムネイルは `thumbnails/{recording_id}.jpg` のまま**（§5.1）。原本の contentPath に依存しないので `sites/` 前置の影響を受けない（構造的に衝突しない）
- **派生物は原本の dir を引き継ぐので自動的に前置される**（`EncodedRelPath`、[retention.md](retention.md) §6 参照。原本が `sites/tokyo/20240101/....m2ts` なら派生物は `sites/tokyo/20240101/...._h264.mp4` になる）
- **原本と encoded の全行は `sites/{site}/` 前置済みである。** 前置なしの行は worker の起動時検査で拒否する
- **2 種類の予約を分けて理解する。** どちらも `internal/config` にコードがあるが、根拠が違う:
  1. **トップレベルディレクトリ名の予約**（`catalog` / `thumbnails` / `sites` の 3 つ）。これは**今も load-bearing**: `catalog/` は削除 reconcile の孤児回収と rescue スキャンが SkipDir する対象、`thumbnails/` はサムネイルの名前空間、`sites/` は本節の原本の名前空間。この 3 つのいずれかを一般のディレクトリ名として使うと実際に壊れるので、この予約は外せない
  2. **site 名としての `catalog` / `thumbnails` の禁止**（`internal/config.reservedSiteNames`）。導入時の根拠は「`{site}/` を先頭成分にする前提で、site 名がこの 2 つと一致するとトップレベル予約ディレクトリと直接衝突する」だったが、`sites/` を挟んだことで site 名は常に `sites/{site}/...` に閉じ込められ、トップレベルの `catalog/` / `thumbnails/` とは構造的に衝突しなくなった。**この禁止を残しているのはパス衝突を防ぐためではなく、緩めても得られる自由度（`catalog` / `thumbnails` を site 名にしたい運用要求は無い）が、緩めるコスト（`internal/config` のバリデーション・テストの変更）に見合わないため。** `sites` 自体を site 名にすることは禁止する必要がない（`sites/sites/...` になるだけで衝突しない）

## 5.1 サムネイル

録画 1 本につき `kind = 'thumbnail'` の media_asset を 1 つ作る（`UNIQUE (recording_id, kind, profile)`）。

- **投入（レベルトリガー）**: active な original があり active な thumbnail が無く、
  かつごみ箱（`recordings.deleted_at IS NOT NULL`）に入っていない録画だけ
  River `thumbnail` キューへ unique ジョブ（`recording_id`）を積む。ごみ箱の録画を
  除外するのは、配信側（`GetThumbnailMediaAssetForServing`）が `deleted_at IS NULL`
  を要求するため、生成しても誰にも配られず猶予期間ぶん ffmpeg を無駄打ちするだけ
  だから。ingest コミット後のヒント投入と `thumbnail_reconcile` の定期ギャップ
  埋めは同じ条件を使う。定期パスは、delete reconcile が原本の実体無しを確認した
  `missing_media_assets` の原本を既知の恒久失敗として除外する。ファイル復旧後に
  マーカーが消えれば、次の定期パスで再び候補になる。`EnqueueMissingThumbnails`
  による明示的な復旧投入はこの除外を行わず、ファイルを戻した直後などに使える。
  命令的チェーン（「ingest 成功 → 必ず thumbnail」）は採らない
- **抽出位置（固定ポリシー）**: `seek = min(duration × 10%, 30s)`。duration は
  ffprobe が読む実ファイル長。取れなければ 0 秒（先頭フレーム）。設定キーは設けない
- **画素縦横比**: ffmpeg で入力の SAR を偶数幅の正方形ピクセルへ焼き込んでから
  JPEG 化する。JPEG の SAR を解釈しないブラウザでも anamorphic 映像を歪ませない
- **ストレージ契約**: ffmpeg は `storage.scratch_dir` に JPEG を書き、完成後に
  メディアへストリームコピー + fsync → `media_assets` INSERT（`ON CONFLICT DO NOTHING`）
- **相対パス**: `thumbnails/{recording_id}.jpg`（原本の contentPath に依存しない。
  原本削除後もパスが安定する）
- **配信**: streamer の `GET /api/recordings/{id}/thumbnail`（openapi 外。api はファイルを開かない）

## 経緯と失敗事例

- 原本 `rel_path` への `sites/{site}/` 前置。「site 名を先頭成分にする」当初案が既存 rel_path と衝突しうることはレビューで発見された
- site 名としての `catalog` / `thumbnails` の禁止（`reservedSiteNames`）。`sites` 自体を site 名にすることは禁止していない —— `sites/sites/...` になるだけで衝突しない
- サムネイルは派生物として投入する。ごみ箱の録画は投入対象から除外する
