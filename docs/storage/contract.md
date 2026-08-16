> [storage.md](../storage.md) §1〜5 の一部。索引から辿る

## 1. 方針: S3 SDK を持たない

アプリのコードに S3 SDK やオブジェクトストレージの抽象化を持ち込まない。ローカルディスクとクラウドストレージの差は、OS / CSI / FUSE のレイヤーで吸収する。将来ネイティブ S3 API が必要になっても、DB の相対パス = キーなので、その時点で薄い抽象を後付けする余地は残る。

## 2. FUSE 越し S3 の制約

S3 マウント（k8s-csi-s3 の geesefs/s3fs、AWS Mountpoint 等）では以下が成立しない:

- **アトミック rename がない**（コピー + 削除になる。数十 GB の録画で実コピーが走る。Mountpoint は rename 自体非対応）
- **ランダムライトができない/遅い**。特に **ffmpeg は MP4 出力時にヘッダ（moov atom）を書き戻すためシークする**ので、S3 マウント上への直接エンコード出力は壊れるか激遅になる
- fsync・close 時のエラー報告・ファイルロックの意味論も怪しい

これらの制約を踏まえ、アプリのファイル操作を安全なサブセットに制約する（次節のストレージ契約）。

## 3. ストレージ契約（4 つのルール）

以下を守る限りローカルディスクでも S3 マウントでも同じコードが動く:

1. **書き込みは常にシーケンシャル・一発書き**。追記もランダムライトもしない
2. **「作業はローカル、置くのは一回」**: ffmpeg の出力は必ずワーカーのローカルスクラッチ（k8s では emptyDir）に書き、完成したファイルをストレージへストリームコピーして fsync。MP4 のシーク問題と書きかけファイル問題が同時に消える。
   **例外: mirakc からの record pull（ingest）は宛先へ直接シーケンシャルに書く**（スクラッチ経由にしない）。根拠は 3 つ:
   (1) このルールの主動機は ffmpeg の moov atom 書き戻し（シーク）であり、TS の 1 パス・追記なしのシーケンシャル書きにはそもそも当たらない、
   (2) スクラッチ経由にすると最大録画サイズぶんのスクラッチ容量を要求し（k8s の emptyDir サイジングが変わる）、システム内で最大のバイト流に 2 度目のローカル I/O パスを作る（[recording/ingest.md](../recording/ingest.md) §5.1 が明示的に却下した設計）、
   (3) このルールが本来消すはずだった書きかけ残骸は、ルール 3（公開 = DB 登録）と cleanup の孤児回収が既に扱う。
   したがって **転送中は宛先パスに書きかけのバイトが存在しうる。これは契約の許容範囲**（読み手は DB に載っているパスしか見ない）。その代わり「同じ `rel_path` へ 2 本の ingest が同時に書かない」ことは別に保証する必要があり、ingest は `rel_path` の Postgres advisory lock で確保する（[recording/ingest.md](../recording/ingest.md) §5.3）。ffmpeg（encode / thumbnail）はこの例外の対象外で、ルール 2 のまま
3. **「公開済み」の定義は rename ではなく DB 登録**: コピー完了 → `media_assets` 行の登録、が公開の定義。読み手は DB に載っているパスしか見ない。rename のアトミック性に依存しないので S3 マウントの弱い意味論が許容範囲に入る。書きかけ・コピー失敗の残骸は cleanup ジョブが「DB に対応行のないファイル」として掃除する。
   **この順序を反転させない**: 「行を先に登録し、rename で宛先を作る」案は、rename 非対応バックエンド（Mountpoint）で宛先が恒久的に作られず、行が指す唯一の実体が一時ファイルのまま残り、それを孤児回収がいずれ消してしまう（`active` 行の実体欠落を検出する経路が無い）。コピー完了 → 登録、の順序は守る
4. **DB には相対パスのみ保存**。ルートは設定で与える。ロック・xattr・パーミッションに依存しない

ポイントはルール 3。Postgres を真実の座に置く設計（[データ層](../data.md) 参照）なので、「ストレージは信頼性の低いただの置き場、整合性は DB とジョブの冪等性で担保」と割り切れる。

## 4. クラウド側のマウント選択肢

| 選択肢 | 特徴 |
|---|---|
| **JuiceFS**（第一候補） | メタデータを DB に、データを S3 に置く FUSE FS。本物のアトミック rename を含むまともな POSIX 意味論。**メタデータストアに PostgreSQL を使える**ため「ステートフル基盤は Postgres と S3 だけ」という構成に綺麗にはまる |
| k8s-csi-s3（geesefs） | シーケンシャル書き込みは高速で契約とは相性が良い。コミュニティドライバ |
| RWX PVC（NFS 等） | S3 にこだわらないならこれでも契約は満たせる |

**注意**: JuiceFS のメタデータストアに Rokuban と同じ Postgres インスタンスを使うと、DB 障害がストレージ障害に連鎖し「DB が詰まっても仕事は失われない」の前提を崩す。使うなら別インスタンスを明記すること。

## 5. 2 階層: 録画バッファとアーカイブ

「mirakc が直接書くストレージは高速に、録画後の保存先はアーカイブ用途（S3 可）で低速に」という分離は、ingest の設計（[録画エンジン](../recording.md) 参照）が既に実現している。新機能は不要で、「録画後のファイル移動」= ingest そのもの。

### 2 階層の対応関係

| 階層 | 実体 | 要件 | 寿命 |
|---|---|---|---|
| 録画バッファ | mirakc `recording.basedir`（エッジのローカルディスク） | 高速・低レイテンシ（I/O 飽和 = ドロップ直結） | ingest コミット後に record 削除（リングバッファ） |
| アーカイブ | Rokuban のメディアストレージ（ローカル FS / NAS / CSI の S3） | 低速可（書き込みはリトライ可能な転送のみ） | 保持ポリシーに従う |

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
`GET /api/storage` で読める（issue #238 M7-5）。api ロールはファイルシステムに
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
- **サムネイルは `thumbnails/{recording_id}.jpg` のまま**（§5.1）。原本の contentPath に依存しないので `sites/` 前置の影響を受けない（構造的に衝突しない）
- **派生物は原本の dir を引き継ぐので自動的に前置される**（`EncodedRelPath`、[retention.md](retention.md) §6 参照。原本が `sites/tokyo/20240101/....m2ts` なら派生物は `sites/tokyo/20240101/...._h264.mp4` になる）
- **前置前に ingest 済みの既存行は移行しない。** 新規 ingest 分だけ `sites/{site}/` が付き、ディスク上は前置あり/なしが混在する。`rel_path` をパースする読者がいない（rescue の資産種別判定は拡張子しか見ない）ため混在は無害 --- ただし、これは「既存行が `sites/` から始まっていない」という上記の前提の上に成り立つ断定であり、`sites/` 自体を先頭成分に使っていた既存の `filename_template` があれば話は別（`sites/` は `catalog/` / `thumbnails/` と同じ「新設の予約ディレクトリが過去の運用と衝突しないことを祈る」という一般的なトレードオフを負っている）
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
  だから。ingest コミット後のヒント投入と、ギャップ埋め
  （`ListRecordingIDsMissingThumbnail`）の両方で同じ条件を使う。
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

- 原本 `rel_path` への `sites/{site}/` 前置は issue #186（M4-14）。「site 名を先頭成分にする」当初案が既存 rel_path と衝突しうることは PR #196 のレビューで発見された
- site 名としての `catalog` / `thumbnails` の禁止（`reservedSiteNames`）は M4-11 で導入。`sites` 自体を site 名にできる理由づけは issue #186 のコメント参照
- サムネイルの導入は M3-4。ごみ箱の録画を投入対象から除外する判断は issue #109
