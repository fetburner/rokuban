> [data.md](../data.md) §4 §6 の一部。索引から辿る

## 4. スキーマ設計: desired / observed の分離

予約・録画中の状態・録画結果は、書き手も寿命も違うので、1 つの表に同居させない。あるべき姿と観測結果を別の表に分け、予約のフィールドは mirakc のモデルに素直に合わせる。

k8s の spec/status と同じ分離を、スキーマに刻む。具体的なテーブル定義（DDL）は [データベーススキーマ v1](../schema.md) を参照する。

表の割り方は次の通り。

- ルールから導く予約は、あるべき姿を表す。ルール評価の純粋な出力で、手動予約もルール由来の予約も同じ表に入る。区別は、意図の行の有無から導出する
- 番組単位のユーザー意図（録れ / 録るな）とパラメータの上書きは、永続表 2 つに置く。api だけが書き、ruler は読むだけである。導出行とは別に置くので、ruler が毎パス base を再計算しても意図が失われない
- schedule の同期表は、観測結果を表す。mirakc 側に実在する schedule の最新観測で、reconciler は、あるべき姿との差分だけを見る
- 録画完了後の成果物は、record からメディア資産へ紐付ける。相対パス・エンコード派生物・サムネイルを持つ
- 予約のフィールドは、mirakc のモデルに素直に合わせる。「いつか使うかもしれない列」（マージン等）は持たない

予約がどのルールから生まれたかを外部キーで辿れるため、監査とデバッグは SQL で完結する。

<details><summary>根拠</summary>

- あるべき姿: `rules` → `reservations`
- 区別の導出元: `program_intents.action` の有無
- 永続表 2 つ: `program_intents` / `program_overrides`
- 観測結果: `schedule_sync`
- 成果物: `records` → `media_assets`
- mirakc のモデル: programId + RecordingOptions + tags

</details>

### 予約オプションの base / overrides 分離

ruler が計算する base と、ユーザーが上書きした overrides は別の表に置き、都度合成して実効値を得る。ruler が base を再計算しても overrides には触れないので、3-way merge は不要である。

base は、ルールと EPG から ruler が計算するフィールド群で、ruler だけが書く。overrides は、ユーザーが上書きしたフィールドだけで、api だけが書く。同じ行の 2 列には置かない。

定義・設計根拠・意図を導出行と同じ表に置かない理由は、[録画エンジン](../recording.md) §4「予約モデル」にある。

<details><summary>根拠</summary>

- 合成の式: `effective = base + overrides`

</details>

### mirakc 固有概念の隔離

永続テーブルには、mirakc 固有のフィールドを持ち込まない。mirakc の形をしてよいのは、短命な導出状態だけである。

| 種類 | 持つもの |
|---|---|
| mirakc 非依存の永続資産 | ルール・録画履歴・メディア資産・ドロップ統計・tombstone・上書き |
| mirakc の形をした短命な導出状態 | 予約の base・schedule の同期表 |

予約の base は、ルールから毎回再計算する。schedule の同期表は、エンジンから再同期する。エンジンを載せ替える場合でも、書き直すのは reconciler・watcher・ingest の取得部と、予約まわりのスキーマだけである。ライブラリと履歴は、無傷で持ち越せる。

<details><summary>根拠</summary>

- 永続資産: `rules` / `media_assets` / ドロップ統計 / tombstone / `program_overrides`
- 導出状態: `reservations` の base、`schedule_sync`

</details>

## 6. EPG プロジェクション

EPG は、api が Postgres だけで読めるようにプロジェクションとして持つ。画面に描画するものは全部入れ、放送済みの番組は刈り取って件数を一定以下に保つ。

### 不変条件

api ロールは、EPG を含むすべてのデータ読み取りを Postgres だけで完結させる。**api が mirakc に問い合わせるパスは存在しない。**

mirakc に触るのは、watcher・ruler・reconciler・worker だけである。この不変条件が、サーバーレス api・ハイブリッド構成（[全体アーキテクチャ](../overview.md) 参照）・自宅ダウン中の番組表閲覧を守る。

### 「UI 完全な EPG プロジェクション」

プロダクトが画面に描画するものは全部入れ、mirakc の運用状態は入れない。権威の所在は変わらず、プロジェクションはレベルトリガーでいつでも全量再構築できる使い捨てキャッシュである。

入れるのは、番組名・説明・拡張形式イベント（出演者等）・ジャンル・映像と音声の属性・無料フラグ・サービスのメタデータである。生の EIT/SI テーブルとチューナー状態は除外する。

初期の「フルミラーを避ける」という意図は、権威の二重化を避けることだった。権威が常に mirakc にある性質で、この意図は維持される。

コストは無視できる。8 日 x 全サービスで数万〜10 万行 x 数 KB なので、上限は数百 MB である。churn 対策は、§5（[search.md](search.md)）の「性能上の実注意点」の通りである（バッチ upsert と autovacuum）。行が太っても、コストが無視できることは変わらない。

スキーマは、クエリ軸（サービス・時間範囲・ジャンル・無料）を型付きカラムにし、詳細ペイロードを JSONB にする。検索は pg_trgm で、[search.md](search.md) の決定のままである。

### 件数の上限: ローリングウィンドウ + 非正規化スナップショット

EPG テーブルは、放送済み番組を刈り取るローリングウィンドウ（8 日 + 猶予）で件数を一定以下に保つ。録画した番組の情報は、予約から ingest の時点で録画行に非正規化スナップショットし、録画ライブラリを EPG からも mirakc からも自立させる。**EPG テーブルは永遠に太らない。**

録画ライブラリは、EPG テーブルにも mirakc にも依存せず自己完結する。このスナップショットの上に乗るのは、「mirakc 固有概念を永続テーブルに漏らさない」規律、catalog エクスポート（[メディアストレージ](../storage.md) 参照）、履歴ベース重複排除である。

### サービスロゴ: ドロップ

サービスロゴは、Rokuban 側で取得・保管・配信しない。ロゴは運用者が mirakc に静的に登録したファイルで、もう一度取得して管理する価値が薄いので、実装前に見送った。

当初は、ロゴを DB（bytea）には置かず、ファイルとしてロゴ用のディレクトリに置く設計だった。mirakc のロゴ API から watcher が取得し、ハッシュ管理するはずだった。

見送った理由は、mirakc が起動中の局ロゴ抽出（放送波からの動的抽出）をサポートしていないからである（[dekiru-mirakc: logos](https://mirakc.github.io/dekiru-mirakc/stable/config/logos.html)）。運用者が `mirakc-arib` 等で事前抽出したファイルを `config.yml` に静的パスとして登録し、mirakc の `GET /api/services/{id}/logo` は、それを配るだけである。つまりロゴは放送から自動では増えず、運用者が mirakc 側の設定を触った時点で、既に手元にファイルがある。

この静的アセットを Rokuban 側でもう一度取得・ハッシュ管理・自前配信する価値は薄い。「api ロールはファイルシステムに依存しない」不変条件との境界（配信を streamer 経由にするか等）を、余分に検討するコストにも見合わない。

サービス表のロゴ関連の 2 列は、mirakc の構造体をそのまま射影しているだけなので残っている。ただし、これらを使ってロゴ本体を取得・配信する機構は作らない。

<details><summary>根拠</summary>

- 当初の置き場所: `logos/` 配下
- 残っている列: `epg_services.has_logo_data` / `logo_id`。mirakc の `Service` 構造体を射影している

</details>
