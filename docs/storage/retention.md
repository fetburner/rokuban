> [storage.md](../storage.md) §6 §7 の一部。索引から辿る

## 6. 原本 TS の保持ポリシー

生の放送データは MPEG-2（地デジで約 6〜7 GB/時、BS はさらに大きい）でストレージ効率が悪く、エンコード完了後に原本を削除したいという要件がある。EPGStation の「エンコード後に元ファイル削除」に相当するが、命令的（エンコード完了時に削除を実行）ではなく**宣言的な保持ポリシー + レベルトリガー reconcile** で実現する。

### 設計

**ルール（または個別予約）が保持ポリシーを持つ**: `keepOriginal: always / until_encoded`。実効値（ルールの base + 予約単位の overrides）は `recording_encode_policy.keep_original` / `recording_encode_policy.encode_profiles` へスナップショットされ、「この録画の望ましい最終状態は『派生物のみ、原本なし』」という desired state になる。`recording_encode_policy` は `recordings` を `recording_id` で指す衛星表で、行の存在そのものが「凍結済み」を意味する（[スキーマ](../schema.md) §5 参照）。

**凍結する瞬間は ingest が原本 media_asset をコミットする tx の中**（`internal/worker/ingest.go` の `resolveAndSnapshotEncodePolicy`）であって、予約確定時でも録画開始時でもない。再導出（reservations 経由で毎回引き直す）は選べない —— 導出元（`reservations` / `program_overrides` / `program_intents`）は放送終了 + 猶予後に GC される寿命の短い表だが、`recordings` は永続資産（CLAUDE.md 不変条件 12「表は行の寿命で割る」）。導出に依存させると、番組が EPG から消えて GC された時点で desired が空になり、エンコード未完了の録画で原本削除が止まる／再エンコードが投入できなくなる。凍結した `recording_encode_policy` の行は「この録画の望ましい最終状態」であり、`recordings` 行と同時に生まれて同時に死ぬので不変条件 12 には反しない（衛星表として別テーブルに置くことは「行の寿命が同じ」であることと矛盾しない。不変条件 13 参照）。ただし凍結する以上、**ingest 完了より後の override 変更はその録画には反映されない**という境界が生まれる（[録画エンジン](../recording.md) §4.5）。

**予約をどのキーで引くか**: `resolveAndSnapshotEncodePolicy` は予約を `reservations.id` への FK ではなく、放送イベントキー `(site, network_id, service_id, event_id)` で引く。`reservations.id` は ruler の導出削除・再実体化（EPG フリッカー、ルール編集、dedup）で変わりうる不安定な値（CLAUDE.md 不変条件 9「identity」: 導出器が作るキーで引かない）で、録画開始から ingest 完了までの窓（番組の尺ぶん、数時間）でこれが起きると FK は予約を見失う。放送イベントキーは `recordings` が録画開始時から凍結して持つ列（導出器が作るキーではない）なので、予約の再実体化を跨いでも変わらない。

具体的には `program_snapshots` で `(network_id, service_id, event_id)` → `program_id` を引き、`reservations` を `program_id` で結合する（`GetReservationEncodePolicyByEvent`、`internal/db/queries/recording_policy.sql`）。`program_snapshots` は放送後 `epg.retention_grace`（既定 24h）で GC される寿命の短い表（[スキーマ](../schema.md) §3「射影にある間は更新、消えたら凍結」）で、ingest は通常なら録画終了直後 --- GC の猶予期間より十分前 --- に走る。**ただし「通常なら」であって、滞留の設計はこれを超える遅延を明示的に許容している**（下記「凍結が依存する寿命と、エッジの滞留の交点」）。

`recordings.source`（`DeriveRecordingSource`、`internal/db/recording_source.go`）はこの JOIN 失敗の異常度を判定する軸として使える場面が半分しかない。`source = 'rule'` は「作成時点で予約があり、かつ `program_intents.action = 'record'` の行が無かった」を意味するので、JOIN が失敗するのは常に「**予約はあったのに引けなくなった**」を意味し、`slog.Warn` に識別子（site/network_id/service_id/event_id）と recording_id を残す。原因は 3 つ: (a) GC が想定より早く走った、(b) 予約が恒久的に削除された、(c) **GC は設計どおりに走ったが ingest が猶予を跨いで遅れた**（下記「凍結が依存する寿命と、エッジの滞留の交点」。異常系ではなく設計が許容するシナリオだが、エンコードが投入されない点は同じ）。一方 `source = 'manual'` は「intent が `action = 'record'` だった（予約の有無に関わらず）」と「そもそも予約が最初から無かった」（手動起動、日常的）という区別できない 2 つの経路を 1 つの値に潰しているため、JOIN 失敗が異常かどうか判定できない。前者（ユーザーが手動予約して encodeProfiles を指定した録画）で解決に失敗すると静かにエンコードされない状態がそのまま残ってしまうので、`source = 'manual'` でも黙って return せず `slog.Info` に同じ識別子を残す。

**retention reconcile ループ**（worker の cleanup 系ジョブ）が定期的に走り、次を**すべて**満たす原本アセットを削除する:

1. ポリシーが `until_encoded`
2. desired な派生物（ルールで指定した全エンコードプロファイル + サムネイル）がすべて `media_assets` にコミット済み
3. 原本を入力とする実行中・再試行中のジョブがない

命令的なジョブチェーン（最後のエンコードジョブが削除ジョブを投入）だと、複数プロファイル時の「全部終わったら」の fan-in・途中失敗・再実行で壊れやすい。レベルトリガーなら「観測された派生物の集合 >= 望ましい集合」を毎回評価するだけで、どこで落ちても収束する。

上記 2 の「desired な派生物」は凍結時点の `encode_profiles` であり、現在の `encode.profiles` 設定とは突き合わせない。プロファイルを改名・削除して現在の設定に存在しなくなったプロファイルをまだエンコードしていない録画は、`until_encoded` でも原本を保持し続ける（満たせない desired に対して原本を捨てないのが仕様 --- 設定の 1 行の編集で原本という不可逆な資産が削除可能になる方を避ける）。`rokuban_encode_reconcile_unsatisfiable` はプロファイル名別の該当録画数を出すが、`keep_original` を問わず数える（`always` の録画も含む）ため、この保持件数そのものとは一致しない。

### 凍結が依存する寿命と、エッジの滞留の交点

凍結の JOIN 先（`program_snapshots` と、そこへの FK CASCADE で連なる `reservations` / `program_intents` / `program_overrides`）の**行の寿命は放送の時計**で決まる（`start_at + duration_ms` + `epg.retention_grace`）。一方、その最後の読者である ingest がいつ走るかは**エッジの排出の時計**で決まり、エッジのリングバッファは「回線断・クラウド側障害で未 ingest の record が N 日分溜まる」ことを前提にサイジングする（[運用](../operations.md) §4「録画バッファのサイジング」）。2 つの時計の間には制約が書かれていない。交点はこう書ける:

> **encode 意図が生き残る滞留の上限は `epg.retention_grace`（既定 24h）であって、リングバッファの N 日ではない。滞留を N 日まで許すつもりなら `epg.retention_grace >= N` にする。**

GC 済みのスナップショットの上で ingest が走った場合に何が起きるか（括弧内は確認しているテスト）:

- encode policy は既定値 `keep_original='always'` / `encode_profiles=[]` で凍結される（`TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults` / `TestIngestWorker_NoReservation_LeavesEncodePolicyDefault`）。**原本は残るのでデータは失われず、エンコードだけが投入されない**
- 落ちるのは encode 意図だけではない。復帰時に `recordings` 行を作る `internal/watcher` の `createRecording` も同じ GC 済みの予約を引くので、ルール由来の録画でも `source` が `manual` に倒れ `rule_id` が NULL になる（`TestProcessRecord_ReservationGCedBeyondGrace_SourceManual`）
- したがって**その録画のログは `slog.Warn` ではなく `slog.Info`** になる（`TestIngestWorker_LogsInfoWhenManualSourceReservationUnresolvable`）。上記「`source = 'rule'` なら Warn」が実際に出るのは、**録画開始時には予約が生きていて、ingest だけが猶予を跨いで遅れた**場合（上の原因 (c) のうち観測が届いていたぶん）に限られる。この 2 段（GC → `manual` → Info）を 1 本で通すテストは無い（パッケージ境界。上記 2 テストの合成である）
- 事後回復は `POST /api/recordings/{id}/encode-profiles`（下記「凍結の例外: 事後追加」。追加のみ）**だけ**。**`encode_reconcile`（desired−observed の定期パス。[ingest](../recording/ingest.md) §5.5）はこのケースを回復しない** —— `ListRecordingsMissingEncodes`（`internal/db/queries/encode_reconcile.sql`）が `cardinality(encode_profiles) > 0` を要求するので、既定値（`encode_profiles = '{}'`）で凍結された録画はそもそも候補に入らない。desired が空である以上「欠けている派生物」も無く、バックストップとしては正しい振る舞いだが、**失われた意図は誰も取り戻さない**

**GC 側を滞留と連動させる案（未 ingest の `record_sync` が指す放送イベントのスナップショットを刈らない）は採らない。** 留め置きの根拠にできるのは `record_sync` 行であり、それは watcher が mirakc を観測して初めて作られる（`internal/watcher/watcher.go` の `processRecord` 入口の `AcquireRecordSync` が status を問わず作り、`Sweep` は進行中の record も列挙する）。したがって:

- **断が始まる前に一度でも観測された record にはアンカーがある** —— この分は案 1 でも留め置ける（`status='recording'` の段階で観測されていれば足りる）
- **断の最中に始まった録画にはアンカーが無い。** 行ができるのは復帰後で、そのときには GC は済んでいる（`runGC` は ruler のサイトパスが失敗しても実行され、削除条件は時計の比較だけ）

つまり**正しい分割は「リンクが生きているか」ではなく「その record の観測が届いていたか」**で、数日の断ではその大半が未観測になるので、案 1 は主要部分を塞げない。加えて `DeleteEndedProgramSnapshots`（この表から行を消す唯一の経路。`internal/db/queries/program_snapshots.sql`）が `record_sync` の状態に依存し、ingest が恒久的に完了しない record がスナップショットと予約を無期限にピン留めする漏れができる。

同じ理由で**凍結を録画開始時へ前倒す案も採らない**（`recordings` 行の生成も watcher の観測に依存するので、断の最中に始まった録画ではクラウドから見た「録画開始時」が「復帰時」になる）。加えて[録画エンジン](../recording.md) §4.5 の「録画開始後の変更でも ingest 完了までは効く」を失う。`epg.retention_grace` を上げることは、この 2 案が塞げる範囲と塞げない未観測ぶんの両方を 1 つの数で覆う。

**ただし「上げれば済む」ではない。既定は上げない** —— コストは滞留を N 日許す構成にだけ課す:

- `reservations` / `program_snapshots` / `program_intents` / `program_overrides` の行が「予約された番組数 × N 日」ぶん長く残る。時間窓で絞らずこれらを読む経路がある（`ListCapacityDemand` / `ListCapacityDemandAllSites`）
- **同じキーが EPG 射影のローリングウィンドウも駆動する。** `cfg.Epg.RetentionGrace` は ruler の GC と EPG 射影の `PruneEpgPrograms`（`internal/worker/epg.go`）の**両方**に渡される（`cmd/rokuban/server.go`）ので、N 日にすると `epg_programs` に**予約の有無に関わらず全サービスの放送済み番組**が N 日ぶん残る。ルール照合（`internal/rulequery` の `MatchProgramIDs`）は時間で絞らないのでその放送済み番組も拾い、終了済み番組の予約に対して reconciler の `programEnded` 分岐と `recordNeverScheduled` が**永続表 `never_scheduled_events` に欠測行**を作る窓が 24h から N 日に広がる。**行がどれだけ増えるかは未検証**（この節の他の記述と違い、測っていない）

滞留を見張るメトリクスと閾値は[運用](../operations.md) §4 にある。

**「クラウド側障害」と「回線断」を同じ結論で括らない。** 上の帰結が決定論的に成り立つのは「GC は動き続けたが ingest が猶予を跨いで遅れた」場合であって、ruler ごと止まる障害（worker 全停止・DB 到達不能）では**断のあいだ GC も進まない**。復帰時は sweep + ingest と `ruler_pass` の競争になり、ingest が先に走れば意図は守られる。どちらになるかはジョブの実行順に依存する（未検証）。

### 安全性

- **放送データが 0 コピーになる瞬間は構造的に存在しない**。エッジの record 削除は ingest コミット後（[録画エンジン](../recording.md) 参照）、原本削除はエンコード検証後。常に 1 コピー以上ある
- **「唯一のコピーを消す」パスがない**。エンコードが恒久的に失敗すれば条件 2 が満たされず原本は自然に保持され続ける（+ アラート対象）
- **条件 2 の「全プロファイル完備」は `encode_profiles` が空でないことも要求する**。API はエンコードプロファイル未指定のルールで `until_encoded` を選択不可にしているが（下記「UI / 運用」）、それを回避して `until_encoded` かつ `encode_profiles = '{}'` の組が `recording_encode_policy` に焼かれた場合、「全称量化された条件が空集合に対して自明に真になる」ため対策なしでは即座に原本が消える。`cardinality(encode_profiles) > 0` を要求するガード（同じ条件を `recording_encode_policy` テーブル自身の CHECK にも持つ）は、削除 reconcile が until_encoded 腕を消費する箇所ごとに手で複製するのではなく、名前付き述語 `until_encoded_deletable_originals`（view。§7 参照）の定義 1 箇所に置く。これにより、入力側の検証が抜けても、この view を参照するすべての経路（入口・前パスの拾い直し・否定形の判定）に構造的に効く
- **削除プロトコルも冪等**: アセット行を deleting にマーク → unlink → deleted にマーク。どこで落ちても reconcile が拾い直し、残骸は孤児クリーンアップが回収
- **メタデータは tombstone として残す**。ドロップスキャン結果・元サイズ・録画品質は原本削除後も UI で見られる（「ドロップがあったから再放送を待つ」判断は削除後にこそ必要）

### UI / 運用

- 原本削除後は**再エンコード不可**になるため、ルール設定で明示。デフォルトは安全側の `keepOriginal: always` とし、ストレージ効率はユーザーの opt-in
- エンコードプロファイル未指定のルールでは `until_encoded` を選択不可（原本が唯一の視聴可能物）
- 視聴は常に派生物側（MPEG-2 TS はブラウザ直接再生に不向き）なので、原本削除で失うのは再エンコードの自由度だけ。H.265 で 1/4〜1/10 になるため、これが実質のストレージ戦略になる

### 凍結の例外: 事後追加

`recording_encode_policy.encode_profiles` は ingest 完了時に一度だけ焼き込まれる凍結値だが、**ユーザー起点の追加方向の書き換えだけは凍結の例外として認める**。予約が無い録画（mirakc に直接起こされた手動録画等）は `encode_profiles = '{}'` のまま永久に凍結されエンコードを依頼する手段が無かった問題と、録画完了後に「もう1つプロファイルを足したい」という要求に応える。

- **範囲は追加のみ**。`POST /api/recordings/{id}/encode-profiles`（`internal/api/recordings.go` の `AddRecordingEncodeProfiles`）は `AppendRecordingEncodeProfiles`（`internal/db/queries/recordings.sql`）で union + dedup にしか書けない。全置換にすると、ユーザーが誤って既存のプロファイル指定を消す事故につながるため、その経路自体を用意しない
- **原本が active でなければ不可**。`GetActiveOriginalMediaAsset` が `ErrNoRows` の録画（原本削除済み、`state = 'deleting'`（unlink 待ち。一覧の射影は `state <> 'deleted'` なので UI 上は「原本あり」に見える）、またはそもそも ingest が完了しておらず `kind='original'` の行自体が無い、のいずれか）には 409 を返す。`EnqueueMissingEncodes` はこのケースで黙って no-op になる（原本が無ければ何もしない設計。上記「安全性」参照）ため、サイレントな失敗にしないよう api 層で明示的に検査する
- **`recording_encode_policy` に行が無い（未凍結）録画でも、原本が active なら追加できる**。`internal/inplace.Register`（災害復旧。カタログを 1 世代も持たない状態からのストレージ再スキャン）が作る原本は `internal/worker/ingest.go` の `resolveAndSnapshotEncodePolicy` を経由しないため、`recording_encode_policy` 行が無いまま原本だけが active な録画が存在しうる。`AppendRecordingEncodeProfiles` は `INSERT ... ON CONFLICT (recording_id) DO UPDATE` で書くので、行が無ければ「原本が active = 凍結済みとみなす」を適用して `keep_original = 'always'`（安全側の既定値）で新規に凍結し、行があれば `encode_profiles` だけ追記する。行の有無をここで判定してエラーにする経路は持たない —— 原本が active でなければ手前の `GetActiveOriginalMediaAsset` の 409 検査で既に止まっているため、この INSERT に到達する時点で「原本 active」は保証されている
- **実行経路**: api がトランザクション内で `encode_profiles` を更新し、同一トランザクションで `EncodeEnqueueHintArgs`（ヒントジョブ）を投入する。実際の `EnqueueMissingEncodes` 呼び出し（desired − observed の差分を埋める encode ジョブの投入）は worker ロール側の `EncodeEnqueueHintWorker` が行う（既存の hint job パターン。`rules.go` の `insertRulerPassHint` と同型）。詳細は `internal/worker/encode.go` の `EncodeEnqueueHintArgs` の doc コメント参照
- この例外を経ても「ingest 完了時点で確定した最終状態」という設計そのものは変わらない —— 削除・変更方向の書き換えは今も無い

## 7. 削除エンジン

### 物理削除は 1 本の reconcile ループに統一

物理 unlink に至る経路を 3 ソース → 1 つの削除 reconcile に揃える:

| ソース | 猶予 | 意図 |
|---|---|---|
| 手動削除（ごみ箱） | `deleted_at` + 30 日（設定可） | 人為ミスへの備え |
| 原本の保持ポリシー（`until_encoded`） | なし（派生物完備が条件） | 設計されたポリシー削除。**ごみ箱は経由しない**（原本はサイズが支配的で、経由させるとストレージ節約が猶予期間ぶん遅延する。安全条件は派生物完備で既に担保） |
| 孤児ファイル | mtime 猶予 + エイジング | DB 喪失・残骸への防御 |

**一括削除サーキットブレーカーはループ全体に 1 つ**: ソースを問わず 1 パスの物理削除が閾値（件数 / ライブラリ比率 / 総バイト数、例: 5% or 100 GB）を超えたら停止してアラート。

### 削除可否の述語に名前を与える

「このアセットは消してよいか」は、ごみ箱腕（猶予超過 or 今すぐ purge）と until_encoded 腕（派生物完備）の 2 つで、`internal/db/queries/delete_reconcile.sql` の 5 クエリ（入口 2 つ・前パスの拾い直し・否定形 2 つ）がこれを消費する。以前はこの 2 腕を 5 クエリに手で複製しており、`cardinality(encode_profiles) > 0` のガードが複製の 1 つ（入口）にしか入らずドリフトした。

いずれの腕もスキーマ側に名前を与え、5 クエリはそこへの参照にする:

- **until_encoded 腕**: パラメータを取らないので view `until_encoded_deletable_originals` にする
- **ごみ箱腕**: `grace_cutoff` がパラメータなので view には畳めず、set-returning SQL 関数 `trash_deletable_recordings(grace_cutoff)` にする
- 否定形（`ListUnqualifiedDeletingAssets` / `RevertMediaAssetToActive`）は、この 2 つの述語への `NOT EXISTS` で書く。手で「同条件を再掲」するコメントを揃える義務が無くなる

### 不変条件の修正

従来の「DB にないファイル = 孤児 = 削除対象」は、暗黙に「DB は常にファイルより新しい」を仮定しており、**DB リストアはこの仮定を壊す唯一の正規操作**。契約を「孤児に見えることは削除の必要条件であって十分条件ではない」に修正する。

### ごみ箱 = 論理削除。「場所」ではなく「状態」

- UI の削除は `deleted_at` を立てるだけ（録画単位。原本 + 派生物 + サムネイルのアセットグループごと）。ファイルには触れない
- ごみ箱ビュー = `deleted_at IS NOT NULL` の一覧。**復元は `deleted_at` を消し、即時削除の要求行（`recording_purge_requests`）を消すだけ**（ファイル操作ゼロ・即時）。「今すぐ完全削除」も個別/一括で可能。即時要求を `recordings` の列ではなく衛星表に置く理由は [スキーマ](../schema.md) §5 の「ごみ箱」
- 物理的な隔離ディレクトリへの移動はしない（FUSE-S3 では数十 GB の rename がコピーになる。論理削除なら I/O ゼロで同じ猶予が得られる）
- 物理削除後も tombstone は残る → ドロップ統計・録画履歴は消えず、**ごみ箱を空にしても再放送重複排除は壊れない**
- **`recordings.purged_at` は「完全削除が完了した」不可逆な事実を持つ列。** 削除 reconcile がパス末尾で、ごみ箱条件を満たしかつ物理削除が終わっていない `media_assets` が 1 行も残っていない録画に一度だけ立てる。tombstone は上の行のとおり残り続けるが、ごみ箱ビュー（`ListTrashRecordings`）は `purged_at IS NULL` も要求するので、purge が完了した録画はごみ箱一覧には出ない。「`media_assets` に未削除行が 0」を毎パス導出する案は採らない —— アセットを一度も持ったことがない録画ではこの条件が purge 前から真であり、「消した」と「元から無い」を区別できないため（CLAUDE.md 不変条件 9）
- 将来オプション: ごみ箱サイズの UI 表示 + 空き容量逼迫時に猶予期間前でも古い順に purge する容量トリガー。初期実装は期間ベースのみ
- **復元と物理削除の競合**: `media_assets.state = 'deleting'` は unlink 待ちの間しか続かない一時状態で、**復元は `media_assets` に一切触れない**（`recordings.deleted_at` を消して即時削除の要求行を消すだけ）ため、unlink が失敗して `deleting` のまま次パスに持ち越されると「復元したのに次パスで消える」窓ができうる。前パスの `deleting` 行を拾い直す経路（`ListMediaAssetsPendingDelete`）は無条件に unlink へ進むのではなく、trash 猶予超過 / until_encoded 派生物完備の判定（上記「削除可否の述語に名前を与える」の 2 つの名前付き述語）を**適用の瞬間に再評価**する。該当しなくなった行は `ListUnqualifiedDeletingAssets`（この 2 述語への `NOT EXISTS`）で候補として挙げ、`resolveUnqualifiedDeletingAsset` がファイルの現存を `stat` で確認したうえで、まだ存在すれば `active` に戻し、既に無ければ（unlink 成功後 `MarkMediaAssetDeleted` のコミット前にプロセスが落ちていた場合）`active` には戻さず `deleted` を確定する——ここで無条件に `active` へ戻すと、復元 API 側で `deleting → active` を即時に書き換える方式（却下案）を採らなかった理由そのもの、「`active` なのにファイルが無い行」を revert 経路自身が作ってしまうため
- **復元と即時削除要求の競合**: 復元は「`deleted_at` を消す」と「即時削除の要求行を DELETE する」の 2 表更新で、**1 文のデータ変更 CTE ではなくトランザクション内の 2 文**で流す。CTE はアーム全体が 1 つのスナップショットを共有するため、行ロックで UPDATE アームが待たされている間に commit された要求行が DELETE アームから見えず、「復元は成功したのに要求行だけ残る」が観測される。残った要求行は上の「ごみ箱腕」が `deleted_at IS NOT NULL` を要求するのでその場では何も起こさないが、**次の普通の論理削除で猶予をバイパスして即時 purge の対象になる**（ユーザーは即時削除を要求していない）。2 文なら DELETE が UPDATE の後に新しいスナップショットを取るので要求行が見える。**ただし窓を閉じているのは 2 文に割ったことではなく、要求行を入れる経路が先に対象の `recordings` 行をロックすること** —— DELETE が 0 行だったときロックは何も残らないので（READ COMMITTED に述語ロックは無い）、「DELETE の後・COMMIT の前」に要求行が commit されれば同じ害が出る。個別 purge は `recordings` の UPDATE がそのロックを兼ねている。**上の「一括」を素直に `INSERT INTO recording_purge_requests SELECT id FROM recordings WHERE deleted_at IS NOT NULL` と書くと `recordings` をロックしないので窓が開き直る** —— この表に行を入れる経路は必ず対象の `recordings` 行を先にロックする

### 孤児回収の 3 重の安全弁

1. **mtime 猶予**: mtime が 7 日以内のファイルは孤児候補にすらしない（正常系の録画 → ingest → エンコードは数時間で完結）。バックアップが 1 日古い程度のリストアはこれだけで守られる
2. **孤児エイジング**: 孤児候補は `orphan_files` テーブルに first_seen を記録し、14 日連続で孤児であり続けたものだけ削除。**観測記録が DB 側にあるため、DB リストアで時計もリセットされ、削除までの窓が自動的に開き直す**
3. **サーキットブレーカー**（上記）: DB 全損直後は全ファイルが孤児に見えるため確実に発動する

「リストア後は cleanup を止めておく」という人間の記憶に頼る運用が不要になる。

### 孤児回収の逆: 実体無し検出

孤児回収は「ファイルはあるが `media_assets` に無い」を追う。逆方向 --- `state = 'active'` なのに実体ファイルが無い行 --- を検出する経路が無く、行が嘘をついていても誰も気付かない穴があった。不変条件 3（コミット = DB 行）は行が真実であることを要求するだけで、それを検証する経路までは要求しないため、この穴は不変条件そのものの欠陥ではなく検出器の不在だった。

削除 reconcile パスが孤児回収と同じ 1 回のディレクトリ走査結果を使い、逆方向にも突き合わせる。`state = 'active'` な行のうち、その走査でファイルが観測されなかったものを `missing_media_assets`（`orphan_files` の鏡像。行の存在 = 直前のパスで実体を観測できなかったという主張）に記録し、Warn ログと `rokuban_media_assets_missing{kind}` ゲージに出す。**自動では一切消さない** --- 「ファイルが無い」は削除の必要条件であって十分条件ではない（孤児回収と同じ非対称。手動削除・バックアップからの部分復元・FS の破損・別プロセスの事故のいずれでも起きるため、実削除の判断は人間に委ねる）。

孤児回収の 3 重の安全弁と同じ慎重さを、逆方向の形で 2 つ持つ:

1. **エイジング**: 候補は `first_seen` を記録し、既定 24 時間連続で観測され続けたものだけを確認済みとして報告する（`missing_asset_age`。単発の走査揺れを異常と区別する。孤児エイジングの 14 日と揃える必要はない --- 削除の猶予ではなく通知の遅れを抑える値なので短めにしている）
2. **全損シグネチャ**: 「この 1 回の走査でファイルを 1 件も観測できなかったのに `active` な行が存在する」という**形**で検知し（件数の閾値ではない。reconciler の `breaker.ReconcileTotalLoss` と同じ考え方）、そのパスの記録を丸ごと見送る（`rokuban_missing_asset_scan_suspected_storage_failure_total` が進む）。既存の確認済み候補にも触れない --- 疑わしいパスの結果で前回までの状態を上書きしない。**この形が「マウントが落ちている」を捕まえられるのは 1 マウント = `media_dir` 全体という構成でだけ**である。判定に使うのは走査が観測した全ファイル（`catalog/` 以外）の件数なので、`media_dir` 直下がローカルディスクでその下のサブツリーだけがマウント（アーカイブ階層だけが落ちる部分マウント障害）だと、あるいは `.DS_Store` / `lost+found` / mtime が新しくて孤児回収も消せない残骸が 1 個でもあると、`0 件` にならず発動しない。その場合は死んだサブツリー配下の `active` 行が全部エイジング後に確認済みとして報告される（削除はしないので被害は騒音のみ。マウント単位で判定する形にはしていない —— `media_dir` 配下のどこにマウント境界があるかは設定にも `rel_path` にも現れず、Rokuban はそれを知らない）

サーキットブレーカーは持たない。孤児回収・`until_encoded` の安全弁は「大量削除という不可逆な操作」を止めるためのものだが、この検出は削除を一切行わないので止める対象が無い。

**同じ走査を逆向きに使うと、走査の除外の誤りの向きが反転する。** 走査は `catalog/` を SkipDir する（[contract.md](contract.md) §5 のトップレベル予約ディレクトリ）。孤児方向では除外は安全側 --- 除外されたパスは孤児候補にならない＝削除されない。逆方向では**除外されたパスの資産が恒久的に「実体無し」と誤報される**（15 分ごとの Warn とゲージが下がらない。削除はしないので被害は騒音のみ）。今日は該当する `rel_path` が存在しないが、それは「既存行が予約ディレクトリを先頭成分に持たない」という contract.md §5 自身が但し書きを付けている前提の上の断定なので、走査の除外を足すときは削除側だけでなくこちら側の誤報も確認する。

**同型の穴が「走査が降りないディレクトリ」の側にもある。** 走査は symlink を辿らないが、書き込み・読み出し側のパス解決は字句的（`filepath.Join` + 接頭辞検証）なので、`media_dir` 配下のディレクトリ成分を symlink にして別ボリュームを合成する構成では、**書けて読めるのに走査からは見えない**サブツリーができ、その配下の資産が恒久的に誤報される（未検証 --- symlink を張った構成のテストは無い）。走査側で辿る形にはしていない: 循環と、`media_dir` の外を指す symlink（`rel_path` が契約上の名前空間の外のファイルを指すことになる）の扱いを先に決める必要があり、この検出器の範囲を超える。合成が必要ならディレクトリ成分の symlink ではなくマウントを使う。

### 既存不変条件の再確認

- **「放送データのコピーが常に 1 つ以上」は DB 喪失時も維持される**: エッジ record の削除は ingest の DB コミット後 → コミット直後に DB を失ってもファイルはアーカイブに存在し、安全弁が守り、rescue が再登録する
- cleanup は mirakc の basedir に絶対に触らない（エッジ側削除は ingest の検証済み削除のみ）

## 経緯と失敗事例

- 保持ポリシーの `recording_encode_policy` への凍結は M3-14 / issue #103。「行の存在 = 凍結済み」の衛星表化は issue #159
- 予約を放送イベントキーで引く形は issue #149。旧実装は `recordings.reservation_id`（bigint FK、`ON DELETE SET NULL`）で予約を引いており、録画開始から ingest 完了までの窓で ruler の導出削除・再実体化が起きると FK が NULL に落ち、「予約が無い」と誤認して encode policy を凍結し損なっていた（ログにも出ない）。列自体は issue #158 で削除。導出器が作るキーで引く同族の失敗は #53 / #98 / #99
- 空の `encode_profiles` で「全称量化が空集合に自明に真」となり原本が即座に消える罠は issue #103 で特定。ガードを名前付き述語 1 箇所に置く形は issue #160 —— それ以前は issue #104 で入れたガードが 5 複製の 1 つ（入口）にしか入っておらずドリフトしていた
- エンコードプロファイルの事後追加（凍結の例外）は issue #133。未凍結（`internal/inplace.Register` 由来）の録画の扱いは issue #159 のレビューで発見
- 「凍結が依存する寿命と、エッジの滞留の交点」は issue #214。docs 全体のレビューで見つかった設計前提の衝突で、コードのバグ報告ではない。**片方の doc が「ingest は GC 猶予より前に走る」と書き、もう片方が「N 日分の滞留を吸収する」と書いていて、互いを見ていなかった。** GC を `record_sync` と連動させる案・凍結を録画開始へ前倒す案は、どちらも滞留の主因である回線断の**未観測ぶん**（断の最中に始まった録画。クラウド側にアンカーが無い）を塞げないことが分かったので採らず、`epg.retention_grace` とリングバッファの N 日の関係として書いた。**初版は「クラウド側にアンカーが無い」を無条件に書いていた** —— 断の前に観測済みの record にはアンカーがある（`AcquireRecordSync` は status を問わず行を作る）ので、正しい分割は「リンクが生きているか」ではなく「その record の観測が届いていたか」。同じ PR で「未 ingest 滞留のアラートは回線断への備え」と書いていた 3 箇所（`internal/db/queries/metrics.sql` / [アラート設計](../operations/alerts.md) / [ストレージ契約](contract.md)）も、この結論と衝突したまま残っていたので直した。
- `recordings.purged_at` は issue #135。復元と物理削除の競合の閉じ方（適用の瞬間の再評価と `stat` 確認）は issue #105
