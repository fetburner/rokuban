# メディアストレージ

録画ファイル・エンコード成果物・サムネイルの保存先は、アプリからは**ただのディレクトリ**として扱う。バックエンドに S3 SDK を組み込まず、`BlobStore` のような抽象化レイヤーも作らない（`os.File` とディレクトリ規約だけ）。

- **ミニ PC / 自宅サーバー**: ローカルディスクのディレクトリをそのまま使う。追加設定なし
- **クラウド / k8s**: CSI ドライバで S3 をマウントし、アプリには透過的に S3 保存させる

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
2. **「作業はローカル、置くのは一回」**: ffmpeg の出力や mirakc からの record pull は必ずワーカーのローカルスクラッチ（k8s では emptyDir）に書き、完成したファイルをストレージへストリームコピーして fsync。MP4 のシーク問題と書きかけファイル問題が同時に消える
3. **「公開済み」の定義は rename ではなく DB 登録**: コピー完了 → `media_assets` 行の登録、が公開の定義。読み手は DB に載っているパスしか見ない。rename のアトミック性に依存しないので S3 マウントの弱い意味論が許容範囲に入る。書きかけ・コピー失敗の残骸は cleanup ジョブが「DB に対応行のないファイル」として掃除する
4. **DB には相対パスのみ保存**。ルートは設定で与える。ロック・xattr・パーミッションに依存しない

ポイントはルール 3。Postgres を真実の座に置く設計（[データ層](data.md) 参照）なので、「ストレージは信頼性の低いただの置き場、整合性は DB とジョブの冪等性で担保」と割り切れる。

## 4. クラウド側のマウント選択肢

| 選択肢 | 特徴 |
|---|---|
| **JuiceFS**（第一候補） | メタデータを DB に、データを S3 に置く FUSE FS。本物のアトミック rename を含むまともな POSIX 意味論。**メタデータストアに PostgreSQL を使える**ため「ステートフル基盤は Postgres と S3 だけ」という構成に綺麗にはまる |
| k8s-csi-s3（geesefs） | シーケンシャル書き込みは高速で契約とは相性が良い。コミュニティドライバ |
| RWX PVC（NFS 等） | S3 にこだわらないならこれでも契約は満たせる |

**注意**: JuiceFS のメタデータストアに Rokuban と同じ Postgres インスタンスを使うと、DB 障害がストレージ障害に連鎖し「DB が詰まっても仕事は失われない」の前提を崩す。使うなら別インスタンスを明記すること。

## 5. 2 階層: 録画バッファとアーカイブ

「mirakc が直接書くストレージは高速に、録画後の保存先はアーカイブ用途（S3 可）で低速に」という分離は、ingest の設計（[録画エンジン](recording.md) 参照）が既に実現している。新機能は不要で、「録画後のファイル移動」= ingest そのもの。

### 2 階層の対応関係

| 階層 | 実体 | 要件 | 寿命 |
|---|---|---|---|
| 録画バッファ | mirakc `recording.basedir`（エッジのローカルディスク） | 高速・低レイテンシ（I/O 飽和 = ドロップ直結） | ingest コミット後に record 削除（リングバッファ） |
| アーカイブ | Rokuban のメディアストレージ（ローカル FS / NAS / CSI の S3） | 低速可（書き込みはリトライ可能な転送のみ） | 保持ポリシーに従う |

「mirakc に最終保存先を直接書かせない」根拠はまさにこの要件: 録画はシステム内で唯一のリアルタイム・リトライ不能な操作であり、遅いストレージのストールが放送の欠損に直結する。monolith モードでも basedir を NVMe、メディアストレージを HDD/NAS に置くだけで同じ分離が効く（設定レベルの話でコードは変わらない）。

### 録画バッファのサイジング指針

- **容量の支配項は同時録画数ではなく「ingest が詰まったときの滞留分」**。回線断・クラウド側障害時は未 ingest の record が溜まり続ける。推奨値は「N 日分の全録画を保持できる容量」（地デジ約 7 GB/時で見積り）とし、既定の「未 ingest record 総量メトリクス + エッジディスク残量アラート」と対にする
- **速度要件は絶対帯域ではなくレイテンシ**。書き込みは 1 録画あたり約 2 MB/s（地デジ 17 Mbps）で、同時 8 本でも 16 MB/s に過ぎない。怖いのは他 I/O との競合によるレイテンシスパイクで、ingest pull のサイト単位 1〜2 本キャップはこのための決定でもある

### アーカイブの速度要件

- 「低速で良い」の正確な意味: **平均スループット >= 1 日の録画総量 / 24 時間**。瞬間的な変動は録画バッファが吸収するので、リアルタイム性は一切要求されない。エンコードの読み出しもバッチなので遅くて良い
- 唯一レイテンシが人間に見えるのは**再生時のシーク**（S3 + FUSE の range read）。原本削除ポリシーと組み合わせた「視聴は H.265 派生物、原本は消すか S3 の奥」という運用が前提なら実用上問題にならない見込み

### 保留: アセット種別ごとのストレージルート分離

派生物（視聴用）だけ速いストレージに置きたくなった場合、originals / derivatives で 2 つのストレージルートを持つ小さな拡張で対応できる。現時点では単一ルートで始め、シークの体感が問題になったら足す（YAGNI）。

## 5.1 サムネイル（M3-4）

録画 1 本につき `kind = 'thumbnail'` の media_asset を 1 つ作る（`UNIQUE (recording_id, kind, profile)`）。

- **投入（レベルトリガー）**: active な original があり active な thumbnail が無く、
  かつごみ箱（`recordings.deleted_at IS NOT NULL`）に入っていない録画だけ
  River `thumbnail` キューへ unique ジョブ（`recording_id`）を積む。ごみ箱の録画を
  除外するのは、配信側（`GetThumbnailMediaAssetForServing`）が `deleted_at IS NULL`
  を要求するため、生成しても誰にも配られず猶予期間ぶん ffmpeg を無駄打ちするだけ
  だから（issue #109）。ingest コミット後のヒント投入と、ギャップ埋め
  （`ListRecordingIDsMissingThumbnail`）の両方で同じ条件を使う。
  命令的チェーン（「ingest 成功 → 必ず thumbnail」）は採らない
- **抽出位置（固定ポリシー）**: `seek = min(duration × 10%, 30s)`。duration は
  ffprobe が読む実ファイル長。取れなければ 0 秒（先頭フレーム）。設定キーは設けない
- **ストレージ契約**: ffmpeg は `storage.scratch_dir` に JPEG を書き、完成後に
  メディアへストリームコピー + fsync → `media_assets` INSERT（`ON CONFLICT DO NOTHING`）
- **相対パス**: `thumbnails/{recording_id}.jpg`（原本の contentPath に依存しない。
  原本削除後もパスが安定する）
- **配信**: streamer の `GET /api/recordings/{id}/thumbnail`（openapi 外。api はファイルを開かない）

## 6. 原本 TS の保持ポリシー

生の放送データは MPEG-2（地デジで約 6〜7 GB/時、BS はさらに大きい）でストレージ効率が悪く、エンコード完了後に原本を削除したいという要件がある。EPGStation の「エンコード後に元ファイル削除」に相当するが、命令的（エンコード完了時に削除を実行）ではなく**宣言的な保持ポリシー + レベルトリガー reconcile** で実現する。

### 設計

**ルール（または個別予約）が保持ポリシーを持つ**: `keepOriginal: always / until_encoded`。実効値（ルールの base + 予約単位の overrides）は `recordings.keep_original` / `recordings.encode_profiles` へスナップショットされ、「この録画の望ましい最終状態は『派生物のみ、原本なし』」という desired state になる（M3-14、issue #103）。

**凍結する瞬間は ingest が原本 media_asset をコミットする tx の中**（`internal/worker/ingest.go` の `resolveAndSnapshotEncodePolicy`）であって、予約確定時でも録画開始時でもない。再導出（reservations 経由で毎回引き直す）は選べない —— 導出元（`reservations` / `program_overrides` / `program_intents`）は放送終了 + 猶予後に GC される寿命の短い表だが、`recordings` は永続資産（CLAUDE.md 不変条件 12「表は行の寿命で割る」）。導出に依存させると、番組が EPG から消えて GC された時点で desired が空になり、エンコード未完了の録画で原本削除が止まる／再エンコードが投入できなくなる。凍結した 2 列は「この録画の望ましい最終状態」であり、`recordings` 行と同時に生まれて同時に死ぬので不変条件 12 には反しない。ただし凍結する以上、**ingest 完了より後の override 変更はその録画には反映されない**という境界が生まれる（[予約モデル](recording/reservation-model.md) §4.5）。

**予約をどのキーで引くか（issue #149）**: `resolveAndSnapshotEncodePolicy` は予約を `recordings.reservation_id`（bigint FK、`ON DELETE SET NULL`）ではなく、放送イベントキー `(site, network_id, service_id, event_id)` で引く。`reservations.id` は ruler の導出削除・再実体化（EPG フリッカー、ルール編集、dedup）で変わりうる不安定な値（CLAUDE.md 不変条件 9「identity」、#53 / #98 / #99 と同じ族）で、録画開始から ingest 完了までの窓（番組の尺ぶん、数時間）でこれが起きると FK が NULL に落ち、旧実装は「予約が無い」と誤認して encode policy を凍結し損なっていた（ログにも出ない）。放送イベントキーは `recordings` が録画開始時から凍結して持つ列（導出器が作るキーではない）なので、予約の再実体化を跨いでも変わらない。

具体的には `program_snapshots` で `(network_id, service_id, event_id)` → `program_id` を引き、`reservations` を `program_id` で結合する（`GetReservationEncodePolicyByEvent`、`internal/db/queries/recording_policy.sql`）。`program_snapshots` は放送後 `epg.retention_grace`（既定 24h）で GC される寿命の短い表（`docs/schema.md` §3「射影にある間は更新、消えたら凍結」、`docs/schema/reservations.md` 参照）だが、ingest は録画終了直後 --- GC の猶予期間より十分前 --- に走るため、この前提は通常経路では効かない。

`recordings.source`（`DeriveRecordingSource`、`internal/db/recording_source.go`）はこの JOIN 失敗の異常度を判定する軸として使える場面が半分しかない。`source = 'rule'` は「作成時点で予約があり、かつ `program_intents.action = 'record'` の行が無かった」を意味するので、JOIN が失敗するのは常に異常系（GC が想定より早く走った、または予約が恒久的に削除された）で `slog.Warn` に識別子（site/network_id/service_id/event_id）と recording_id を残す。一方 `source = 'manual'` は「intent が `action = 'record'` だった（予約の有無に関わらず）」と「そもそも予約が最初から無かった」（手動起動、日常的）という区別できない 2 つの経路を 1 つの値に潰しているため、JOIN 失敗が異常かどうか判定できない。前者（ユーザーが手動予約して encodeProfiles を指定した録画）で解決に失敗すると静かにエンコードされない状態がそのまま残ってしまうので、`source = 'manual'` でも黙って return せず `slog.Info` に同じ識別子を残す。

**retention reconcile ループ**（worker の cleanup 系ジョブ）が定期的に走り、次を**すべて**満たす原本アセットを削除する:

1. ポリシーが `until_encoded`
2. desired な派生物（ルールで指定した全エンコードプロファイル + サムネイル）がすべて `media_assets` にコミット済み
3. 原本を入力とする実行中・再試行中のジョブがない

命令的なジョブチェーン（最後のエンコードジョブが削除ジョブを投入）だと、複数プロファイル時の「全部終わったら」の fan-in・途中失敗・再実行で壊れやすい。レベルトリガーなら「観測された派生物の集合 >= 望ましい集合」を毎回評価するだけで、どこで落ちても収束する。

### 安全性

- **放送データが 0 コピーになる瞬間は構造的に存在しない**。エッジの record 削除は ingest コミット後（[録画エンジン](recording.md) 参照）、原本削除はエンコード検証後。常に 1 コピー以上ある
- **「唯一のコピーを消す」パスがない**。エンコードが恒久的に失敗すれば条件 2 が満たされず原本は自然に保持され続ける（+ アラート対象）
- **条件 2 の「全プロファイル完備」は `encode_profiles` が空でないことも要求する**。API はエンコードプロファイル未指定のルールで `until_encoded` を選択不可にしているが（下記「UI / 運用」）、それを回避して `until_encoded` かつ `encode_profiles = '{}'` の組が recordings に焼かれた場合、「全称量化された条件が空集合に対して自明に真になる」ため対策なしでは即座に原本が消える（issue #103 の「罠」）。削除 reconcile のクエリ自身に `cardinality(encode_profiles) > 0` を要求させ、入力側の検証が抜けても構造的に安全側に倒す
- **削除プロトコルも冪等**: アセット行を deleting にマーク → unlink → deleted にマーク。どこで落ちても reconcile が拾い直し、残骸は孤児クリーンアップが回収
- **メタデータは tombstone として残す**。ドロップスキャン結果・元サイズ・録画品質は原本削除後も UI で見られる（「ドロップがあったから再放送を待つ」判断は削除後にこそ必要）

### UI / 運用

- 原本削除後は**再エンコード不可**になるため、ルール設定で明示。デフォルトは安全側の `keepOriginal: always` とし、ストレージ効率はユーザーの opt-in
- エンコードプロファイル未指定のルールでは `until_encoded` を選択不可（原本が唯一の視聴可能物）
- 視聴は常に派生物側（MPEG-2 TS はブラウザ直接再生に不向き）なので、原本削除で失うのは再エンコードの自由度だけ。H.265 で 1/4〜1/10 になるため、これが実質のストレージ戦略になる

### 凍結の例外: 事後追加（issue #133）

`recordings.encode_profiles` は ingest 完了時に一度だけ焼き込まれる凍結値だが、**ユーザー起点の追加方向の書き換えだけは凍結の例外として認める**。予約が無い録画（mirakc に直接起こされた手動録画等）は `encode_profiles = '{}'` のまま永久に凍結されエンコードを依頼する手段が無かった問題と、録画完了後に「もう1つプロファイルを足したい」という要求に応える。

- **範囲は追加のみ**。`POST /api/recordings/{id}/encode-profiles`（`internal/api/recordings.go` の `AddRecordingEncodeProfiles`）は `AppendRecordingEncodeProfiles`（`internal/db/queries/recordings.sql`）で union + dedup にしか書けない。全置換にすると、ユーザーが誤って既存のプロファイル指定を消す事故につながるため、その経路自体を用意しない
- **原本削除済みなら不可**。`GetActiveOriginalMediaAsset` が `ErrNoRows` の録画（`until_encoded` でエンコード完了後に原本が削除された等）には 409 を返す。`EnqueueMissingEncodes` はこのケースで黙って no-op になる（原本が無ければ何もしない設計。上記「安全性」参照）ため、サイレントな失敗にしないよう api 層で明示的に検査する
- **実行経路**: api がトランザクション内で `encode_profiles` を更新し、同一トランザクションで `EncodeEnqueueHintArgs`（ヒントジョブ）を投入する。実際の `EnqueueMissingEncodes` 呼び出し（desired − observed の差分を埋める encode ジョブの投入）は worker ロール側の `EncodeEnqueueHintWorker` が行う（既存の hint job パターン。`rules.go` の `insertRulerPassHint` と同型）。詳細は `internal/worker/encode.go` の `EncodeEnqueueHintArgs` の doc コメント参照
- この例外を経ても「ingest 完了時点で確定した最終状態」という #103 の設計そのものは変わらない —— 削除・変更方向の書き換えは今も無い

## 7. 削除エンジン

### 物理削除は 1 本の reconcile ループに統一

物理 unlink に至る経路を 3 ソース → 1 つの削除 reconcile に揃える:

| ソース | 猶予 | 意図 |
|---|---|---|
| 手動削除（ごみ箱） | `deleted_at` + 30 日（設定可） | 人為ミスへの備え |
| 原本の保持ポリシー（`until_encoded`） | なし（派生物完備が条件） | 設計されたポリシー削除。**ごみ箱は経由しない**（原本はサイズが支配的で、経由させるとストレージ節約が猶予期間ぶん遅延する。安全条件は派生物完備で既に担保） |
| 孤児ファイル | mtime 猶予 + エイジング | DB 喪失・残骸への防御 |

**一括削除サーキットブレーカーはループ全体に 1 つ**: ソースを問わず 1 パスの物理削除が閾値（件数 / ライブラリ比率 / 総バイト数、例: 5% or 100 GB）を超えたら停止してアラート。

### 不変条件の修正

従来の「DB にないファイル = 孤児 = 削除対象」は、暗黙に「DB は常にファイルより新しい」を仮定しており、**DB リストアはこの仮定を壊す唯一の正規操作**。契約を「孤児に見えることは削除の必要条件であって十分条件ではない」に修正する。

### ごみ箱 = 論理削除。「場所」ではなく「状態」

- UI の削除は `deleted_at` を立てるだけ（録画単位。原本 + 派生物 + サムネイルのアセットグループごと）。ファイルには触れない
- ごみ箱ビュー = `deleted_at IS NOT NULL` の一覧。**復元は `deleted_at` を消すだけ**（ファイル操作ゼロ・即時）。「今すぐ完全削除」も個別/一括で可能
- 物理的な隔離ディレクトリへの移動はしない（FUSE-S3 では数十 GB の rename がコピーになる。論理削除なら I/O ゼロで同じ猶予が得られる）
- 物理削除後も tombstone は残る → ドロップ統計・録画履歴は消えず、**ごみ箱を空にしても再放送重複排除は壊れない**
- **`recordings.purged_at`（issue #135）は「完全削除が完了した」不可逆な事実を持つ列。** 削除 reconcile がパス末尾で、ごみ箱条件を満たしかつ物理削除が終わっていない `media_assets` が 1 行も残っていない録画に一度だけ立てる。tombstone は上の行のとおり残り続けるが、ごみ箱ビュー（`ListTrashRecordings`）は `purged_at IS NULL` も要求するので、purge が完了した録画はごみ箱一覧には出ない。「`media_assets` に未削除行が 0」を毎パス導出する案は採らない —— アセットを一度も持ったことがない録画ではこの条件が purge 前から真であり、「消した」と「元から無い」を区別できないため（CLAUDE.md 不変条件 9）
- 将来オプション: ごみ箱サイズの UI 表示 + 空き容量逼迫時に猶予期間前でも古い順に purge する容量トリガー。初期実装は期間ベースのみ
- **復元と物理削除の競合（issue #105）**: `media_assets.state = 'deleting'` は unlink 待ちの間しか続かない一時状態で、復元は `recordings.deleted_at` しか消さないため、unlink が失敗して `deleting` のまま次パスに持ち越されると「復元したのに次パスで消える」窓ができうる。前パスの `deleting` 行を拾い直す経路（`ListMediaAssetsPendingDelete`）は無条件に unlink へ進むのではなく、trash 猶予超過 / until_encoded 派生物完備の判定を**適用の瞬間に再評価**する。該当しなくなった行は `ListUnqualifiedDeletingAssets` で候補として挙げ、`resolveUnqualifiedDeletingAsset` がファイルの現存を `stat` で確認したうえで、まだ存在すれば `active` に戻し、既に無ければ（unlink 成功後 `MarkMediaAssetDeleted` のコミット前にプロセスが落ちていた場合）`active` には戻さず `deleted` を確定する——ここで無条件に `active` へ戻すと、復元 API 側で `deleting → active` を即時に書き換える方式（案 B）を採らなかった理由そのもの、「`active` なのにファイルが無い行」を revert 経路自身が作ってしまうため

### 孤児回収の 3 重の安全弁

1. **mtime 猶予**: mtime が 7 日以内のファイルは孤児候補にすらしない（正常系の録画 → ingest → エンコードは数時間で完結）。バックアップが 1 日古い程度のリストアはこれだけで守られる
2. **孤児エイジング**: 孤児候補は `orphan_files` テーブルに first_seen を記録し、14 日連続で孤児であり続けたものだけ削除。**観測記録が DB 側にあるため、DB リストアで時計もリセットされ、削除までの窓が自動的に開き直す**
3. **サーキットブレーカー**（上記）: DB 全損直後は全ファイルが孤児に見えるため確実に発動する

「リストア後は cleanup を止めておく」という人間の記憶に頼る運用が不要になる。

### 既存不変条件の再確認

- **「放送データのコピーが常に 1 つ以上」は DB 喪失時も維持される**: エッジ record の削除は ingest の DB コミット後 → コミット直後に DB を失ってもファイルはアーカイブに存在し、安全弁が守り、rescue が再登録する
- cleanup は mirakc の basedir に絶対に触らない（エッジ側削除は ingest の検証済み削除のみ）

## 8. catalog エクスポートと rescue（災害復旧）

### 保護対象の仕分け

失うと痛いデータを仕分けすると、EPG プロジェクションは mirakc から再構築可能、ジョブキューは一時的で、**保護対象は「ルール・録画履歴・media_assets・ドロップ統計・tombstone・手動オーバーライド」のみ（数 MB）**。

### catalog エクスポート

- worker の定期ジョブが、このコアデータを JSON で**メディアストレージ自身の `catalog/` 配下に書き出す**（日次 + 世代保持）。メディアが生き残る障害では catalog も一緒に生き残る
- pg_dump に依存しない（distroless イメージに postgres クライアント不要）アプリレベルのエクスポートで、後述 rescue の入力形式を兼ねる
- フル忠実度が欲しい場合の日次 pg_dump 構成例はドキュメントに記載（推奨・非必須）

### rescue --- ストレージスキャンからの再構築

`rokuban rescue`: ストレージを走査し、

- `catalog/` があれば照合してフルメタデータ（番組情報・ドロップ統計・保持ポリシー）ごと復元
- catalog が無ければ TS / M2TS を `original`、MP4 / MKV / WebM を `encoded`
  （`profile = rescue-<拡張子>`）として、現在位置のまま登録する。タイトルと時刻は
  ファイル名 / mtime、番組・サービス情報は「metadata unavailable」と明示した素の録画になる
- `catalog/` 自身、未知拡張子、symlink は走査対象にしない。ファイル本体はコピーも変更もしない
- 同じ相対パスは安定した合成番組 identity へ写し、再実行しても録画・asset が増殖しない

登録トランザクションは `internal/inplace` に置き、`rokuban import epgstation` も同じ
in-place 登録機構を使う。DB 行のコミットが公開であるというストレージ契約は通常 ingest と同じ。

### Postgres 運用

世帯スケールでは catalog（+ 任意で日次 pg_dump）で十分。WAL アーカイビングは過剰。
