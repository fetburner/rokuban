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
5. **サイトスコープ**: mirakc の programId / record id はインスタンス単位のスコープしか持たない。[設定](../configuration.md)（issue #9）は「多拠点が現実化したら `mirakcs:` リストで互換拡張」と定めており、その際のスキーマ波及を避けるため **mirakc を指すすべてのテーブルに `site` 列を最初から持つ**
   - `site` は設定ファイルで定義するサイト名（text）。サイトのレジストリは設定であり、DB に sites テーブルは作らない
   - M1 では設定が単一 `mirakc:` なので全行が同一サイト名になる。`mirakcs:` リスト対応は設定とアプリの変更のみで、マイグレーション不要
   - site を持つのは reservations / schedule_sync / record_sync / recordings（+ M1-6 の EPG プロジェクション）。media_assets / drop_stats は中央ストレージの台帳なので持たない
   - **ただし「マイグレーション不要」は利益を過大に見せている**（[#31](https://github.com/fetburner/rokuban/issues/31)）。`site` の値は設定由来ですらなく `db.DefaultSite = "default"` のハードコードで、API には site の次元が無い（`openapi.yaml` が 2 箇所で自認）。`programId` は site スコープなので、多拠点化で本当に壊れるのは **API の資源同定とクライアントのキャッシュキー**の方であり、そこは未払いである。原則 10 の「先払いは高い方から」に反しているので、`#29`（意図の宛先変更）に着手する前に方針を決める
6. **導出値と不可逆な事実を分ける**（CLAUDE.md 不変条件 9。M2-4 / M2-5 で同じ歪みを 3 回踏んで言語化した）
   - 毎パス再計算される値と、二度と再取得できない事実を 1 つの列に同居させると**導出側が事実を上書きする**
   - `program_intents.action`（#18）・`reservations.source`（#26）・`reservations.state`（M2-4）が実例。いずれも実害が出た
   - 新しい列を足すときは「毎パス作り直せるか」を問う。作り直せるなら列にせず導出する
   - **混同は列だけでなく identity と式にも起きる。** 導出物の id を API の宛先にすると書ける意図が導出側の状態に依存する（[#29](https://github.com/fetburner/rokuban/issues/29)）。導出を式で書いても実装が列に「遷移」を保存すると片側の分岐しか持たない（[#30](https://github.com/fetburner/rokuban/issues/30)）
   - 例外は **`circuit_breakers`**（§3.6）。「誰かが確認した」は再取得できないので、このスキーマで唯一の意図的な非導出状態
7. **意味を持たない行を作らない**（CLAUDE.md 不変条件 10）
   - **行の存在そのものを主張として使う**。空の上書きは「行が無い」（`program_overrides`）、停止していないブレーカーは「行が無い」（`circuit_breakers`）
   - 「何も主張していない行」を作ると掃除の規則が必要になり、その規則が判断材料を別表に求めて壊れる（#18 の `rule_id` 依存）
   - 「あってはいけない組み合わせ」は **CHECK で禁止するより表現不可能にする**
8. **型の規律**
   - 状態は Postgres の enum 型ではなく `text` + `CHECK`（enum 型はマイグレーションが面倒で利点が薄い）
   - 時刻はすべて `timestamptz`
   - ID は `bigint GENERATED ALWAYS AS IDENTITY`
   - クエリ軸（WHERE / JOIN に使う列）は型付きカラム、可変・詳細ペイロードは `jsonb`
   - **jsonb を許すのは「そのテーブル自身のロジックが中身を一切使わない不透明なペイロード」のときだけ**（[録画エンジン](../recording.md) §4.2「jsonb を許す条件」）。内容でクエリするなら型付き列
   - 不透明なペイロードには**内容を検査する CHECK も置かない**。「クエリはしないが制約はする」という中途半端な状態を作らない。同じ理由でマージも SQL（`||` / `- keys`）ではなく Go 側で型付きに行う
   - **PostgreSQL 15 以上**を前提とする（`UNIQUE NULLS NOT DISTINCT` が 15 で導入）
9. **表は行の寿命で割る**（[CLAUDE.md](../../CLAUDE.md) 不変条件 12。M2 完了後のレビューで言語化）
   - **1 表 = 1 つの書き手 = 1 つの寿命。** 原則 6 は列の粒度なので、行に寿命が混ざるケースを網に掛けられない
   - `reservations` の 1 行には**かつて** ①ruler の導出出力（寿命 = 1 パス）②番組の事実のスナップショット（寿命 = 放送。3 表に重複しドリフト中だった。[#27](https://github.com/fetburner/rokuban/issues/27)）③不可逆な観測（`state = 'orphaned'`。[#28](https://github.com/fetburner/rokuban/issues/28) / [#30](https://github.com/fetburner/rokuban/issues/30)）が同居していた。**Phase 1 で②を `program_snapshots`（§3.7）に、③を `orphaned_at` に分離し、`reservations` には①だけが残っている**
   - ユーザー意図を導出行から出したのは正しかったが、分割の軸を「予約という概念」に取ったので 2 回に分かれた（#18 → M2-4）。軸を寿命に取れば 1 回で済んだ
   - 新しい列を足すときは「**この値はこの行と同時に生まれて同時に死ぬか**」を問う。違えば `(site, program_id)` を主キーにした別表にする
10. **形を固定する前に、その形を決める判定基準を書く**（[CLAUDE.md](../../CLAUDE.md) 不変条件 11）
    - このドキュメント冒頭の「最終形で切る」の訂正がその実例。判定基準（原則 6 / 7）が M2-4 / M2-5 の後に来たので、それまでに固めた `reservations` の列が 5 回変更された
    - 導出テーブルの列は**書き手のコードと同じ PR で決める**。新しい列を足すときは「これを書くコードは今あるか」を問う
    - 将来への先払いは**高い方から**。`site` 列は安い方（DB）を先払いして高い方（API の資源同定）が未払いのままで、利益が担保されていない（原則 5 の訂正。[#31](https://github.com/fetburner/rokuban/issues/31)）

