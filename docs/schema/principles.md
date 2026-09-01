> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§1）。

## 1. 設計原則

1. **desired / observed の分離**（k8s の spec/status と同型）
   - desired: `reservations`（ruler / api が書く「あるべき姿」）
   - observed: `schedule_sync` / `record_sync`（mirakc の観測結果。短命・使い捨て）
   - reconciler / watcher はこの 2 つの差分だけを見る
2. **mirakc 固有概念の隔離**（不変条件 7）
   - mirakc の形をしてよいのは短命な導出状態（`reservations` の base、`schedule_sync`、`record_sync`）だけ
   - 永続テーブル（`recordings` / `media_assets` / `drop_stats`）に mirakc の ID や enum を**構造として**持ち込まない。mirakc の record id は `record_sync` にのみ存在し、`record_sync.recording_id` が永続側への片方向ポインタになる
   - 例外: 品質イベント（`recording.failed` の理由等）は履歴として価値があるため、**構造化カラムではなく jsonb の自由形式ログ**として保持する（システムのロジックはその中身に依存しない）
3. **コミット = DB 行**（不変条件 3）: ファイルの公開は `media_assets` 行の INSERT。rename のアトミック性に依存しない
4. **tombstone**: 物理削除後もメタデータ行は残す。ドロップ統計・録画履歴・重複排除は削除後も機能する
5. **識別子 / 存在のスコープ**: mirakc が指すものは 2 種類ある。**record id はインスタンス単位で採番される識別子**で、取り違えると別の録画を指してしまう。**programId（`Service.id` も同型）は放送そのものから合成される値**で、識別子ではなく存在のスコープしか持たない。取り違えても別の番組にはならず、その site の EPG に無ければ 404 になるだけである（[ruler](../recording/ruler.md)「サイトの扱い」）
   - 「同一放送なら全 site で同一 programId になる」は、NID/SID/eventId から合成する Mirakurun の ID 合成規則からの**演繹**であり、複数サイトの実機で測定した結果ではない（**未検証**）。式は [reservations.md](reservations.md)「チャンネル識別はスナップショットする」。ruler の N 予約・重複排除（`internal/ruler/dedupe.go`）はこの前提の上に成り立つ
   - [設定](../configuration.md)は「多拠点が現実化したら `mirakcs:` リストで互換拡張」と定めており、その際のスキーマ波及を避けるため **mirakc を指すすべてのテーブルに `site` 列を最初から持つ**。理由は識別子が曖昧だからではなく、**行の存在と状態が site ごとの観測だから**である（同じ放送でも A では録画中、B では未予約になりうる）
   - `site` は設定ファイルで定義するサイト名（`config.mirakcs[].site`。各要素必須で既定値は無い）。サイトのレジストリは設定であり、DB に sites テーブルは作らない
   - site を持つのは reservations / schedule_sync / record_sync / recordings（+ EPG プロジェクション）。media_assets / drop_stats は中央ストレージの台帳なので持たない
   - **API の資源同定は判定基準で決める**（[api/rest.md](../api/rest.md) §エンドポイント設計の規約）。site をパスに置くかどうかは、件数（単体か一覧か）でも資源の種類名でもなく、**その資源の存在・状態が特定 1 つの site（mirakc インスタンス）の観測に閉じているか**で決める。mirakc の record や、その site の EPG に存在するかで決まる programId はこれに該当し、単体でも一覧でも site をパスに含める。番組・意図・上書きを指すパスは `/api/sites/{site}/programs/{programId}` の形を取る（TanStack Query のクエリキー・SSE の invalidate 単位もサイトごとに階層化される）。同じ種類の資源でも、実体が site に閉じない場合（site 非依存で動くワーカーが書く行など）は種類名では判定せず、その資源だけを site 無しにする。site に束縛されない資源（rokuban 採番の `recordings.id` 等）や、複数 site にまたがりうる観測の集合は、単体か一覧かによらず site をパスに固定せず、絞り込み条件として指定するか結果本体が運ぶ。導出行（`reservations`）は書き込みの宛先にしない —— 意図（`program_intents`）・上書き（`program_overrides`）は `(site, programId)` を自身のキーとして書く。`reservations` の導出の書き手は ruler だけ（例外はルール削除 API の同期削除 1 本。[reservations.md](reservations.md) §3 冒頭）
6. **導出値と不可逆な事実を分ける**（CLAUDE.md 不変条件 9）
   - 毎パス再計算される値と、二度と再取得できない事実を 1 つの列に同居させない。混同は列だけでなく identity・式・適用の瞬間にも起きる。実例と失敗事例は [invariants.md](../invariants.md) §9
   - 例外は **`circuit_breakers`**（§3.6）。「誰かが確認した」は再取得できないので、このスキーマで唯一の意図的な非導出状態
7. **意味を持たない行を作らない**（CLAUDE.md 不変条件 10）
   - **行の存在そのものを主張として使う**。空の上書きは「行が無い」（`program_overrides`）、停止していないブレーカーは「行が無い」（`circuit_breakers`）。詳細は [invariants.md](../invariants.md) §10
   - **同じ述語を 2 箇所目のクエリファイルに書く前に view にする。** 述語の一致をコメント（「揃えること」）で守るのは、CHECK で禁止するより弱い —— view なら乖離が表現不可能になる（`program_investments`）。ただし述語の正体が永続の観測なら、view で導出せず専用表の行の存在にする（`never_scheduled_events`）
8. **型の規律**
   - 状態は Postgres の enum 型ではなく `text` + `CHECK`（enum 型はマイグレーションが面倒で利点が薄い）
   - 時刻はすべて `timestamptz`
   - ID は `bigint GENERATED ALWAYS AS IDENTITY`
   - クエリ軸（WHERE / JOIN に使う列）は型付きカラム、可変・詳細ペイロードは `jsonb`
   - **jsonb を許すのは「そのテーブル自身のロジックが中身を一切使わない不透明なペイロード」のときだけ**（[録画エンジン](../recording.md) §4.2「jsonb を許す条件」）。内容でクエリするなら型付き列
   - 不透明なペイロードには**内容を検査する CHECK も置かない**。「クエリはしないが制約はする」という中途半端な状態を作らない。同じ理由でマージも SQL（`||` / `- keys`）ではなく Go 側で型付きに行う
   - まだやりがちな違反: `recordings.quality_events`（jsonb）の中身への `EXISTS(jsonb_array_elements(...))` を core ロジックの WHERE 軸にすること。欠測の判定軸は専用表 `never_scheduled_events` の行の存在にし、`recordings` は観測された試行だけを持つ（[recordings.md](recordings.md)「never_scheduled_events」）
   - **PostgreSQL 15 以上**を前提とする（`UNIQUE NULLS NOT DISTINCT` が 15 で導入）
9. **表は行の寿命で割る**（[CLAUDE.md](../../CLAUDE.md) 不変条件 12）
   - **1 表 = 1 つの書き手 = 1 つの寿命。** 原則 6 は列の粒度なので、行に寿命が混ざるケースを網に掛けられない。`reservations` に 3 つの寿命が同居していた実例は [invariants.md](../invariants.md) §12
   - 新しい列を足すときは「**この値はこの行と同時に生まれて同時に死ぬか**」を問う。違えば `(site, program_id)` を主キーにした別表にする
   - **この寿命チェックは永続表に対して盲目**（[CLAUDE.md](../../CLAUDE.md) 不変条件 13）。**recordings 本体は「試行の帰結の観測」だけを持つ脊椎で、脊椎（watcher / reconciler）以外のループが書く状態は `recording_id` を FK に持つ衛星表（`media_assets` がその形）に置く。** 境界（`deleted_at` / `superseded_at` は衛星に出せない等）は [invariants.md](../invariants.md) §13
10. **形を固定する前に、その形を決める判定基準を書く**（[CLAUDE.md](../../CLAUDE.md) 不変条件 11）
    - 導出テーブルの列は**書き手のコードと同じ PR で決める**。新しい列を足すときは「これを書くコードは今あるか」を問う。判定基準が後から来て `reservations` の列が 5 回変更された経緯は [invariants.md](../invariants.md) §11
    - 将来への先払いは**高い方から**。`site` 列（安い方、DB）は v1 から先払いしていたが、API の資源同定（高い方）は後から判定基準を書いた（[api/rest.md](../api/rest.md) §エンドポイント設計の規約）。現行のパスが全エンドポイントでこの基準に適合しているとは限らない（同節の未解決を参照）

