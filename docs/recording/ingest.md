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

`internal/tsstat/integration_test.go` が既知の良品に対する差分テストを持つ
（issue #6 の差分テスト戦略）。`ROKUBAN_TEST_TS_FILE` に別実装で drop / error /
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

pull 完了後に書き込みバイト数を HEAD の Content-Length と照合 → 一致で `media_assets` コミット（コミット = DB 行。部分ファイルは孤児として cleanup が回収）→ **mirakc 側の record 削除はコミット後のみ**。どこで落ちても最悪「もう一度 pull」で、データ喪失は構造的に起きない。

運用上の唯一のリスクは**長時間の転送失敗でエッジのリングバッファが溜まり続ける**こと。ジョブは諦めず再試行し続け（max attempts で dead-letter にすると record が宙に浮く）、「未 ingest の record 総量」をメトリクス化してエッジのディスク残量と突き合わせてアラートする（[storage.md](../storage.md) のサイジング指針参照）。

#### 冪等性: コミット済みなら転送をやり直さない

`media_assets` に `kind='original'` の行が既にコミットされていれば、ジョブは転送を行わず、エッジ record の削除だけを再試行して終わる（`IngestWorker.hasOriginalMediaAsset`）。エッジ record の削除は失敗してもログのみで ingest 自体は成功扱いにしているため、mirakc 側に record が残ったまま record_sweep 経由で同じ record の ingest ジョブが再投入されうる。ここで止めないと `os.Create` がコミット済みファイルを 0 バイトに切り詰めて全量を再ダウンロードし、streamer が不変条件 3（コミット = DB 行）に反して欠けたファイルを配ることになる。

### 5.4 負荷分担: worker

`records/{id}/stream` の負荷が乗るのは worker（ingest ジョブ、KEDA で 0〜N）であり、reconciler は数百件のメタデータ diff を回すだけの軽いジョブのまま。ただし**本当のボトルネックはクラウド側ではなくエッジ側**:

- ハイブリッド構成では自宅アップリンク帯域が律速。worker を増やしても速くならない
- エッジでは録画中の書き込みと pull の読み出しが同じディスクで競合する。pull がディスクを飽和させて録画をドロップさせるのは本末転倒

→ **ingest の同時実行数は mirakc サイト単位で少数（1〜2）にキャップ**する（サイト別キュー or River の同時実行数設定）。worker の水平スケールが効くのは encode（CPU バウンド、入力はクラウド側ストレージ）の方。

### 5.5 ingest 完了後のフロー

ingest 完了時に**同一トランザクションで**エンコードジョブを River に投入。信頼性は River の at-least-once + 冪等性で担保される。

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

