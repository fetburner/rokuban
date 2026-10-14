> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§3 / §3.5 / §3.6 / §3.7）。

## 3. reservations — 予約（desired state）

予約の表は、ルール評価の純粋な出力だけを持ち、導出の書き手は ruler ただ 1 人である。手動予約もルール由来の予約も同じ表に入り、区別はユーザー意図の有無から導出する。表には区別の列を持たない。

```sql
CREATE TABLE reservations (
    site              text   NOT NULL,          -- 設定ファイル定義のサイト名
    program_id        bigint NOT NULL,          -- mirakc/Mirakurun の programId（site 単位のスコープ）
    rule_id           bigint,                   -- 勝者ルール。REFERENCES rules(id) ON DELETE SET NULL

    -- 予約オプションの導出側。
    -- ユーザーの上書きは program_overrides 表にあり、この行には載らない（§3.5）
    base              jsonb,                    -- ruler だけが書く。manual では NULL

    -- 重複排除の判定根拠。ruler が毎パス作り直す導出列で、
    -- base と同じ凍結規則に従う。FK は張らない（後述）
    dedup_match_recording_id bigint,
    dedup_similarity         real,
    CHECK ((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL)),

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (site, program_id),  -- desired 予約は site x programId につき最大 1 つ
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
        ON DELETE CASCADE
);

CREATE INDEX ON reservations (rule_id);
```

<details><summary>根拠</summary>

- 区別の導出元: `program_intents.action`（`action='record'` があれば手動予約）
- 表に残る出力: `rule_id` / `base` / dedup 根拠 2 列

</details>

### 書き手と表に入れないもの

予約の表に残るのは ruler の 1 パスの出力だけで、導出の書き手は ruler ただ 1 人である（不変条件 12「1 表 = 1 つの書き手 = 1 つの寿命」）。番組の事実と不可逆な観測は別の表が持ち、1 行に同居させない。

出力とは、勝者ルール・導出した予約オプション・重複排除の根拠 2 列である。番組の事実は番組スナップショット（§3.7）が持ち、不可逆な観測は録画履歴の試行行（[recordings.md](recordings.md) §5）が持つ。**この 3 つを 1 行に同居させてはならない**（[invariants.md](../invariants.md) §12）。

番組の事実（タイトル・開始時刻・尺・チャンネル識別）は、番組キーの外部キーで参照する番組スナップショットにある。不可逆な観測を書く列も無い。「番組終了後に schedule が観測されなかった」という観測は、録画履歴の試行行が持つ（[recordings.md](recordings.md) §5「行の作られ方」）。この事実は履歴側に置く。短命な導出表に残すと、ruler の再実体化で消える。さらに、1 放送イベントに active な試行は 1 行という一意制約による宣言的な強制も効かない。

<details><summary>根拠</summary>

- 出力の列: `rule_id` / `base` / dedup 根拠 2 列
- 番組の事実の表: `program_snapshots`。参照は `(site, program_id)` の FK
- 一意制約: `recordings_unique_active_event`

</details>

### サイトをまたぐ予約

同一の BS/CS 番組が複数サイトの EPG に現れた場合、サイトが違えば別予約になる。両サイトで録ることが表現できる。サイトをまたぐ重複排除はルール側の関心事で、履歴ベース重複排除で扱う。

### ルール削除時の同期削除

ルールの削除は、ユーザーの投資が無い予約行を、ルール削除と同じトランザクションで同期削除する。DELETE 文が投資の有無を適用の瞬間に再評価するので、並行して着地する手動予約を踏み潰さない。これが、予約の表に書き手が 2 人いる唯一の例外である。

ルール削除（`DELETE /api/rules/{id}`）は、1 表に書き手が 2 人いる形（不変条件 12 の兆候）である。だが両者の DELETE 文は、いずれも WHERE で意図と上書きの有無を適用の瞬間に再評価する。そのため、導出の判定と適用の間に並行して着地する手動予約を踏み潰す窓（[invariants.md](../invariants.md) §9「適用の瞬間」）は生じない。同期を選んだ理由は [録画エンジン](../recording.md) §4.4「取消は `PUT .../intent {action: skip}`」に詳しい。

<details><summary>根拠</summary>

- 投資の判定: `program_investments`（intent / overrides）

</details>

### 重複排除の根拠に FK を張らない

重複排除の根拠として持つ録画 id には、外部キーを張らない。外部キーと、根拠 2 列が揃うことを保つ CHECK が両立しないためである。外部キーの動作は片方の列だけを NULL にするので、録画履歴の物理削除が CHECK に違反して中断する。

外部キーの動作は、外部キー側の列しか NULL にできない。そのため、参照されている録画履歴の行を物理削除すると、録画 id だけが NULL で類似度は残る行ができて CHECK に違反し、**DELETE 自体が中断する。** 後片付けの機構が安全網に引っかかって、削除を不可能にする。外すのは外部キーの方にした。

外す理由は 3 つある。第一に、根拠 2 列は ruler が毎パス作り直す導出値（不変条件 9）なので、参照先が消えても次のパスで両方 NULL に戻る。孤立は自己修復する。第二に、録画履歴の id は再利用されないので、孤立した id が別の録画を指すことは構造的に起きない。外部キーを外して一番怖い失敗モードが無い。第三に、§5 の tombstone 契約により、本番では録画履歴の行を物理削除しない。外部キーが守っているのは、起きない事象である。

CHECK は「あってはいけない組み合わせを表現不可能にする」（不変条件 10）を担っている。仕事をしているのは CHECK の方である。

<details><summary>根拠</summary>

- 外部キーを張らない列: `dedup_match_recording_id`（`recordings(id)` を指す）
- 両立しない定義: `REFERENCES recordings (id) ON DELETE SET NULL` と、根拠 2 列の CHECK
- CHECK 違反の行の例: `(NULL, 0.87)`
- id が再利用されない理由: `recordings.id` は `GENERATED ALWAYS AS IDENTITY`

</details>

### base / overrides の意味論

予約に効く実効オプションは、導出側の base にユーザーの overrides を重ねたものである。overrides は別の表にあるので、ruler が base を丸ごと再計算しても消えない。reconciler の同期も ingest / encode の参照も、常にこの実効値を使う。

実効値の解決は 1 箇所に集約し、jsonb の Unmarshal 失敗を握りつぶさない。スキップかどうかの判定にも、名前を付けている。

base と overrides は同形の jsonb ドキュメントである（§8）。ruler は EPG 更新のたびに base を丸ごと再計算してよく、**overrides は別の表なので構造的に触れない。**

スキップは overrides のキーにせず、ユーザー意図の action 列で持つ。列なので、base 側のスキップに対する優先順位が明示的に決まる。スキップの意図が勝つ。意図がスキップで、かつ上書きが無い番組は、予約行を持たない（overrides があればスキップでも行は残る。下記「program_investments」参照）。

<details><summary>根拠</summary>

- 実効値の式: `effective = COALESCE(base, '{}') ⊕ program_overrides.overrides`
- 解決の集約先: `reservation.EffectiveOptions`
- スキップ判定: `reservation.Options.IsSkipped()` が `effective.skip` に名前を付ける
- スキップの置き場: `program_intents.action`。`action = 'skip'` が勝つ

</details>

### active / detached / orphaned は API が都度導出する

予約の active / detached / orphaned という状態は、列に持たず、API が読むたびに導出する。同期の可否は実効値のスキップで決め、表示用の状態をフィルタに使ってはならない。

<details><summary>根拠</summary>

- 存在しない列: `state`
- 導出の実装: `internal/api/handler.go` の `reservationState`

</details>

#### 状態を列に焼かない理由

**状態を列に焼いてはならない。** 導出値と不可逆な観測を 1 列に潰す形になり、実装は式では書かず前パスからの遷移を書くことになり、片側の分岐しか持たなくなる。状態は API が読むたびに計算して返す（[invariants.md](../invariants.md) §9「式」）。

active と detached は、勝者ルールと base から導出する。orphaned は、この予約の放送イベントに欠測行があり、同じイベントの録画履歴の行が 1 つも無いことから導出する。

「schedule が観測されなかった」は、最終観測時刻のようなタイムスタンプからは導出できない。観測側が事実として欠測行を書く必要がある。予約行の結合には、放送イベントのキーを使う。不安定な導出 id は使わない。これは不変条件 9 の identity であり、予約クエリファイルのコメントが権威である。

<details><summary>根拠</summary>

- active / detached の導出元: `(rule_id, base)`
- 欠測行: `never_scheduled_events`。判定は `GetReservationFull` / `ListReservationsFull` の `never_recorded` 列
- 導出しきれないタイムスタンプ: `epg_last_seen_at`
- 放送イベントのキー: `(site, network_id, service_id, event_id)`
- 権威のコメント: `internal/db/queries/reservations.sql`

</details>

#### 3 つの状態

active は勝者ルールを持つ通常の予約、detached はルールが外れたが投資が残る予約、orphaned は一度も schedule が観測されず本物の録画試行も無い予約である。

| 値 | 意味 | 導出の材料 |
|---|---|---|
| `active` | 通常の desired 予約 | 勝者ルール |
| `detached` | ルールが外れた投資付きの予約 | 勝者ルール無し・base あり |
| `orphaned` | 一度も観測されなかった予約 | 欠測行・録画履歴なし |

active は、勝者ルールを持つ通常の予約である（ルールの無い手動予約も含む）。detached は、ルールがマッチしなくなったが、record 意図または上書きがある行である。base は凍結され、実質手動予約として動く。意図がスキップなら、録画しない detached になる。

orphaned は、この予約に対応する放送イベントについて、一度も schedule が観測されなかった欠測行があり、本物の録画試行は 1 行も無い予約である。mirakc 由来の途中失敗は欠測表に入らない（再試行経路を壊さない）。即削除せず残して、「録れなかった」を説明可能にする。

orphaned の判定は、欠測表の EXISTS と、同じ放送イベントの録画履歴全体に対する NOT EXISTS の積である。録画履歴は live 限定にしないので、本物の録画をごみ箱に入れても orphaned に戻らない。放送イベントのキーは、番組スナップショットを経由して引く。予約行の導出 id を結合先にしてはならない（[invariants.md](../invariants.md) §9「identity」）。

<details><summary>根拠</summary>

- active: `rule_id IS NOT NULL`（または base が無い manual 予約）
- detached: `rule_id IS NULL AND base IS NOT NULL`。`program_investments` view に行がある。`intent{skip}` なら録画しない detached
- orphaned: `never_scheduled_events` 表の EXISTS と、`recordings` 全履歴に対する NOT EXISTS の積（`GetReservationFull` の `never_recorded`）
- 結合先にしない id: 予約行の導出 id。放送イベントキーは `program_snapshots` を経由して引く

</details>

#### 物理削除と遷移

予約行の物理削除（GC）は、番組の終了時刻を過ぎた後に限り、スナップショットの GC に連動する。放送開始直前は猶予で削除せず、そのための専用の状態は増やさない。

終了時刻は、番組スナップショットの開始時刻に尺を足して判定する（§3.7）。予約は番組スナップショットへの外部キーが CASCADE なので、スナップショットが GC されたときに一緒に落ちる。active / detached / orphaned のいずれであるかは問わない。

欠測表は番組スナップショットへの外部キーを持たないので、同時には消えない。放送地平を超える保持猶予 + 30 日で、別途刈られる（[recordings.md](recordings.md) §5）。

意図も上書きもない active 予約がルール・EPG から消えた場合は、通常の宣言的動作として削除する（大量削除サーキットブレーカーの対象）。ただし放送開始直前は猶予で削除しない。猶予中の行は勝者ルールが前パスのまま据え置かれるので、active の導出はそのまま成立し続ける。専用の状態は増やさない（[ruler.md](../recording/ruler.md)「直前 unmatch の猶予」）。ルール再マッチで base を再計算して active に戻る（overrides は無傷）。

<details><summary>根拠</summary>

- 終了時刻: `program_snapshots.start_at + duration_ms`。FK は `ON DELETE CASCADE`
- 欠測表の刈り: `never_scheduled_events`。`retention_grace + 30日`
- 猶予の設定: `ruler.retract_grace`
- 猶予中の導出: `rule_id IS NOT NULL`

</details>

#### 同期対象のフィルタ

同期対象かの判定に使ってよいのは、放送イベントに欠測行が無いことだけである。active や detached は表示用の派生値なので、フィルタに使ってはならない。次の 4 点がその境界になる。

1. active / detached をフィルタにしてはならない。どちらも UI 表示用の派生値で、同期の可否を決めるのは実効値のスキップである。導出値を同期フィルタに使うと、ルールが外れた手動予約が気づかないうちに録画されなくなる（[invariants.md](../invariants.md) §9）。
2. 同期除外は、欠測表の行の存在だけを見る。一度欠測と判定された放送イベントは、本物の record が後から来ても、行が寿命内にある限り同期対象に戻らない。終了済み予約を再び schedule しないためである。
3. 表示は、欠測行に加えて、本物の録画履歴の行が無いことも見る。record が来たら orphaned は消えるが、欠測行は寿命内では残る。録画履歴の照合は live 限定にしないので、ごみ箱操作で orphaned に戻らない。
4. どちらも、mirakc 由来の途中失敗だけでは成立しない。failed 試行は録画履歴にだけ入り欠測表には入らないので、再試行経路を壊さない。

欠測行を保持猶予 + 30 日まで残しても、通常は読者から観測されない。この表の読者は予約行を経由し、予約行は番組終了 + 保持猶予で先に GC されるからである。ただし、`program_id` の再利用で同じ放送イベントのキーから新番組が到達可能になる経路は、この限りではない（§3.7 の未解決の窓）。

<details><summary>根拠</summary>

- フィルタの実装: `ListReservationsForSyncEvaluation`
- 同期の可否を決める値: `effective.skip`
- 表示の列: `never_recorded`
- 欠測表の寿命: `retention_grace + 30日`。読者が経由する行: `reservations`

</details>

### 書き込み所有権

予約の表に書くのは ruler だけで、API は書かない。ユーザー意図と上書きの表は API だけが書く。例外は、ルール削除時の同期削除である。

| 対象 | 書く人 |
|---|---|
| `reservations` の INSERT/UPDATE | ruler（毎パス） |
| `reservations` の DELETE（desired から外れた予約） | ruler（毎パス） |
| `reservations` の DELETE（ルール削除時） | api |
| 番組スナップショットの GC と CASCADE | ruler のパス |
| `program_intents` / `program_overrides` | api |

#### ruler の DELETE の分類

ruler が消す予約は 3 つのクエリに分かれる。録画・ingest 完了済みの行は、メディアアセットの存在だけで確定的に消し、ルールが供給していた行はブレーカーの対象にし、投資を手放した行はブレーカーの外で消す。

録画・ingest 完了済みで原本のメディアアセットを持つ行は、メディアアセットの存在だけで確定的に判る。そのため、投資の有無や EPG 射影・ブレーカーに依存せず、完了済み予約の削除が消す。

それ以外の「ルールが base を供給していながら desired から外れた」行は、大量削除サーキットブレーカーの対象である。ユーザーが投資を手放す書き込みをしない限り起きない行は、ブレーカーの外で消す。分類の条件と境界は[録画エンジン](../recording.md) §3.2 にある。

ルール削除時の DELETE は、投資の無い予約だけが対象で、api の `DeleteRule` が書く。前述の例外である。

<details><summary>根拠</summary>

- ruler が書く列: `base` / `rule_id` / dedup 根拠 2 列（INSERT/UPDATE）
- 完了済みの削除: `DeleteFulfilledReservationsBySiteAndProgramIDs`（原本 `media_asset` 持ち）
- ブレーカーの対象の削除: `DeleteReservationsBySiteAndProgramIDs`
- ブレーカーの外の削除: `DeleteReleasedReservationsBySiteAndProgramIDs`
- 番組スナップショットの GC: `epg.retention_grace` 経過後に `runGC`（§3.7）。CASCADE 先は `reservations` / `program_intents` / `program_overrides`
- ルール削除時の DELETE: `program_investments` に行が無い予約のみ。api の `DeleteRule`

</details>

#### API の書き込み

API は予約の表に INSERT/UPDATE しない。手動予約は、ユーザー意図に record を書くだけである。行自体は、次の ruler パスが投資を desired に含めることで生成する（[録画エンジン](../recording.md) §4.4）。

<details><summary>根拠</summary>

- 手動予約の書き込み: `program_intents` に `action='record'`
- 投資の判定: `program_investments`（§3.5）

</details>

## 3.5 program_intents / program_overrides — 番組単位のユーザー意図（永続）

意図と上書きの 2 表は API だけが書き、ruler は読むだけである。表を 2 つに分けるのは、録る・録らないの主張とパラメータの上書きが、ユーザーが番組について主張する独立の 2 つのことだからである。1 表にすると、パラメータだけ上書きして録る録らないは意見なし、という状態を表現できない。

```sql
CREATE TABLE program_intents (
    site       text   NOT NULL,
    program_id bigint NOT NULL,
    action     text   NOT NULL CHECK (action IN ('record', 'skip')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id),
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
        ON DELETE CASCADE
);

-- パラメータの上書き（program_intents とは別表。理由は下記）
CREATE TABLE program_overrides (
    site       text   NOT NULL,
    program_id bigint NOT NULL,
    overrides  jsonb  NOT NULL,   -- 上書きしたキーのみの疎なドキュメント
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id),
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
        ON DELETE CASCADE
);
```

### 2 表に分ける理由

表を 2 つに分けるのは、録る録らないの主張とパラメータの上書きが独立だからである。1 表だと、上書きだけして録る録らないは意見なし、という状態を表現できない。

予約（導出）とユーザー意図（永続）を分離する（[録画エンジン](../recording.md) §4.2）。**表を 2 つに分けるのは、ユーザーが番組について主張しうる 2 つのことが独立だからである。** 1 つは録る・録らないで、もう 1 つはパラメータの上書きである。

1 表に同居させると、action が NOT NULL のために「パラメータだけ上書きした。録る録らないについては意見なし」が表現できない。行が空になったときに、何を主張していた行かを行自身から読めなくなる。理由と具体的な誤動作は [録画エンジン](../recording.md) §4.2「overrides は `program_intents` とは別の表に置く」にある。

<details><summary>根拠</summary>

- 意図の列: `action NOT NULL`
- 別の表に置く理由: `program_intents` と `program_overrides`

</details>

### 意図の値

意図の値は、録れと録るなの 2 つである。録れは、手動予約と、重複排除スキップの明示的な無効化を表す。録るなは、番組単位の除外を表す。

意図がスキップで、かつ上書きが無い番組は、予約に行を持たない（overrides があればスキップでも行は残る。下記「program_investments」参照）。

<details><summary>根拠</summary>

- 値: `action` は `record`（録れ）/ `skip`（録るな）

</details>

### 上書きペイロードの扱い

上書きの内容には、CHECK を置かない。上書きの表自身のロジックが内容を一切使わない不透明なペイロードなので、jsonb を許している。

内容を検査する制約は、技術的には可能である。しかし「クエリはしないが制約はする」という中途半端な状態を作らない。**空の上書き = 行が無い**で表し、マージも Go 側で、型付きフィールドとして行う。SQL では行わない。

<details><summary>根拠</summary>

- 不透明なペイロード: `overrides`
- 可能だが置かない制約: `jsonb_strip_nulls(overrides) <> '{}'` など
- マージの型: `reservation.Options`

</details>

### 所有権・GC・サイトの範囲

書き込み所有権は API のみである。ruler は base を再計算するだけで、この 2 表に触らないので、手動編集が構造的に上書きされない。

GC は、番組スナップショットへの外部キーが CASCADE なので、番組終了後のスナップショット GC（§3.7）に連動して自動的に落ちる。サイトスコープを持つのは、「サイト A では録らない、B では録る」が N 予約の下では意味を持つためである（[録画エンジン](../recording.md) §3.1）。SSE ヒントは、どちらも予約のトピックに寄せる。意図の変更は、予約一覧と番組表の両方に現れる。

<details><summary>根拠</summary>

- 連動して落ちる設定: `program_snapshots` への FK が `ON DELETE CASCADE`
- SSE のトピック: `reservations`
- 再計算される列: `base`

</details>

### 行の存在と予約の保持

予約を保つのは、record 意図の行と上書きの行である。skip 意図の行は、単独では逆に予約を desired から外す側にいる。

これは §3「意図が skip で、かつ上書きが無い番組は予約行を持たない」の通りである。そのため、行の存在で予約を保つ意味は、意図と上書きで同じにならない。上書きの行があるだけで予約が保たれるのは、「overrides あり → 削除せず detached で保持」（[録画エンジン](../recording.md) §4.3）の要求である。この 2 条件（record 意図と上書きの和集合）は、投資の view が 1 箇所にまとめて定義している。

<details><summary>根拠</summary>

- 予約を保つ行: `program_intents` は `action = 'record'` の行に限る。`program_overrides` は行があるだけで保たれる
- 2 条件の定義: `program_investments` view

</details>

### 取消の書き方

取消は、無条件にスキップ意図を書いて導出行を落とす。行を消すだけでは、「消された行」と「最初から無かった行」が ruler から区別できず、次の全量パスが復活させる。

<details><summary>根拠</summary>

- 取消の書き込み: `intent{skip}`

</details>

### program_investments — 「この番組にユーザーの投資があるか」の一級述語

投資の view は、「この番組にユーザーの投資があるか」を 1 箇所で定義する一級の述語である。record 意図の行か上書きの行があれば、投資がある。

```sql
CREATE VIEW program_investments AS
SELECT site, program_id FROM program_intents WHERE action = 'record'
UNION
SELECT site, program_id FROM program_overrides;
```

削除ガード（ruler の導出削除・ルール削除の detached 判定）と、desired の union の第 2・3 項は、同じ述語を指す。だが view 化前は 4 箇所に散在し、2 つの形（意図の表を action で絞るかどうか）が併存していた。パラメータを持たない集合演算なので、view で足りる。定義を変えると、4 箇所の消費者すべてのテストが落ちる。

<details><summary>根拠</summary>

- ルール削除の判定: `DeleteRule`
- 絞る対象: `program_intents` を `action` で絞るか

</details>

## 3.6 circuit_breakers — 大量削除ブレーカーのラッチ

ブレーカーのラッチは、行の存在そのものが発動中を表し、再開は行の削除で行う。誰かが確認したという事実は再取得できないので、導出できない状態を持つ唯一の表である。

```sql
CREATE TABLE circuit_breakers (
    site       text NOT NULL,
    name       text NOT NULL,              -- internal/breaker の定数（ruler_deletes 等）
    tripped_at timestamptz NOT NULL DEFAULT now(),
    pending    integer NOT NULL,            -- 発動時に止めた件数
    threshold  integer NOT NULL,            -- 発動時の閾値（設定変更で変わるので焼く）
    detail     jsonb NOT NULL DEFAULT '{}', -- 何が消されようとしていたかの抜粋
    PRIMARY KEY (site, name)
);
```

<details><summary>根拠</summary>

- 同じ規律の表: `program_overrides`（行の存在が意味を持つ）

</details>

### 導出できない状態

停止していない状態を表す行は無い（§3.5 の上書きの表と同じ規律）。このスキーマで、導出できない状態を持つのはこの表だけである。他の表はすべて desired（ユーザーが書く）・observed（再取得できる）・導出結果のどれかである。**誰かが確認したという事実は再取得できない。** レベルトリガー設計の例外として、意図的に置く（[録画エンジン](../recording.md) §3.2「発動はラッチ」）。

### 発動時刻と閾値の扱い

発動時刻は、再発動で更新しない。いつから止まっているかが運用上の関心事なので、パスごとに現在時刻へ進めてはならない。閾値は、設定変更で変わるので、発動時の値を焼く。

<details><summary>根拠</summary>

- 発動時刻: `tripped_at`
- 発動時の値: `threshold`（閾値）/ `pending`（止めた件数）

</details>

### 名前と詳細に CHECK を置かない

名前に CHECK を置かない。ブレーカーの追加をマイグレーションなしでできるようにするためで、値の権威は Go 側の定数である。§1「型の規律」の「状態は text + CHECK」の例外だが、この列が持つのは識別子であり、状態は持たない。

詳細にも、内容の CHECK を置かない。手動確認のための材料であり、ブレーカー自身のロジックは中身を一切使わない不透明なペイロードである（UI が表示するだけ）。

<details><summary>根拠</summary>

- 名前の列: `name`。権威は `internal/breaker` の定数（`ruler_deletes` 等）
- 詳細の列: `detail`

</details>

### SSE トピック

SSE は、専用のトピックで流す。既存の予約・ルール・録画のトピックとは、関心事が違うためである（「トピック名はクライアントの関心事に揃え、テーブル名には揃えない」）。

<details><summary>根拠</summary>

- 専用のトピック: `breakers`
- 既存のトピック: `reservations` / `rules` / `recordings`

</details>

## 3.7 program_snapshots — 番組の事実のスナップショット

番組スナップショットは、番組の事実を持つ表である。値の出所は EPG 射影ただ 1 つで、クライアントからは受け取らず、射影にある間は追従し、消えたら凍結する。消すのは終了後の GC の 1 経路だけで、予約・意図・上書きは連動して落ちるが、録画履歴は残る。

EPG 射影は使い捨てキャッシュなので、番組が射影から消えても、予約・意図・上書きの GC 判定と UI 表示が成立し続けるために、この表を置く。

**3 表がそれぞれコピーを持つ形にしてはならない。** 書き込み時点が違うのでドリフトし、GC の判定が表ごとに違う時刻を使うことになる（[invariants.md](../invariants.md) §12）。

```sql
CREATE TABLE program_snapshots (
    site        text        NOT NULL,
    program_id  bigint      NOT NULL,
    title       text        NOT NULL DEFAULT '',
    start_at    timestamptz NOT NULL,
    duration_ms bigint      NOT NULL,
    -- チャンネル・放送イベント識別。
    network_id   integer NOT NULL,
    service_id   integer NOT NULL,
    channel_type text    NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel      text    NOT NULL,
    event_id     integer NOT NULL,
    service_name text    NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);
```

<details><summary>根拠</summary>

- EPG 射影: §9。GC 対象の 3 表: `reservations` / `program_intents` / `program_overrides`

</details>

### 値の出所と追従

値の出所は EPG 射影ただ 1 つで、クライアントからは受け取らない。射影にある間は毎パス追従し、消えたら凍結する。

書き手は API と ruler の 2 人だが、どちらも射影から引くので値の権威は割れない。クライアントからは受け取らない（サーバー権威）。射影に番組がなければ 400 を返す。

API は、意図・上書きの作成時に射影から番組を引く。ruler は、毎パス射影から upsert する。射影にある間は更新し、消えたら凍結する。

延長・繰り下げで EPG 側の時刻は変わるので、射影に番組がある間は毎パス追従する。凍結しっぱなしにしないのは、GC 判定・容量超過判定（[データ層](../data.md) §6.5）が、予約の時刻を需要区間として使うためである。予約が無く skip 意図だけが行を支える場合も、skip 意図を導出値と混同せず、番組が射影にある限り ruler が追従させる。

<details><summary>根拠</summary>

- API の書き込み: `ensureProgramSnapshot` が `GetProgramSnapshotSource` で `epg_programs ⋈ epg_services` から引く
- ruler の書き込み: `UpsertProgramSnapshotsFromProjection`

</details>

### program_id 再利用による付け替え窓

**未解決: 番組 ID の再利用で、スナップショットや欠測行が新番組に付け替わる窓がある。** identity は変更せず、検出器で実測を試みる。検出器はスナップショットが生きている窓しか観測できず、カウンタ 0 は再利用が無い根拠にならない。

EPG の放送地平を約 8 日とし、旧番組 A と新番組 B が同時には射影されないと仮定する。

窓は 2 つある。(a) 番組スナップショットは A の終了 + 保持猶予まで残る。そのため B の開始が A の終了から 8 日を超えて 8 日 + 保持猶予以内の約 1 日幅で、A のスナップショットが B に上書きされる。(b) 欠測表の A の欠測行は、保持猶予 + 30 日生きる。そのため B の開始が A の終了から 8 日を超えて既定の 31 日以内なら、約 23 日幅で、B が A の欠測として扱われる。

検出器は、番組スナップショットと EPG 射影の結合を引く以上、観測できるのは A のスナップショットがまだ生きている窓 (a) だけである。窓 (b) は、スナップショットが先に GC された後の話なので、検出器には原理的に映らない。したがって、カウンタが 0 でも「再利用が起きていない」ことの根拠にはならず、「窓 (a) では観測されなかった」ことしか意味しない。

<details><summary>根拠</summary>

- 再利用される番号: `program_id`
- 窓 (a) の対象: `program_snapshots`（A の終了 + `retention_grace` まで残る。幅は `8日 + retention_grace` 以内）
- 窓 (b) の対象: `never_scheduled_events`（`retention_grace + 30日`）
- 検出器: `ListProgramIDReusesBySite` / `rokuban_ruler_program_id_reuse_total`
- 検出器が引く結合: `program_snapshots ⋈ epg_programs`

</details>

### チャンネルと放送イベントの識別

チャンネル識別と放送イベント識別は、API のフィールドから素直に引いてスナップショットする。programId を分解して逆算しない。

Mirakurun 互換の programId は、ネットワーク・サービス・イベントの 3 つ組を合成した規則を持つが、本番コードでこれを逆算してはならない。reconciler の contentPath 生成は、このスナップショットを読む。容量超過の判定（[データ層](../data.md) §6.5）の需要単位が、チャンネル種別とチャンネルの組なので、使い捨ての EPG 射影への JOIN に頼らずここを読む。

イベント ID とサービス名も、同じ経路でスナップショットする。reconciler が欠測行を書くときに、放送イベントの識別が要る。watcher が録画履歴を作るときにも、表示名が要る。イベント ID は、他のチャンネル識別列と同様に EPG 射影から素直に引き、programId を分解して逆算しない。

<details><summary>根拠</summary>

- 合成規則: `NID*10^10 + SID*10^5 + EID`
- 素直に引く列: `network_id` / `service_id` / `channel_type` / `channel`
- 読む側: reconciler の contentPath 生成。需要単位は `(channel_type, channel)`
- 欠測を書く側: `reconciler.recordNeverScheduled` が `never_scheduled_events` に書く。識別は `(network_id, service_id, event_id)`
- 表示名: watcher が `recordings` を作るときの `service_name`。`event_id` は `epg_programs.event_id` から引く

</details>

### 識別列の NOT NULL

チャンネル・放送イベント識別の 6 列は NOT NULL である。新規書き込みの経路は 2 つで、どちらも EPG 射影への INNER JOIN で NULL を書けない。reconciler 側の「NULL なら推測せず schedule を作らない / 試行行を作らない」という分岐も、この状態が表現不可能になったことで削除している（不変条件 10）。

<details><summary>根拠</summary>

- 6 列: `network_id` / `service_id` / `channel_type` / `channel` / `event_id` / `service_name`
- 書き込みの 2 経路: `GetProgramSnapshotSource` / `UpsertProgramSnapshotsFromProjection`。結合先は `epg_programs` / `epg_services`

</details>

### GC と削除経路

GC は、番組スナップショットからの 1 本の DELETE に集約している。参照の無い行を掃除する規則は、意図の作成とレースするので足してはならない。

予約・意図・上書きは、この表への外部キーを CASCADE で持つので、1 本で 3 表とも一緒に落ちる。録画履歴は予約への外部キーを持たないので、この削除で録画履歴とメディアアセットは失われない。

**この表からの DELETE 経路は GC 1 本に限定すること。** 他の場所から消せると、意図を巻き添えにする。特に「参照が 1 つも無いスナップショット行を掃除する」規則を足してはならない。掃除しないなら害はなく、GC が拾う。だが掃除規則は、意図の作成とレースする。ruler の導出削除が並行して作られた手動予約を消したのと同じ形である（[invariants.md](../invariants.md) §9「適用の瞬間」）。

録画履歴は、この外部キーの対象外である。録画時点のスナップショット（§5）として独立にコピーを持つため、番組終了後に番組スナップショットが消えても、録画履歴には影響しない。

<details><summary>根拠</summary>

- GC の DELETE: `DeleteEndedProgramSnapshots`。条件は `start_at + duration_ms < now() - epg.retention_grace`
- CASCADE の対象: `reservations` / `program_intents` / `program_overrides`（`(site, program_id)` FK を `ON DELETE CASCADE` で持つ）
- 外部キーを持たない表: `recordings` / `media_assets`

</details>
