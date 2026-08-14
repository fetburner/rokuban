> [recording.md](../recording.md) §5「ingest パイプライン」・§6「B-CAS 復号の責務境界」の一部。索引から辿る。

## 5. ingest パイプライン

録画完了後、mirakc のエッジから Rokuban のアーカイブストレージへ録画データを取り込む一連の処理。

### 5.1 転送方式: API pull 固定

`records/{id}/stream` による HTTP pull を全構成で統一する。「monolith モードなら mirakc の basedir を直接読めるのでは」を検討し、**HTTP loopback 経由を維持**と結論した:

- **ディスク I/O は直読みでも減らない**。basedir（リングバッファ）→ メディアストレージのコピー自体は必要で、節約できるのは loopback TCP のオーバーヘッドだけ。1 日数本・数十 GB では無視できる
- **コピー自体が耐障害設計**。録画はシステム内で唯一のリアルタイム・リトライ不能な操作なので、ローカルディスクへ録画 → 完了後にリトライ可能な転送、という分離は崩さない（mirakc に最終保存先へ直接書かせる案は、録画中の NAS/FUSE ストールが放送の欠損に直結するため不採用）。ドロップスキャンも転送パスがあるからタダで載る
- **コードパスが 1 本**。HTTP pull は monolith / 分散 / ハイブリッドの全構成で動く唯一の方法
- **所有権が明確**。basedir は mirakc の所有物で、Rokuban は API 越しの客に徹する

「同居時の basedir 直読み」は loopback が実測でボトルネックになった時の最適化オプション（YAGNI）。契約は **mirakc とは常に API、自身のストレージとは常にファイルシステム**の 2 面で固定（[storage.md](../storage.md) 参照）。

### 5.2 インライン TS ドロップスキャン

ingest はどのみち record の全バイトをストリームコピーするので、その途中で 188 バイト境界の TS パケット統計を取得する。**追加 I/O パスゼロ**で EPGStation 相当のドロップログが作れる。

採取する統計:

- PID ごとの continuity counter 不連続
- transport_error_indicator
- scrambling_control

PID 別サマリを media_assets に紐づくテーブルへ格納し、UI で表示する。実装は `internal/tsstat`。

#### 判定の規約（誤検知を出さないために必要なもの）

continuity counter の不連続を数えるだけでは実放送で大量の誤検知が出る。実測した
既知の良品（NHK 総合、5.3GB / 2841 万パケット）では `discontinuity_indicator` が
230 回立っていた。以下を守って初めて drop が 0 になる。

| 規約 | 扱い |
|---|---|
| NULL パケット（PID 0x1FFF） | CC は意味を持たないので統計対象外 |
| payload なしパケット（`adaptation_field_control` が `00` / `10`） | CC は増えない。**増えていたら payload 付きパケットの欠落**として数える |
| `discontinuity_indicator` | CC の不連続は正常。基準を取り直す |
| CC が直前と同じ + payload が**同一** | 規格が許す重複。1 回までは正常、2 回以上は異常 |
| CC が直前と同じ + payload が**相違** | 重複ではなく 15 個（16n-1）欠落。CC が一周して直前と一致している |
| PID の初回パケット | 基準がないので数えない |
| `transport_error_indicator` | error に数え、**CC の追跡から外す**（後述） |
| `transport_scrambling_control` | `00` 以外を異常として数える |

**16 の倍数の欠落は原理的に検知できない。** CC は 4 ビットなので、ちょうど 16n 個
欠落すると CC が期待値と完全に一致し、payload を見ても正常な次のパケットと区別が
つかない。これは規格上の限界で、他実装も同じ。

#### 検証方法

`internal/tsstat/integration_test.go` が既知の良品に対する差分テストを持つ。
`ROKUBAN_TEST_TS_FILE` に別実装で drop / error /
scrambled がいずれも 0 と確認済みの .m2ts を指すと有効になる。ファイルは巨大かつ
著作物なのでリポジトリには置かない。

clean なファイルでは「誤検知がないこと」しか確かめられないので、検知側は同じ
ファイルをストリーム処理中に**メモリ上で**壊して検証する（欠落・TEI・scrambling を
注入。ディスクに改変コピーは作らない）。

#### [tspacketchk](https://github.com/kaikoma-soft/tspacketchk) との差分

判定ロジックは概ね一致している（重複を payload 比較で見分ける点、payload なし
パケットの CC を検査する点は同実装から取り入れた）。意図的に違えているのは 1 点:

**TEI パケットの CC を信用しない。** tspacketchk は TEI 時に error を数えつつ CC を
更新するため、直後のパケットで drop も 1 数える（1 つの破損が error と drop の両方に
計上される）。Rokuban は TEI 時に継続性の追跡を打ち切り、次のパケットで基準を
取り直す。破損の実数を二重に数えないことを優先した。

**他の候補との比較**:

| 候補 | 判定 |
|---|---|
| mirakc の `logFilter` ログ | Web API に record のログファイルを取り出すエンドポイントが存在しない。収集には共有 FS かエッジ転送エージェントが必要 → 不採用 |
| インラインスキャン | 追加 I/O ゼロ。採用 |
| `recording.record-broken` / `recording.failed` | 構造化品質シグナルとして補完的に使用 |

外部ツール（tsselect 等）の exec も検討したが、数十 GB への二度目の I/O パスと依存の追加に見合わないため、インライン 1 パスとする。

### 5.3 リトライ設計（3 層）

ソース確認（`mirakc-core/src/web/api/recording/records/stream.rs`）:

- `GET /records/{id}/stream` は **Range ヘッダー対応**（`compute_content_range`）→ `Range: bytes=N-` で途中再開可能
- **フィルタ併用時は Range が 400**。ingest は素の TS が欲しいのでフィルタなしで pull → 常に Range 可
- **HEAD エンドポイントあり**（`checkRecordStream`）→ 転送せず正確な Content-Length を取得できる

#### 層 1: 接続断の再開（ジョブ内リトライループ）

切断時は書き込み済みオフセットから `Range: bytes=N-` で再接続して追記。ドロップスキャンのカウンタはメモリ上に生きているので継続できる。タイムアウトは総時間ではなく**ストール検知**（`ingest.stall_timeout`、既定 30 秒間無進捗で切断扱い）--- 総時間タイムアウトは遅い回線の正常な転送を殺す。

#### 層 2: ジョブ再試行（プロセス死）

River の at-least-once + 指数バックオフ。**ゼロから作り直す**（部分ファイルは truncate）。中途再開はスキャナ状態の永続化とストレージ契約（シーケンシャル一発書き）違反の追記が必要になり、層 1 で大半が救われる以上、複雑さに見合わない。

#### 層 3: 完全性検証とコミット

pull 完了後に書き込みバイト数を HEAD の Content-Length と照合 → 一致で `media_assets` コミット（コミット = DB 行。部分ファイルは孤児として cleanup が回収）→ **mirakc 側の record 削除はコミット後のみ**。**HEAD が長さを返さない場合（`Content-Length` 不明）はこの照合をスキップしてそのままコミットする**（`ingest.go` の `expectedLen >= 0` ガード）。どこで落ちても最悪「もう一度 pull」で、データ喪失は構造的に起きない。

運用上の主なリスクは**長時間の転送失敗でエッジのリングバッファが溜まり続ける**こと。`IngestWorker` 自体は River の既定の試行上限のままで、上限に達すると discard（dead-letter）されうる。それでも record が宙に浮かないのは、mirakc 側の record がコミット成功後にしか削除されない（上記のとおり）ため: discard された後も record_sweep（5 分周期の定期全量突き合わせ。[watcher.md](watcher.md) §3.3 の (c)）が同じ finished record を見つけ、`processRecord` が同一トランザクションで ingest ジョブを再投入し続けるからである。「未 ingest の record 総量」をメトリクス化してエッジのディスク残量と突き合わせてアラートする（[storage.md](../storage.md) のサイジング指針参照）。

**帰結はディスクだけではない。** 滞留が `epg.retention_grace`（既定 24h）を跨ぐと、その録画の encode policy は予約から解決できず既定値で凍結され（エンコードが投入されない）、`recordings.source` / `rule_id` も落ちる。原本は残るのでデータは失われない。**このケースは下記 §5.5 の `encode_reconcile` でも回復しない**（desired が空になるので候補に入らない）。詳細と、滞留の型ごとに見るメトリクスが分かれること（**未 ingest 総量は回線断の滞留を数えない**）は [storage.md](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」と [operations.md](../operations.md) §4。

#### 冪等性: コミット済みなら転送をやり直さない

`media_assets` に `kind='original'` の行が既にコミットされていれば、ジョブは転送を行わず、エッジ record の削除だけを再試行して終わる（`IngestWorker.hasOriginalMediaAsset`）。エッジ record の削除は失敗してもログのみで ingest 自体は成功扱いにしているため、mirakc 側に record が残ったまま record_sweep 経由で同じ record の ingest ジョブが再投入されうる。ここで止めないと `os.Create` がコミット済みファイルを 0 バイトに切り詰めて全量を再ダウンロードし、streamer が不変条件 3（コミット = DB 行）に反して欠けたファイルを配ることになる。

#### 宛先 rel_path の排他: 一意索引が効く前に決着させる

`media_assets` の一意索引（`rel_path`, `WHERE state <> 'deleted'`）が効くのは `commit` の INSERT の瞬間だが、宛先へのバイトはそれより前に落ちる（[storage/contract.md](../storage/contract.md) §3 ルール 3 の順序そのもの --- コピー完了 → 行の登録）。**したがって順序では実ファイルを守れない。** 別の `recording_id` が同じ rel_path を算出するケース（同一サイト内で `contentPath` が偶然重複する等）で 2 つの ingest ジョブがほぼ同時に走ると、両方が `os.Create` で宛先を開き、先にコミットした側のファイルを後発が上書きしうる（PR #196 のレビューで実測、issue #197）。

これを閉じるため、`IngestWorker.Work` は `determineRelPath` の直後・`os.Create` より前・mirakc のストリームを開くより前に、`rel_path` のハッシュをキーにした **Postgres のセッションレベル advisory lock** を `pg_try_advisory_lock`（ノンブロッキング）で取得し、`commit` まで保持する。負けた側はバイトを 1 つも書かずに失敗し、River のバックオフで再試行する。

- **セッションレベルであってトランザクションレベルではない。** 転送は数時間かかりうるので、トランザクションロック（`pg_advisory_xact_lock`）だと同じ長さのトランザクションを開き続けることになる。セッションロックはコネクションの生存期間にだけ紐づくので、`commit` は別の短命なトランザクションとして自由に行える
- **ノンブロッキングであってブロッキング版（`pg_advisory_lock`）ではない。** ブロッキング版だと、ingest のキュー枠（site あたり 1〜2、下記 §5.4）を「待ち」で丸ごと塞いでしまう
- **先読み（`checkRelPathConflict`、`GetLiveMediaAssetByRelPath`）はロックの下へ移した。** これにより **ingest 対 ingest に関してはもはや先読みではなく決着そのものになる** --- ロックを保持している間、他の ingest ジョブは同じ rel_path への転送を開始できないので、この SELECT の結果は `commit` まで安定する。ここで拾うのは「別の（今 transfer 中ではない）recording が過去にこの rel_path を使って既にコミットした」という恒久的な衝突であり、`state <> 'deleted'` の述語（`active` に限らず、delete_reconcile の unlink 前後の中間状態である `deleting` も含む）は変えていない。**ただし delete_reconcile の状態遷移に対しては、従来どおりヒントのまま** --- delete_reconcile は rel_path の advisory lock を取らないので、この SELECT と実際の `CreateMediaAsset` の INSERT の間に `deleting` → `deleted` の遷移が進む TOCTOU の窓は残る
- **行の一意性の最後の砦は今も一意索引**（レベルトリガー、不変条件 5）。ロックはその代替ではなく、一意索引が効くより前の窓を閉じるためだけにある
- **正直に書く劣化モード**: 転送中にロック用コネクションが死ぬと、ロックは早期に解放される。その窓はロック導入前と同じ TOCTOU に戻るだけで、新しい壊れ方を作るものではない（単調な改善であって完全な排他の証明ではない）

**今でも先に浮かぶ案が壊すもの**: 一時ファイル + `os.Rename` で宛先を作る案は採らない --- rename は S3 マウントの一部（AWS Mountpoint）に存在せず、他（geesefs/s3fs）では数十 GB の実コピーになる（[storage/contract.md](../storage/contract.md) §2）。commit を先にして rename を後にすると、rename が恒久失敗したとき行が指す唯一の実体が一時ファイルのまま残り、`active` 行の実体欠落を検出する経路が無いまま孤児回収に食われる。

### 5.4 負荷分担: worker

`records/{id}/stream` の負荷が乗るのは worker（ingest ジョブ、KEDA で 0〜N）であり、reconciler は数百件のメタデータ diff を回すだけの軽いジョブのまま。ただし**本当のボトルネックはクラウド側ではなくエッジ側**:

- ハイブリッド構成では自宅アップリンク帯域が律速。worker を増やしても速くならない
- エッジでは録画中の書き込みと pull の読み出しが同じディスクで競合する。pull がディスクを飽和させて録画をドロップさせるのは本末転倒

→ **ingest の同時実行数は mirakc サイト単位で少数（1〜2）にキャップ**する（サイト別キュー or River の同時実行数設定）。worker の水平スケールが効くのは encode（CPU バウンド、入力はクラウド側ストレージ）の方。

### 5.5 ingest 完了後のフロー

**同一トランザクションでの投入はしない。** `media_assets` のコミット**後**に、ベストエフォートのヒントとしてエンコードジョブを投入する（`IngestWorker.Work` → `EnqueueMissingEncodes`。`ingest.go` の `enqueueMissingEncodesFromContext` 呼び出し）。投入に失敗してもログのみで、コミット済みの ingest は巻き戻さない。

**落としたヒントは定期パスが埋める。** ヒント投入の失敗とエッジ record の削除成功（`DeleteRecord`。上記「層 3」）が両方起きると、そのヒントは二度と飛ばない —— エッジに record が残っていないので record_sweep も ingest ジョブを再投入しない。ヒントだけに頼ると、コミット済みの録画が誰にも再投入されず黙ってエンコードされないまま残る。これを塞ぐのが `encode_reconcile` ジョブ（`internal/worker/encode_reconcile.go`、既定 15 分周期）で、desired（`recording_encode_policy.encode_profiles`）− observed（active な `encoded` の `media_assets`）の差分を定期的に取り直して `EnqueueMissingEncodes` を呼ぶ。真実は DB の状態であって「ヒントが飛んだかどうか」ではない（不変条件 5）。

対象は「原本（`kind='original'`）が active でコミット済み」かつ「ごみ箱に入っていない」録画に限る（ingest 未完了の録画とユーザーが捨てた録画を掘り起こさない）。エンコードは site の属性を持たない（アーカイブもプロファイルも単一）ので、このジョブは record_sweep のような site 単位ではなく全体で 1 本。`worker.periodic_jobs: false` の構成では `rokuban enqueue encode-reconcile` を CronJob から叩く（[operations/monitoring.md](../operations/monitoring.md) の CronJob 一覧）。

**繰り返すパスは「投入しても必ず失敗する仕事」を作ってはならない。** ヒントは一度きりなので、設定から消えたプロファイルを投入して `unknown encode profile` で失敗させるのは運用者への通知として妥当だが、15 分ごとに同じことをすると失敗を無限に作り続ける。定期パスは desired を**現在の `encode.profiles` に存在する名前だけ**に絞る。落とした録画は数えて出す（`rokuban_encode_reconcile_unsatisfiable`。プロファイルを改名すると、その名前で凍結済みの過去録画が一斉にここへ落ちる）。

**挙動の変更**: このパスが入るまで、25 回失敗して discarded になった encode ジョブはそこで止まっていた。これからは `encoded` が生まれない限り 15 分ごとに投入し直す（River の一意制約は pending 状態にしか効かず、discarded 済みの引数には合流しない）。真実は River のジョブ履歴ではなく `media_assets` の有無なのでレベルトリガーとしては意図通りだが、**恒久的に失敗するエンコードは「静かに諦める」から「延々と再試行する」に変わる**。

**既知の限界**: 候補は `recording_id` 昇順で 1000 件に切る。このパス自身は候補を減らさない（減らすのは encode の完了）ため、永久に満たせない候補が先頭に溜まると窓を占有し、それより後ろの録画に到達しない（収束は主張しない）。窓が埋まったパスは Warn ログと `rokuban_encode_reconcile_candidates` が上限に張り付くことで見える。[#326](https://github.com/fetburner/rokuban/issues/326) で追う。

### 5.6 転送の途中経過を見せる

`recordings.status = finished` は **mirakc の録画完了**であって取り込み完了ではない。原本が
コミットされるまでブラウザ再生も事後エンコードもできないが、その時間帯を表すものが
`sizeBytes` の省略しか無かったため、遅い回線（実測で数百 KB/s 台）では**止まっているのか
進んでいるのか判別できなかった**。ingest worker は転送中に
`recording_ingest_progress`（[schema/recordings.md](../schema/recordings.md) §5 の衛星表）へ
書けたバイト数を写し、api はそれを `Recording.ingest` として返す。

**進捗の置き場は衛星表**（ジョブ引数でも `record_sync` でもない）。理由は 3 つとも別方向:

- ジョブ引数（`river_job.args`）に持たせると UI が River の内部表を読むことになる
- `record_sync` は mirakc 側の観測で書き手は watcher。転送の進捗は Rokuban 側のファイルに
  何バイト書けたかなので、1 表 2 書き手になる（不変条件 12）
- `recordings` 本体の列にすると、書き手が脊椎（watcher / reconciler）でない状態が脊椎に
  混ざる（不変条件 13）

**分母は `record_sync.content_length`**（watcher が mirakc record の `content.length` として
観測済みの値）。転送開始時に読んで衛星表へ写す。HEAD の `Content-Length` は転送完了後の
照合（層 3）にしか取っておらず転送中には使えない。ファイル stat は api ロールが
ファイルシステムに触れない（不変条件 1）ので分母にできない。mirakc が length を返さない
record では分母を NULL のままにし、UI は % を出さずバイト数だけを出す（でっち上げた分母を
置かない）。

**「リトライ中」を「取り込み待ち」と区別する値は API に持たない。** 区別するには
`river_job` を API 契約に露出させるか、失敗の観測という別寿命の値を進捗行に混ぜる
（不変条件 9 / 12）必要がある。代わりに `observed_at`（進捗を最後に観測した時刻）を返し、
停滞はその古さで読ませる。UI の停滞しきい値は 60 秒 —— 既定のストール検知
（`ingest.stall_timeout` = 30 秒）で正常に再接続している往復を「停滞」と呼ばないため
（`web/src/lib/ingest.ts` の `ingestStaleAfterMs`）。

**API の状態は 4 値で、原本 `media_assets` 行の有無を最優先に導出する**（列に焼いた値では
ない。`internal/api/recordings.go` の `ingestProgressFromFields`）。`kind='original'` の行が
`state` を問わず存在すれば `committed` —— `state='deleted'`（取り込んだ後に削除した）でも
`committed` のままにするのは、**「取り込めなかった」と「取り込んだ後に消した」を混同しない**
ため（[#211](https://github.com/fetburner/rokuban/issues/211) の症状。原本が**いま**あるかは
`sizeBytes` の有無が答える）。取り残された進捗行がコミット済みの録画に「取り込み中」を
名乗らないのも、この優先順位による（真実は `media_assets` 側。不変条件 5）。

**`pending`（取り込み待ち）の根拠は、watcher が ingest ジョブを投入する条件と同じ述語に
揃える**（`record_sync.status = 'finished'`）。`record_sync` 行の**存在**を根拠にしては
ならない —— 行は `failed` / `canceled` の record にも作られ、Rokuban はこの行を消さない
（本番に `DELETE FROM record_sync` の経路は無い）ので、ingest ジョブが一度も投入されない
録画が**永久に「取り込み待ち」を名乗る**。`pending` は「これから来る」の断定なので、
来る根拠が述語として書けないものを入れない（表示側の規律は
[frontend/recordings.md](../frontend/recordings.md)「取り込み中であることを画面に出す」）。

**途中ファイルのサイズを `sizeBytes` に混ぜない**（不変条件 3。コミット = DB 行）。
進捗は `ingest.writtenBytes` という別のフィールドで、原本 `media_assets` 行が生まれる
tx の中で進捗行が消える。

---

## 6. B-CAS 復号の責務境界

### 6.1 復号はエッジ（mirakc パイプライン）の責務

MULTI2 スクランブルの復号（B-CAS カードによる鍵処理 + デスクランブル）を実行するのは mirakc 本体ではなく、mirakc が編成する外部ツール。構成は 2 通り:

1. **チューナーコマンド段で復号**: `recpt1 -b25` 等がチューナー読み出しと同時に libaribb25 + PC/SC カードリーダーで復号。mirakc には `tuners[].decoded: true` を設定
2. **mirakc のフィルタで復号**: `filters.decode-filter` に arib-b25-stream-test（libaribb25 系）等を指定

どちらでも mirakc の録画パイプラインと Web API（ライブ・records `/stream`）から出る TS は**復号済み**。したがって **Rokuban のシステム内に暗号化された TS は一切現れない**。B-CAS カード・カードリーダー（pcscd）・libaribb25 の用意は ISDBScanner と同様、エッジ環境構築時のセットアップ事項としてインストールドキュメントで扱う（アーキテクチャの構成要素ではない）。

ドキュメントでは正規の B-CAS カード + PC/SC リーダー構成のみを扱う（カードエミュレーション類は法的にグレーなため扱わない）。

### 6.2 scrambled カウントは復号障害の検出器

ingest のインラインドロップスキャンで数える scrambling_control ビットは、復号が正常なら常にゼロのはず。**scrambled > 0 は放送品質でなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味するので、ドロップ数とは別枠のアラート対象とする（EPGStation ドロップログの scramble 列と同じ役割）。

---

