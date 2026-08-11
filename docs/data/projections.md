> [data.md](../data.md) §4 §6 の一部。索引から辿る

## 4. スキーマ設計: desired / observed の分離

具体的なテーブル定義（DDL）は [データベーススキーマ v1](../schema.md) 参照。

EPGStation の Reserve テーブルは「予約」「録画中状態」「録画結果」が混在していた。Rokuban では k8s の spec/status と同じ分離をスキーマに刻む:

- `rules` → `reservations` --- desired state。ルール評価の純粋な出力。手動予約もルール由来の予約も同じテーブルに入り、区別は `program_intents.action` の有無から導出する
- `program_intents` / `program_overrides` --- 番組単位のユーザー意図（録れ / 録るな）とパラメータの上書き。**api だけが書き ruler は読むだけ**の永続表 2 つ。導出行（reservations）とは別に置くことで、ruler が毎パス base を再計算しても意図が失われない
- `schedule_sync` --- observed state。mirakc 側に実在する schedule の最新観測。reconciler はこの 2 つの差分だけを見る
- `records` → `media_assets` --- 録画完了後の成果物。相対パス、エンコード派生物、サムネイルを紐付け
- 予約フィールドは mirakc のモデル（programId + RecordingOptions + tags）に素直に合わせる。「いつか使うかもしれない列」（マージン等）は持たない

予約がどのルールから生まれたかを外部キーで辿れるため、監査・デバッグが SQL で完結する。

### 予約オプションの base / overrides 分離

**base**（ruler が「ルール x EPG」から計算するフィールド群。ruler だけが書く）と **overrides**（ユーザーが上書きしたフィールドのみ。api だけが書く）は同じ行の 2 列ではなく別の表に置き、**effective = base + overrides** を都度合成する。ruler が EPG 更新のたびに base を丸ごと再計算しても overrides は別表なので構造的に触れず、3-way merge は不要。定義・設計根拠・意図を導出行と同じ表に置かない理由は [録画エンジン](../recording.md) §4「予約モデル」。

### mirakc 固有概念の隔離

永続テーブル（rules / media_assets / 履歴）に mirakc 固有フィールドを持ち込まない規律を設ける。mirakc の形をしているのは**短命な導出状態だけ**:

- **mirakc 非依存（永続・本当の資産）**: ルール / 録画履歴 / media_assets / ドロップ統計 / tombstone / overrides
- **mirakc の形（短命・導出）**: reservations の base（ルールから毎回再計算）、schedule_sync（observed、エンジンから再同期）

エンジンを載せ替える場合でも、書き直すのは reconciler / watcher / ingest の取得部と予約まわりのスキーマだけで、ライブラリと履歴は無傷で持ち越せる。

## 6. EPG プロジェクション

### 不変条件

> **api ロールは EPG を含むすべてのデータ読み取りを Postgres だけで完結させる。api が mirakc に問い合わせるパスは存在しない。**

mirakc に触るのは watcher / ruler / reconciler / worker のみ。この不変条件がサーバーレス api・ハイブリッド構成（[全体アーキテクチャ](../overview.md) 参照）・自宅ダウン中の番組表閲覧を守る。

### 「UI 完全な EPG プロジェクション」

- **線引き: プロダクトが画面に描画するものは全部入れる。mirakc の運用状態は入れない**（生 EIT/SI テーブル、チューナー状態などは除外）
- 具体的には: 番組名 / 説明 / 拡張形式イベント（出演者等）/ ジャンル / 映像・音声属性 / 無料フラグ / サービスメタデータ
- コストは無視できる: 8 日 x 全サービスで数万〜10 万行 x 数 KB = 数百 MB 上限。churn 対策は §5（[search.md](search.md)）の「性能上の実注意点」の通り（バッチ upsert + autovacuum）で、行が太っても本質は変わらない
- **真実の所在は変わらない**: 真実は常に mirakc。プロジェクションはレベルトリガーでいつでも全量再構築できる使い捨てキャッシュ。初期の「フルミラーを避ける」の意図（真実の二重化を避ける）はこの性質で維持される
- スキーマ: クエリ軸（サービス / 時間範囲 / ジャンル / 無料）は型付きカラム、詳細ペイロードは JSONB。検索は pg_trgm（[search.md](search.md) の決定のまま）

### 有界性: ローリングウィンドウ + 非正規化スナップショット

- EPG テーブルは放送済み番組を刈り取るローリングウィンドウ（8 日 + 猶予）。**永遠に太らない**
- **録画した番組の情報は予約〜ingest 時点で録画行に非正規化スナップショット**する。録画ライブラリは EPG テーブルにも mirakc にも依存せず自己完結 --- 「mirakc 固有概念を永続テーブルに漏らさない」規律、catalog エクスポート（[メディアストレージ](../storage.md) 参照）、履歴ベース重複排除はすべてこのスナップショットの上に乗る

### サービスロゴ: ドロップ

当初は DB（bytea）ではなくファイルとして `logos/` 配下に置き、mirakc のロゴ API から watcher が取得してハッシュ管理する設計だった。**実装前に見送った**。

mirakc は起動中の局ロゴ抽出（放送波からの動的抽出）をサポートしていない（[dekiru-mirakc: logos](https://mirakc.github.io/dekiru-mirakc/stable/config/logos.html)）。運用者が `mirakc-arib` 等で事前抽出したファイルを `config.yml` に静的パスとして登録し、mirakc の `GET /api/services/{id}/logo` はそれを配るだけ。つまりロゴは放送から自動では増えず、運用者が mirakc 側の設定を触った時点で既に手元にファイルがある。この静的アセットを Rokuban 側でもう一度取得・ハッシュ管理・自前配信する価値は薄く、「api ロールはファイルシステムに依存しない」不変条件との境界（配信を streamer 経由にするか等）を余分に検討するコストに見合わない。

`epg_services.has_logo_data` / `logo_id` 列は mirakc の `Service` 構造体をそのまま射影しているだけなので残っているが、これらを使ってロゴ本体を取得・配信する機構は作らない。

## 経緯と失敗事例

- 手動予約とルール予約を同じ `reservations` に統一し、区別を `program_intents.action` の有無から導出する決定は issue #26
- 意図（`program_intents` / `program_overrides`）を導出行と別の永続表に置く設計の経緯（issue #18 の案 A）は [録画エンジン](../recording.md) §4「予約モデル」
- サービスロゴのドロップは M2-12。検討の経緯は issue #24 のコメント参照
