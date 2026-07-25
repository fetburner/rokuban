# 録画エンジン: mirakc への委譲と予約同期

録画の実行は [mirakc](https://github.com/mirakc/mirakc) に全面的に委譲する。Rokuban はルールエンジンと予約の宣言的同期に徹する。

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [data.md](data.md)（データ層）/ [storage.md](storage.md)（メディアストレージ）/ [operations.md](operations.md)（運用）

---

## 1. 方針: mirakc への全面委譲

**「録画（リアルタイム・ハードウェア依存）はエッジの mirakc に、それ以外はサーバー側で弾力的に」**

mirakc に録画を委譲することで、Rokuban のサーバー側に残るのは録画の **コントロールプレーン** --- ルール評価、予約（desired state）生成、mirakc への宣言的同期 --- のみとなる。k8s コントローラと同型のレベルトリガーループで動作する。

重要な帰結として、**Rokuban は TS のストリーム処理（録画・demux・変換）を一切行わない**。TS を変更・解釈する処理は持たないが、**ingest 中の読み取り専用の統計採取は例外**とする（後述の ingest パイプラインを参照）。

---

## 2. mirakc に任せること

mirakc の録画機能は分散システムのバックエンドとして非常に都合の良い性質を持つ:

### schedule API による予約

`POST /api/recording/schedules`（programId 単位）でスケジュールを登録する。スケジュールは `recording.basedir/schedules.json` に mirakc 側で永続化されるため、**Rokuban が落ちていても録画は走る**。

### チューナー調停

mirakc の優先度機構（`X-Mirakurun-Priority` 相当 + schedule options の `priority`）に一元化する。ライブ視聴・録画・（併用する場合の）KonomiTV 等が同じ mirakc に載っても、いざという時は録画が勝つ。

### PSI/SI 追従

EPG 上の時刻ではなく TS 内の PSI/SI（イベント ID）を監視して実際の放送開始・終了に追従する。延長・繰り下げは `recording.rescheduled` で通知され、追従不能時も理由付きの `recording.failed`（`need-rescheduling` / `removed-from-epg` 等）が飛ぶ。

### Records API

録画物は `GET /api/recording/records/{id}/stream` で HTTP 取り出し可能。エッジとサーバー間に共有ファイルシステムが不要になる。

### SSE 再送

`/events` SSE は接続時に既存全 record の `recording.record-saved` を再送する。watcher が落ちていた間のイベントを取りこぼしても、再接続すれば現状と突き合わせて回復できる（実質 at-least-once + 状態同期）。

---

## 3. Rokuban 側のコンポーネント

### 3.1 ruler（ルール評価 → 予約生成）

EPG 更新イベント（`epg.programs-updated`）を契機に、該当サービスの番組をルールと突き合わせ、`reservations`（desired state）を生成・更新する。手動予約も同じテーブルで source が違うだけ。

**ルールエンジンは Rokuban 側に置く**。mirakc はルールベース自動予約をスコープ外と明言しており（contrib にサンプルがある）、まさに「ルール = サーバー、録画エンジン = エッジ」という分割を想定した作りになっている。

#### 複数ルール解決

- **desired 予約は programId につき最大 1 つ**。複数ルールがマッチした場合、予約オプション（priority、エンコードプロファイル、保持ポリシー）は最高 priority のルールから採る。生成元ルールは複数記録してよい（トレーサビリティ用）
- **除外はルール単位ではなく番組単位のオーバーライド**（reservation の skip フラグ）。どのルール経由でマッチしていても一貫して除外される
- EPGStation#538（複数ルールにマッチした番組を除外できない）は、予約がルール単位で管理されていたために起きた不整合。Rokuban は programId ベースなので構造的に防げる

#### 重複排除（再放送スキップ）

- EPGStation#704 の教訓: 囲み文字（:heavy_multiplication_x::heavy_multiplication_x:等）を一律除去する正規化は「前編/後編」の区別まで消して誤判定する。**記号除去 + 完全一致ではなく、pg_trgm の類似度ベース**で設計する（閾値はルール単位で調整可能に）
- EPGStation#473 の要望通り、**手動オーバーライド**（この番組を重複扱いにする / しない）を予約・履歴に持たせ、ruler 評価時に参照する
- 判定に使った根拠（マッチした履歴、類似度）を予約に記録し、UI で「なぜスキップされたか」を説明可能にする

### 3.2 reconciler（宣言的同期）

`reservations`（desired）と `schedule_sync`（observed: `GET /api/recording/schedules` の観測結果）の差分を POST/DELETE で消す、レベルトリガーの宣言的同期ループ。

- **tags 対応付け**: mirakc schedule の `tags` に Rokuban の reservation id を埋め込む（例: `rokuban:reservation=1234`）。手動で mirakc に入れられた schedule との判別もタグで可能
- **contentPath 生成**: `recording.basedir` 相対パス必須。ファイル名テンプレートの展開もここで行う
- **冪等**: 何度落ちても再実行で収束する。時刻精度もプロセス生存性も要求されない

#### 大量削除サーキットブレーカー

予約は「ルール x EPG」から導出されるため、EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう。EPGStation#692（予約と録画が勝手に消える）はこの障害クラスの実例。

対策:

- **1 回の reconcile / ruler パスでの削除数に閾値**（例: 対象総数の 20% または絶対数 N）を設け、超えたら削除を実行せず停止してアラート。手動確認後に再開
- **不変条件: 録画済みデータ（media_assets）に至る自動削除経路は retention reconcile のみ**。EPG・予約側の状態変化から録画物の削除に到達するパスを作らない
- programId が EPG から消えた予約は即削除ではなく「orphaned」マークにして猶予を置く（mirakc 自身も removed-from-epg を理由付き failed として通知してくる）

### 3.3 watcher（SSE 購読・状態反映）

`/events` SSE を購読し、`recording.record-saved` で `recordingStatus: finished` になったら ingest ジョブを投入する。

#### 3 段構えの信頼性設計

1. `record-saved` は同一 record に複数回・順序保証なしで飛ぶ → **record id で冪等投入**（River unique job）
2. watcher ダウン中の取りこぼし → **SSE の接続時全 record 再送**で回復
3. SSE はあくまでヒント → **定期的な `GET /api/recording/records` 全量取得と DB の突き合わせ**（レベルトリガー）が真実

この 3 つでエンコード漏れは構造的に起きない。

#### 品質メタデータ記録

`recording.record-broken` / `recording.failed` イベントは構造化された品質シグナルとして record に紐づけて DB に記録する（「録画品質の実測」計画の入力）。

#### 開始遅延検出器

録画開始は mirakc に委譲済みで Rokuban 側から防ぐ手段はないが、EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）のような mirakc 側の未知の不具合への保険として、**「開始時刻を過ぎたのに recording.started が観測されない予約」を reconcile ループで検出してアラート**する。既存の品質メトリクス（recording.failed / record-broken / ドロップ統計）に加える。レベルトリガーの枠内で安価に実装できる。

---

## 4. 予約モデル: base / overrides 分離

### 4.1 設計根拠（EPGStation v2.10.0 の問題）

EPGStation v2.10.0 の運用で「ルール予約を除外設定したはずなのに適用されない」「いつの間にか除外設定が外れる」という現象を確認した。原因は構造的に 2 つ:

1. **除外がルール予約単位**のため、複数ルールがマッチしていると別ルールの予約が生きて録画される（EPGStation#538）
2. **除外フラグが導出状態（予約行）に保存されている**ため、EPG 更新でルーラーが予約を再生成するとフラグごと消える

「ユーザーの意図を、コントローラが再生成する行に書く」ことが根本原因。以下の設計は両方を構造的に潰す。

### 4.2 base / overrides の分離

reservations の行を 2 層に分ける:

- **base**: ruler が「ルール x EPG」から計算するフィールド群（priority / エンコードプロファイル / 保持ポリシー / ファイル名等）。**ruler だけが書く**
- **overrides**: ユーザーが個別予約で上書きしたフィールドのみを持つ jsonb（skip もこの一種 `{"skip": true}`）。**api（ユーザー操作）だけが書く**
- **effective = base + overrides**。reconciler が mirakc に同期し ingest/encode が参照するのは常に effective

ruler は EPG 更新のたびに base を丸ごと再計算してよい --- overrides に触らないので手動編集は構造的に上書きされない。3-way merge は不要。ruler（シングルトン）と api が別カラムを書くので競合もない。**ルール側の変更は上書きしていないフィールドにだけ自動伝播する**（ユーザーの直感と一致）。

UI: 上書き中のフィールドにマーカー表示 + フィールド単位/予約単位の「ルールに戻す」（override を消すだけ）。

### 4.3 ライフサイクル: detached・再アタッチ・GC

EPG の変化・ルール編集でルールがマッチしなくなったとき:

| 状態 | 挙動 |
|---|---|
| overrides なし | 削除（通常の宣言的動作） |
| **overrides あり（skip 含む）** | **削除せず detached 状態で保持**。skip 付きなら録画しない detached、skip 以外の overrides なら実質 manual として録画する |

重要: **skip のみでも削除しない**。削除すると「EPG の一時不整合で番組消失 → skip ごと行削除 → EPG 回復 → ruler が新規生成 → 除外が外れている」という EPGStation の症状 2 が EPG フリッカー経由で再発する。

- **再アタッチ**: ルールが再マッチしたら base を再計算して再アタッチ（overrides はそのまま）。EPG がちらついても除外は生き残る
- **GC**: detached 行の削除は「番組の終了時刻を過ぎた後」のみ。ユーザー意図の寿命を放送の寿命に揃える
- programId 一意性は detached 行にも適用され、再マッチ時の重複予約は構造的に生まれない
- ルール自体の削除も同じ規則（overrides なし → 削除 / あり → detached）。大量削除にはサーキットブレーカーが別途かかる

除外が外れるのは、ユーザーが自分で「ルールに戻す」を押したときだけになる。

### 4.4 manual 予約との統一

manual 予約は「base を持たず全フィールドが overrides」の縮退形。同一テーブル・同一コードパスで扱う。複数ルールマッチ時（最高 priority ルールが base を供給）も、勝者が入れ替われば base が変わるだけで overrides は生存する。

### 4.5 録画開始後の編集

- priority の変更は録画中の mirakc recorder には効かない可能性が高い
- エンコードプロファイル・保持ポリシーの変更は ingest 時に評価されるので効く
- UI で「開始後に意味を持つフィールド」を区別表示する

### 4.6 スコープ外

「この番組シリーズは常に...」のような永続的例外は overrides（予約の寿命 = 短命）ではなくルール側の機能。必要になったら別途検討。

---

## 5. ingest パイプライン

録画完了後、mirakc のエッジから Rokuban のアーカイブストレージへ録画データを取り込む一連の処理。

### 5.1 転送方式: API pull 固定

`records/{id}/stream` による HTTP pull を全構成で統一する。「monolith モードなら mirakc の basedir を直接読めるのでは」を検討し、**HTTP loopback 経由を維持**と結論した:

- **ディスク I/O は直読みでも減らない**。basedir（リングバッファ）→ メディアストレージのコピー自体は必要で、節約できるのは loopback TCP のオーバーヘッドだけ。1 日数本・数十 GB では無視できる
- **コピー自体が耐障害設計**。録画はシステム内で唯一のリアルタイム・リトライ不能な操作なので、ローカルディスクへ録画 → 完了後にリトライ可能な転送、という分離は崩さない（mirakc に最終保存先へ直接書かせる案は、録画中の NAS/FUSE ストールが放送の欠損に直結するため不採用）。ドロップスキャンも転送パスがあるからタダで載る
- **コードパスが 1 本**。HTTP pull は monolith / 分散 / ハイブリッドの全構成で動く唯一の方法
- **所有権が明確**。basedir は mirakc の所有物で、Rokuban は API 越しの客に徹する

「同居時の basedir 直読み」は loopback が実測でボトルネックになった時の最適化オプション（YAGNI）。契約は **mirakc とは常に API、自身のストレージとは常にファイルシステム**の 2 面で固定（[storage.md](storage.md) 参照）。

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

切断時は書き込み済みオフセットから `Range: bytes=N-` で再接続して追記。ドロップスキャンのカウンタはメモリ上に生きているので継続できる。タイムアウトは総時間ではなく**ストール検知**（N 秒間無進捗で切断扱い）--- 総時間タイムアウトは遅い回線の正常な転送を殺す。

#### 層 2: ジョブ再試行（プロセス死）

River の at-least-once + 指数バックオフ。**ゼロから作り直す**（部分ファイルは truncate）。中途再開はスキャナ状態の永続化とストレージ契約（シーケンシャル一発書き）違反の追記が必要になり、層 1 で大半が救われる以上、複雑さに見合わない。

#### 層 3: 完全性検証とコミット

pull 完了後に書き込みバイト数を HEAD の Content-Length と照合 → 一致で `media_assets` コミット（コミット = DB 行。部分ファイルは孤児として cleanup が回収）→ **mirakc 側の record 削除はコミット後のみ**。どこで落ちても最悪「もう一度 pull」で、データ喪失は構造的に起きない。

運用上の唯一のリスクは**長時間の転送失敗でエッジのリングバッファが溜まり続ける**こと。ジョブは諦めず再試行し続け（max attempts で dead-letter にすると record が宙に浮く）、「未 ingest の record 総量」をメトリクス化してエッジのディスク残量と突き合わせてアラートする（[storage.md](storage.md) のサイジング指針参照）。

### 5.4 負荷分担: worker

`records/{id}/stream` の負荷が乗るのは worker（ingest ジョブ、KEDA で 0〜N）であり、reconciler は数百件のメタデータ diff を回すだけの軽量シングルトンのまま。ただし**本当のボトルネックはクラウド側ではなくエッジ側**:

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

## 7. mirakc schedule options

```json
{
  "programId": 327360102415397,
  "options": {
    "contentPath": "videos/path/to/file.m2ts",
    "priority": 1,
    "preFilters": [],
    "postFilters": [],
    "logFilter": "info"
  },
  "tags": ["rokuban:reservation=1234"]
}
```

- `contentPath`: `recording.basedir` からの相対パス必須（絶対パス・`../`・basedir 外はバリデーションで 4xx）。`recording.records-dir` 設定時は省略可（自動生成名）
- `priority`: チューナー使用優先度（競合時の調停）
- `preFilters` / `postFilters`: config で定義した名前付きフィルタパイプライン
- `logFilter`: mirakc-arib のログをコンテンツ横の `<ファイル名>.log` に出力（EPGStation が自前生成していたドロップログの代替。表示用のパース層は必要）
- `tags`: 自由文字列。Rokuban の reservation id 埋め込みに使う
- **開始/終了マージンのオプションは存在しない**。PSI/SI 追従方式ではそもそも不要（時刻ベース録画だからこそマージンが必要だった）

---

## 8. 録画品質の実測

mirakc の追従品質は EDCB ほどの長期実績がないため、以下をメトリクス化して録画失敗・欠損を継続計測する:

- `recording.failed` の理由別集計
- `recording.record-broken` の記録
- ingest 時のドロップ統計（PID 別 continuity counter 不連続 / TEI）
- scrambled カウンタ（B-CAS 障害検出）
- 開始遅延検出器（開始時刻超過 + recording.started 未観測）

品質問題が実測されたら、その時点でエンジン追加を再検討する。

---

## 9. 落とした機能・スコープ外

### 時刻指定予約

mirakc の予約 API は programId ベースのみで、「サービス X を 21:00〜22:00」を表現できない。用途（変則開始時刻の番組をチューナーのやりくりで録る等）は mirakc の優先度調停 + 番組追従でほぼ吸収されるため、**機能ごと落とす**。撮り逃しは再放送を待つ運用。

### イベントリレー追従

mirakc 利用時は現行 EPGStation でも効いていないため、委譲による退行はない。番組追従自体は mirakc が TS ベースで行うぶん改善方向。

### EDCB ドライバ

EDCB（Linux ネイティブビルド可、番組追従の実績は最強）を録画エンジンの選択肢として検討したが、Windows ネイティブ由来のソフトであること、CtrlCmd（バイナリ TCP）+ Lua HTTP という API、録画物がローカルファイルでエッジ転送エージェントが必要になることから**採用しない**。

録画エンジンの抽象化レイヤーも作らない（YAGNI。mirakc API の呼び出しが reconciler / watcher に局所化されること自体が十分な継ぎ目）。

### 「この番組シリーズは常に...」のような永続的例外

overrides（予約の寿命 = 短命）ではなくルール側の機能。必要になったら別途検討。
