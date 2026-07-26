# 録画エンジン: mirakc への委譲と予約同期

録画の実行は [mirakc](https://github.com/mirakc/mirakc) に全面的に委譲する。Rokuban はルールエンジンと予約の宣言的同期に徹する。

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [data.md](data.md)（データ層）/ [storage.md](storage.md)（メディアストレージ）/ [operations.md](operations.md)（運用）

---

## 1. 方針: mirakc への全面委譲

**「録画（リアルタイム・ハードウェア依存）はエッジの mirakc に、それ以外はサーバー側で弾力的に」**

mirakc に録画を委譲することで、Rokuban のサーバー側に残るのは録画の **コントロールプレーン** --- ルール評価、予約（desired state）生成、mirakc への宣言的同期 --- のみとなる。k8s コントローラと同型のレベルトリガーループで動作する。

重要な帰結として、**Rokuban は TS のストリーム処理（録画・demux・変換）を一切行わない**。TS を変更・解釈する処理は持たないが、**ingest 中の読み取り専用の統計採取は例外**とする（後述の ingest パイプラインを参照）。

### 例外の境界（M2 で PAT / PMT まで拡張）

ドロップ統計に PID の種別（映像 / 音声）を出すため、例外を PSI の読み取りまで広げる。PID 数値だけの統計では「映像が壊れたのか EIT が数回落ちただけか」を区別できず運用判断に使えない（issue #23）。ただし境界を条文として固定する。

| 許す | 許さない |
|---|---|
| PAT / PMT のセクション再構成 | **記述子を一切読まない**（`component_tag` も含む） |
| ES ループの `elementary_PID` と `stream_type` の読み取り | 他テーブル（EIT / SDT / NIT / TOT）の解析 |
| 固定 PID の静的表による命名（PAT / CAT / NIT / SDT / EIT / TOT。解析不要） | ES ペイロードの解釈（字幕デコード・映像解析） |
| PID → 種別の対応を統計メタデータとして記録 | PSI を根拠に EPG プロジェクションを補正すること |
| — | TS の書き出し・変換・demux（この不変条件の本体） |

**「記述子を読まない」が歯止めの本体。** 機械的に判定できる（`Descriptors()` を呼んでいたらレビューで落ちる）し、EIT の解析も自動的に排除される（EIT の中身は記述子）。

**字幕と文字スーパーは区別しない。** ARIB では両者とも `stream_type = 0x06`（private PES）で、区別には `component_tag`（記述子タグ 0x52）が必要になる。守りたいのは映像と音声なので、`stream_type` だけで足りる分類にとどめる。この割り切りにより **ARIB 固有の知識がコードベースに一切入らない**（`stream_type` は ISO/IEC 13818-1 の標準値）。既存依存の gots が `IsVideoContent()` / `IsAudioContent()` を含めて必要なものを全て持っており、自前実装はセクションの取り回しだけになる。

**PSI 解析の失敗は ingest の失敗にしない。** 種別が不明なら PID 数値のまま表示するだけで、ドロップ統計自体は成立する。壊れた PMT で成功するはずの転送を落とさない。

PMT は録画中に更新されうる（version 更新、番組の境目での PID 再割り当て）。同一 PID の分類が途中で変わった場合は**最後に見たものを採用し、変化を検出したことを記録する**。

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

EPG プロジェクションの番組をルールと突き合わせ、`reservations`（desired state）の base を生成・更新する。手動予約も同じテーブルで source が違うだけ。

**ルールエンジンは Rokuban 側に置く**。mirakc はルールベース自動予約をスコープ外と明言しており（contrib にサンプルがある）、まさに「ルール = サーバー、録画エンジン = エッジ」という分割を想定した作りになっている。

#### 評価は全量、書き込みは差分

**1 パスで全ルール x 全射影番組を評価する。差分駆動にしない。**

desired 予約が変わる契機は 3 つあり、EPG の変更はそのうち 1 つでしかない。

| 契機 | 差分駆動で拾えるか |
|---|---|
| EPG の番組が変わった | 拾える（変更行の検出が必要） |
| ユーザーがルールを編集した | 拾えない。EPG は変わっていない |
| 時間が経過して番組がウィンドウに入った | 拾えない |

差分駆動にすると契機ごとに経路が分かれ、取りこぼしが独立したバグになる。全量評価なら経路は 1 本で、どの契機でも次のパスで収束する（レベルトリガー）。

加えて全量評価でないと成立しないものが 2 つある。

- **大量削除サーキットブレーカー**（後述）の「削除数が総数の N% 超過」は、desired 集合の全体が 1 パスで手に入らないと分母が定義できない
- **競合判定は集合の性質**。1 ルールの予約が増減すると他の予約の競合状態が変わるので、部分評価と相性が悪い（EPGStation の `updateRule` が `findTimeRanges` で影響範囲の予約を引き直しているのは同じ理由）

規模は問題にならない。EPG プロジェクションはローリングウィンドウで永久に有界（[データ層](data.md) 参照）で、実測で 19 サービス x 8 日 = 2680 行。数百ルールとの突き合わせは pg_trgm GIN 込みで秒未満。評価は Postgres の集合演算で行うので、**予約 1 件ずつのループにはしない**。

**ただし書き込みは差分にする。** `reservations` には SSE 用の行トリガーがあるため、毎パス全予約の base を書き直すと NOTIFY が全行 x 毎パス飛び、クライアントの invalidate が鳴り続ける。base が実際に変わった行だけ UPDATE する（`WHERE base IS DISTINCT FROM ...`）。churn と bloat も避けられ、`updated_at` が「base が実際に変わった時刻」という意味を保つ。

起動契機は 3 つあるが、**定期パスが真実**で残り 2 つは投入を早めるヒントに過ぎない。ヒントを落としても定期パスが拾う。

| 契機 | 種別 |
|---|---|
| 定期（既定 10 分） | **真実**。デプロイ形態に応じて River `PeriodicJobs` か k8s CronJob が投入する（[データ層](data.md) §2） |
| ルールの作成 / 更新 / 削除 | ヒント。api がルール書き込みと**同一トランザクションで** `InsertTx` する（dual-write にならない） |
| EPG 同期の完了 | ヒント。epg_sync ワーカーがパス完了時に投入する |

ヒントは `UniqueOpts{ByArgs, ByState}` で合流する。ルールを連続で編集してもパスは 1 回で足りる。

##### EPGStation の実績

EPGStation も全量評価であり、**差分駆動を実装した上で無効化している**（`EPGUpdater.ts`）。

```ts
this.updateManage.on(EPGUpdateEvent.PROGRAM_UPDATED, () => {
    // NOTE this.config.epgUpdateIntervalTime の周期で予約情報を更新させるため無効化
    // this.notify();
});
```

EPG 更新完了で `reservationManage.updateAll()` を呼び、全手動予約と全ルールを評価する。ルール編集時のみ `updateRule(ruleId)` の部分評価を持つが、正しさを担保しているのは周期的な全量パスの側。

真似しない点が 3 つある。いずれも「予約 1 件ずつ TypeScript で処理する」ことから来ている。

- ループの各要素間の `Util.sleep(10)`（ルール 100 個 + 手動予約 100 件で sleep だけで 2 秒）
- 全量更新のログが多すぎて必要になった `isSuppressReservesUpdateAllLog` 設定
- 手動予約の追加が全量更新に割り込むための優先度付き実行権

#### 複数ルール解決

- **desired 予約は programId につき最大 1 つ**。複数ルールがマッチした場合、予約オプション（priority、エンコードプロファイル、保持ポリシー）は最高 priority のルールから採る。生成元ルールは `reservation_rule_matches` に全件記録する（トレーサビリティ。issue #3 のスキーマ決定コメント）
- **勝者決定は全順序**: `ORDER BY priority DESC, id ASC`（同率なら先に作られたルールが勝つ）。同率タイを不定のままにすると、全量パスごとに勝者が入れ替わって base の差分書き込みが発火し続け、mirakc に更新 API がないため reconciler が schedule を DELETE + POST で作り直し続けるフラッピングになる。差分書き込みは勝者決定の決定性を前提として要求する。新しい方でなく古い方を勝たせるのは、同率の新ルール追加が既存予約の base を動かさないため（勝たせたければ priority を上げる — 暗黙の新旧より明示的な優先度操作）
- **除外はルール単位ではなく番組単位のオーバーライド**（reservation の skip フラグ）。どのルール経由でマッチしていても一貫して除外される
- EPGStation#538（複数ルールにマッチした番組を除外できない）は、予約がルール単位で管理されていたために起きた不整合。Rokuban は programId ベースなので構造的に防げる

#### 重複排除（再放送スキップ）

- EPGStation#704 の教訓: 囲み文字（:heavy_multiplication_x::heavy_multiplication_x:等）を一律除去する正規化は「前編/後編」の区別まで消して誤判定する。**記号除去 + 完全一致ではなく、pg_trgm の類似度ベース**で設計する（閾値はルール単位で調整可能に）
- EPGStation#473 の要望通り、**手動オーバーライド**（この番組を重複扱いにする / しない）を予約・履歴に持たせ、ruler 評価時に参照する
- 判定に使った根拠（マッチした履歴、類似度）を予約に記録し、UI で「なぜスキップされたか」を説明可能にする

#### サイトの扱い

**ルールはサイトに従属しない**グローバルな永続資産で、rules に site 列はない。サイトは条件の一次元であり、`rule_sites` 子テーブル（指定なし = 全サイト。他の条件テーブルと同じ規約）で絞り込む。site 値は書き込み時に設定のサイトレジストリと照合し、FK は張らない（レジストリは設定ファイルにある — 外部に真実があるものは存在を制約できない）。

**実体化はマッチした全サイトで予約を作る（N 予約が既定）**。同一放送は全サイトで同一 programId を持つ（Mirakurun の ID 合成）ため `(site, program_id)` ごとに予約行ができ、全部録る。これは意図された既定値: 複数録画してドロップ統計で選別するワークフローを一級として扱い、サイト選好・自動フェイルオーバーという機構を持たない。チューナー競合は mirakc の調停に任せ、負けは `need-rescheduling` として観測される（競合の可視化と開始遅延検出器がこの運用を支える）。絞りたければ `rule_sites` が唯一の機構。

NID/SID は放送規格のスコープでサイトに依存しないため、地上波の条件は地域が違えば構造的にマッチしない（NID が違う）。全サイトマッチが実際に効くのは BS/CS と同一地域の複数サイトのみ。

サイト名は安定識別子として扱い、オンラインのリネームはサポートしない（SQL 付け替えを伴う運用作業）。無断リネームは旧サイトの射影が stale になり導出削除として現れるため、サーキットブレーカーが受け止める。

### 3.2 reconciler（宣言的同期）

`reservations`（desired）と `schedule_sync`（observed: `GET /api/recording/schedules` の観測結果）の差分を POST/DELETE で消す、レベルトリガーの宣言的同期ループ。

- **tags 対応付け**: mirakc schedule の `tags` に Rokuban の reservation id を埋め込む（例: `rokuban:reservation=1234`）。手動で mirakc に入れられた schedule との判別もタグで可能
- **contentPath 生成**: `recording.basedir` 相対パス必須。ファイル名テンプレートの展開もここで行う。生成値は初回のみで、以後は base に固定する（後述の差分反映）
- **冪等**: 何度落ちても再実行で収束する。時刻精度もプロセス生存性も要求されない

#### ファイル名テンプレート

`filenameTemplate`（予約オプション。§8）は Go の [`text/template`](https://pkg.go.dev/text/template) 記法。reconciler が予約行のスナップショットだけを使って展開し（`internal/contentpath` パッケージ。`internal/reconciler/contentpath.go` の `buildContentPath` から呼ばれる）、拡張子は含まない前提で常に `.m2ts` を付す。未指定・空文字なら従来どおりの固定形式（`YYYYMMDD/HHMMSS_タイトル_サービスID.m2ts`）のまま（後方互換）。

**方針転換の理由**: 当初は EPGStation 互換の `%変数%` 記法で実装していたが、`text/template` に切り替えた。`%変数%` では変数名の誤り（`%TITEL%`）が黙って空文字になり、録画時に警告ログが出るだけで、ユーザーは数週間後にファイル名が崩れて初めて気づく。`text/template` なら**ルール作成/更新時にテンプレートを検証して 400 で弾ける**（`internal/api/rules.go` の `validateRuleInput` が `internal/contentpath.Validate` を呼ぶ。既存の正規表現検証と同じ場所・同じ形）。「未対応の変数は黙って空文字に置換して警告」という妥協した方針そのものが不要になった。

##### 使えるフィールド

`internal/contentpath.Data` の公開フィールドに対応する。

| フィールド | 値 | 出所 |
|---|---|---|
| `{{.StartAt}}` | 番組開始時刻（JST の `time.Time`）。`{{.StartAt.Format "2006-01"}}` のように任意の書式を書ける | `program_start_at` |
| `{{.Year}}` | 4 桁年（JST） | 同 |
| `{{.ShortYear}}` | 2 桁年（JST） | 同 |
| `{{.Month}}` `{{.Day}}` `{{.Hour}}` `{{.Min}}` `{{.Sec}}` | 2 桁ゼロ埋め（JST） | 同 |
| `{{.DOW}}` | 曜日（`日`〜`土`） | 同 |
| `{{.Title}}` | 番組名（パス成分としてサニタイズ済み） | `reservations.title` |
| `{{.Channel}}` | 物理チャンネル（同上） | `reservations.channel` |
| `{{.ServiceID}}` | サービス ID | `reservations.service_id` |
| `{{.ChannelType}}` | チャンネル種別（同上） | `reservations.channel_type` |

例:

```
{{.Year}}/{{.Month}}/{{.Title}}_{{.Hour}}{{.Min}}
```

**非対応**: チャンネル名（EPGStation の `%CHNAME%` 相当）/ mirakc 内部 ID（`%CHID%` 相当）/ EPGStation の番組 ID（`%ID%` 相当）。いずれも予約行のスナップショットだけからは解決できず、mirakc への問い合わせや EPG プロジェクションの JOIN が要る。reconciler は mirakc に触れず（不変条件 1）ファイル I/O 専任という設計に反するため対応しない。`Data` に存在しないフィールドを参照するとテンプレートは無効になり、ルール作成/更新時点で 400 になる（後述）。

##### サニタイズと階層の規約

- `Title` / `Channel` / `ChannelType` は `internal/contentpath.NewData` の時点で `sanitizeComponent` を通した「1 パス成分に収まる」文字列になっている（ただし空文字は空文字のまま）。番組名に `/` が普通に入る（「A/B」等）ため、データ由来の `/` が区切りに昇格することはない
- **階層を作れるのはテンプレートに書かれた `/`（および `{{.StartAt.Format "2006/01"}}` のようにユーザーが明示的に書いた書式）だけ**
- **拡張子はテンプレートに含めない**。常に `.m2ts` を付す
- 展開結果は最後に必ず `internal/contentpath.SanitizeContentPath` を通すため、テンプレート自体に `..` や絶対パスが書かれていてもパストラバーサル・意図しない絶対パスにはならない
- 時刻は必ず JST で解決する（サーバーのタイムゾーン設定に依存させない）

##### ルール作成時の検証

`text/template` として `Parse` した後、サンプルデータに対して `Execute` まで行って初めて有効と判定する（`{{.Foo}}` のような未知フィールドは `Parse` では素通りし、`Execute` で初めてエラーになるため）。構文エラー・未知フィールドはどちらもルール作成/更新 API で 400 になる。

##### M3: EPGStation からの変換（`rokuban import epgstation`）

EPGStation の `recordedFormat`（`%変数%` 記法）を移行する際は、M3 の `rokuban import epgstation` で以下の変換表に従って `text/template` 記法へ機械的に変換する。

| EPGStation | Rokuban |
|---|---|
| `%YEAR%` | `{{.Year}}` |
| `%SHORTYEAR%` | `{{.ShortYear}}` |
| `%MONTH%` | `{{.Month}}` |
| `%DAY%` | `{{.Day}}` |
| `%HOUR%` | `{{.Hour}}` |
| `%MIN%` | `{{.Min}}` |
| `%SEC%` | `{{.Sec}}` |
| `%DOW%` | `{{.DOW}}` |
| `%TITLE%` | `{{.Title}}` |
| `%CH%` | `{{.Channel}}` |
| `%SID%` | `{{.ServiceID}}` |
| `%TYPE%` | `{{.ChannelType}}` |
| `%CHNAME%` / `%CHID%` / `%ID%` | **未対応**（予約行のスナップショットだけからは解決できない。上記「非対応」参照） |

#### 予約オプションの差分反映

reconciler は存在の突き合わせだけでなく、**effective options と `schedule_sync.options`（mirakc の観測結果）の差分も消す**。ruler が base を毎パス再計算する以上、ルール編集で既存予約の effective が変わるため、これは編集 UI の前提ではなく **ruler の前提**である（issue #19）。

**mirakc に schedule の更新 API はない**（`GET` / `POST` / `GET{id}` / `DELETE{id}` の 4 つだけ）。反映は DELETE → POST の再作成になり、その間 schedule が存在しない窓ができる。そのため差分の対象を最小化する。

| フィールド | 差分対象 | 理由 |
|---|---|---|
| `priority` | **する** | チューナー調停の優先度。ルール編集・overrides 編集の実質的な唯一の変更対象 |
| `contentPath` | **しない**（base に固定） | 下記 |
| `preFilters` / `postFilters` | M3 から | M1/M2 では常に空 |
| `logFilter` / `tags` | しない | 前者は未使用、後者は reservation id で不変 |

**`contentPath` は初回生成値を base に固定し、以後変更しない。** reconciler は番組名からパスを生成するため、EPG の番組名が変われば生成結果も変わる。これを差分と見なすと **EPG 更新のたびに schedule が消えて作り直される** churn になる。差分書き込みという設計は desired が安定していることを前提として要求する（同率 priority のタイを全順序で潰したのと同じクラスの問題。§3.1）。ファイル名を変えたい場合はユーザーが overrides で明示的に指定する。

再作成のガードは時刻の閾値ではなく状態で判定する: **`schedule_sync.state` が `tracking` / `recording` の予約は触らず、次のパスに持ち越す**。DELETE 成功 → POST 失敗で schedule が消えた場合もレベルトリガーで次パスが再作成するが、その間に開始時刻を越えると取りこぼすため `quality_events` に記録する。

差分対象が `priority` だけであることの帰結として、**再作成が走るのは「ユーザーが優先度を変えたとき」だけ**になる。EPG 更新のたびに走る処理ではないので、この単純なガードで足りる。

**upstream への要望**: `priority` の部分更新 API があれば再作成の窓ごと消える。`RecordingOptions` 全体を差し替える汎用 PUT より通りやすく、mirakc 側が触る内部状態（スケジューラのキュー）も小さい。priority は開始前の schedule に対して mirakc のスケジューラが素直に扱える性質のフィールドでもある（§4.5 のとおり録画開始後は効かない）。#8 に調査メモとして残す。

#### 大量削除サーキットブレーカー

予約は「ルール x EPG」から導出されるため、EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう。EPGStation#692（予約と録画が勝手に消える）はこの障害クラスの実例。

対策:

- **1 回の reconcile / ruler パスでの削除数に閾値**（例: 対象総数の 20% または絶対数 N）を設け、超えたら削除を実行せず停止してアラート。手動確認後に再開
- **ブレーカーが守るのは導出された削除だけ**（ルール x EPG の評価結果の変化）。ユーザーの明示操作（ルール削除 API 等）による削除は対象にしない — 代わりに影響件数の内訳を提示する確認 UI が安全装置になる。明示操作までブレーカーで止めると「削除したのに消えない」という別の説明不能を生む。API が同期的に消した予約行を reconciler が mirakc へ伝搬する際の削除が閾値を踏みうる相互作用は M2-5 の実装論点（issue #24）
- **不変条件: 録画済みデータ（media_assets）に至る自動削除経路は retention reconcile のみ**。EPG・予約側の状態変化から録画物の削除に到達するパスを作らない
- programId が EPG から消えた予約は即削除せず猶予を置く（mirakc 自身も removed-from-epg を理由付き failed として通知してくる）。なお実装の `orphaned` state はこの用途ではなく「番組終了後に schedule が観測されなかった」を意味する（[schema.md](schema.md) §3）

#### 番組終了後の GC

`reservations` / `program_intents` の物理削除（GC）は ruler の 1 パス内で、全サイト評価の後に 1 回だけ行う（`internal/ruler/ruler.go` の `runGC`）。対象は `program_start_at + program_duration_ms < now() - 猶予` を満たす行（state を問わない）。猶予には既存の `epg.retention_grace`（既定 24h、EPG プロジェクションのローリングウィンドウと同じ設定）をそのまま流用する。専用の設定項目を増やさず、「EPG から消える」と「予約・意図として GC される」の寿命を揃える。`recordings.reservation_id` は `ON DELETE SET NULL` なので、この削除で録画履歴（recordings/media_assets）が失われることはない。

**GC は大量削除サーキットブレーカー（`MaxDeletesPerPass`）の対象にしない。** ブレーカーが守るのは「ルール x EPG」の評価結果から導出される削除だけで、EPG の一時的な欠損・フリッカーに引きずられて予約を大量に消してしまう事故（上記 EPGStation#692 のクラス）を防ぐためのもの。GC の削除対象は時刻の比較だけで決定的に定まり、EPG の状態には一切左右されない。むしろ reconciler/ruler が長時間停止していた場合、再開後に溜まった期限切れ行を一括で消すのは正常な挙動であり、ここをブレーカーで止めると実害のない削除が積み上がり続けるだけになる。

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

- **base**: ruler が「ルール x EPG」から計算するフィールド群（priority / エンコードプロファイル / 保持ポリシー / ファイル名等）。`reservations.base` に載り、**ruler だけが書く**
- **overrides**: ユーザーが上書きしたフィールドのみを持つ疎な jsonb。**`program_intents` 表に載り、api（ユーザー操作）だけが書く**
- **effective = base + overrides**。reconciler が mirakc に同期し ingest/encode が参照するのは常に effective

**意図（overrides）は導出行（reservations）とは別の表に置く。** ruler が base だけを書く規律でも上書きは守れるが、1 行が「ユーザー意図の永続記録」と「ruler の導出結果」を兼ねると、昇格・取消の分岐・削除の例外という 3 つの複雑さが派生する（issue #18 の案 A）。表を分けると:

| 1 表だったときに必要だったもの | 意図を分けた後 |
|---|---|
| 昇格（manual 行にルールがマッチしたときの effective 保存と `skip:false` の焼き付け） | **不要**。意図は据え置きで `rule_id` と base が変わるだけ |
| 取消の分岐（再生成者がいるか） | **不要**。無条件に `intent{skip}` を書いて導出行を落とす |
| 削除の例外（「overrides があれば消さず detached」） | **不要**。意図は別表なので導出行を消しても失われない |

`skip` は overrides のキーではなく **`program_intents.action`（`record` / `skip`）** という列にする。列なので base 側の skip に対する優先順位が明示的に決まり（`effective.skip = (action = 'skip') OR (意図がなく base.skip)`）、jsonb マージに細工を仕込まなくてよい。M2-6 の重複排除が base に skip を立てても、ユーザーの `record` 意図が勝つ。

意図の寿命は放送の寿命に揃える（番組終了後に GC）。

ruler は EPG 更新のたびに base を丸ごと再計算してよい --- **overrides は別表（`program_intents`）にあるので構造的に触れない**。3-way merge は不要。ruler は `reservations` を、api は `program_intents` を書くので競合もない（ruler のパスはサイト単位で排他。[データ層](data.md) §2）。**ルール側の変更は上書きしていないフィールドにだけ自動伝播する**（ユーザーの直感と一致）。

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
- ルール自体の削除も同じ規則（**意図なし → 削除 / 意図あり → 残す**）。**ユーザーが個別に編集した予約は、ルールを消しても手動予約と同等に生き残る**。意図は番組単位のユーザーの投資であり、ルール削除とは別の意図。録画ドメインでは録り逃しが不可逆で余計な録画は消せば済むため、迷ったら録る側に倒す
- **ルール削除の UX は可視化で解決する**: 削除 API は内訳（予約 N 件を削除、M 件は編集済みのため detached 化）を返し、UI は確認ダイアログとトーストに出す。detached 行は予約一覧にマーカー付きで現れ、個別に削除できる（§4.4 の取消分岐）。残る行は定義上「ユーザーが触ったものだけ」なので件数は常に少なく、1 件ずつ説明可能

除外が外れるのは、ユーザーが自分で「ルールに戻す」を押したときだけになる。

### 4.4 manual 予約との統一

manual 予約は「base を持たず、`program_intents` に `action = 'record'` と上書きだけがある」縮退形。ルール由来予約と同一コードパスで扱う。複数ルールマッチ時（最高 priority ルールが base を供給）も、勝者が入れ替われば base が変わるだけで意図は生存する。

source / state は「今、誰が base を供給しているか」の答えに過ぎない: `manual` = 誰もいない（base = NULL）/ `rule` = ルールが毎パス再計算 / `detached` = かつてのルール（凍結された base）。

#### manual 行にルールがマッチしても昇格は要らない

意図が別表にあるので、手動予約済みの番組にルールがマッチしたとき ruler がやることは **`rule_id` と base を埋めるだけ**。`program_intents` には触らないので、ユーザーの上書きは定義上失われない。effective を保存するための細工（`skip:false` の焼き付け等）は不要になる。

ルールがマッチしなくなったら base は凍結され（ruler は上書きしない）、`rule_id` が外れて実質 manual として動く。意図は無関係に生き続ける。

逆方向（ルール予約済みの番組への手動予約）は行が既にあるので「予約済み」を返すだけ。

manual 予約を「その番組 1 つにマッチする自動生成ルール」として表現する統一はしない。rules がワンショットの行で埋まり、ユーザーが書いた永続資産と導出される短命な行の寿命が混ざる。

#### 取消は無条件に「意図を残して導出行を落とす」

`intent{skip}` を書き、`reservations` の行を削除する。**行の状態による分岐はない。**

行を消すだけにしてはならない。**消された行と最初から無かった行は ruler から区別できない**（DELETE は「録画するな」という負の意図ごと情報を破壊する）ため、次の全量パスが復活させてしまう。意図が別表に残るので、勝者ルールが入れ替わっても・全ルールがマッチしなくなっても・再アタッチされても、除外は一貫して守られる。

意図そのものを捨てたい（「この番組についての指定をなかったことにする」）場合は `program_intents` の行を消す。ルールがマッチしていればその後の全量パスで普通のルール予約として作り直される。

ルール予約の取消を DELETE にしてはならない。**消された行と最初から無かった行は ruler から区別できない**（DELETE は「録画するな」という負の意図ごと情報を破壊する）ため、次の全量パスが復活させてしまう。skip override なら行そのものが「録画するな」の永続的な記録として生き続け、勝者ルールが入れ替わっても・全ルールがマッチしなくなって detached になっても・再アタッチされても、除外は一貫して守られる。行を自分自身の tombstone として使う形。

昇格はこの分岐の事前準備でもある: ルールがマッチしている行を `rule` に付け替えておくことで、その後の取消が確実に skip 経路へ乗る（manual のまま放置すると、取消 = DELETE → 復活 → もう一度取消してやっと skip、という「1 回目の取消が効かない」UX になる）。

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

### mirakc の多段集約

mirakc は `upstream` タイプのチューナー（別の Mirakurun 互換サーバをチューナーとして使う）を定義できるが、**この構成は採らない**。サイト間に余計な通信経路が挟まってストリームが劣化し、録画品質そのものを損なう。多拠点は「サイトごとに独立した mirakc + Rokuban が集約」の形に限る。

副次的な利点として、容量超過の判定（[データ層](data.md) §6.5）が「チューナーは 1 サイトに属する」を前提にできる。集約構成を許すと、API から見えない形でサイト間の容量が共有され、判定の前提が崩れる。

### EDCB ドライバ

EDCB（Linux ネイティブビルド可、番組追従の実績は最強）を録画エンジンの選択肢として検討したが、Windows ネイティブ由来のソフトであること、CtrlCmd（バイナリ TCP）+ Lua HTTP という API、録画物がローカルファイルでエッジ転送エージェントが必要になることから**採用しない**。

録画エンジンの抽象化レイヤーも作らない（YAGNI。mirakc API の呼び出しが reconciler / watcher に局所化されること自体が十分な継ぎ目）。

### 「この番組シリーズは常に...」のような永続的例外

overrides（予約の寿命 = 短命）ではなくルール側の機能。必要になったら別途検討。
