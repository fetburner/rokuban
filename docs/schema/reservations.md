> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§3 / §3.5 / §3.6 / §3.7）。

## 3. reservations — 予約（desired state）

ルール評価の純粋な出力。手動予約もルール由来の予約も同じテーブルに入り、区別はテーブルの列ではなく `program_intents.action` の有無から導出する（`action='record'` があれば手動予約）。

この表に残るのは **ruler の 1 パスの出力**（`rule_id` / `base` / dedup 根拠 2 列）だけで、**導出の書き手は ruler ただ 1 人**（不変条件 12「1 表 = 1 つの書き手 = 1 つの寿命」）。番組の事実は `program_snapshots`（§3.7）、不可逆な観測は `recordings` の試行行（[recordings.md](recordings.md) §5）が持つ。**この 3 つを 1 行に同居させてはならない**（[invariants.md](../invariants.md) §12）。

例外が 1 つだけある: `DELETE /api/rules/{id}`（ルール削除）は、ユーザーの投資（`program_investments`）がない予約行をルール削除と同一トランザクションで同期削除する。1 表に書き手が 2 人いる形（不変条件 12 の兆候）だが、両者の DELETE 文はいずれも WHERE で `program_investments`（intent / overrides）を**適用の瞬間に再評価**するため、導出の判定と適用の間に並行して着地する手動予約を踏み潰す窓（[invariants.md](../invariants.md) §9「適用の瞬間」）は生じない。同期を選んだ理由は [録画エンジン](../recording.md) §4.4「取消は `PUT .../intent {action: skip}`」に詳しい。

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

- 同一の BS/CS 番組が複数サイトの EPG に現れた場合、site が違えば別予約になる（両サイトで録る、が表現可能）。サイト横断の重複排除はルール側の関心事（履歴ベース重複排除で扱う）
- **番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）はこの表にはない。** `(site, program_id)` の FK で参照する `program_snapshots`（§3.7）にある
- **不可逆な観測を書く列も無い。** 「番組終了後に schedule が観測されなかった」という観測は `recordings` の試行行が持つ（[recordings.md](recordings.md) §5「行の作られ方」）。この事実は短命な導出表ではなく履歴側に置く --- 導出表に残すと ruler の再実体化で消え、`recordings_unique_active_event` による「1 放送イベントに active な試行は 1 行」の宣言的な強制も効かない

### 重複排除の根拠に FK を張らない

`dedup_match_recording_id` は `recordings(id)` を指すが **FK を張っていない**。`REFERENCES recordings (id) ON DELETE SET NULL` と上の CHECK は両立しないためである。FK アクションは FK 側の列しか NULL にできないので、参照されている `recordings` 行を物理削除すると `(NULL, 0.87)` という行ができて CHECK に違反し、**DELETE 自体が中断する**（後片付けの機構が安全網に引っかかって削除を不可能にする）。

外すのは FK の方にした。

- 根拠 2 列は ruler が毎パス作り直す導出値（不変条件 9）。参照先が消えても次のパスで両方 NULL に戻るので、**孤立は自己修復する**
- `recordings.id` は `GENERATED ALWAYS AS IDENTITY` で再利用されないため、孤立した id が別の録画を指すことは構造的に起きない（FK を外して一番怖い失敗モードが無い）
- §5 の tombstone 契約により、本番では `recordings` 行を物理削除しない。FK が守っているのは起きない事象
- CHECK は「あってはいけない組み合わせを表現不可能にする」（不変条件 10）を担っている。仕事をしているのはこちら

### base / overrides の意味論

- **effective = COALESCE(base, '{}') ⊕ program_overrides.overrides**。reconciler が mirakc へ同期し、ingest / encode が参照するのは常に effective。解決は `reservation.EffectiveOptions` の 1 箇所に集約し、jsonb の Unmarshal 失敗を握りつぶさない。`reservation.Options.IsSkipped()` が `effective.skip` の判定に名前を付けている
- base と overrides は**同形の jsonb ドキュメント**（§8）。ruler は EPG 更新のたびに base を丸ごと再計算してよく、**overrides は別表なので構造的に触れない**
- **`skip` は overrides のキーではなく `program_intents.action`**。列なので base 側の skip に対する優先順位が明示的に決まる（`action = 'skip'` が勝つ）。**意図が skip で、かつ上書きが無い**番組は予約行を持たない（overrides があれば skip でも行は残る。下記「program_investments」参照）

### active / detached / orphaned は API が都度導出する

`state` という列は存在しない。**この状態を列に焼いてはならない** --- 導出値と不可逆な観測を 1 列に潰す形そのもので、実装は式ではなく前パスからの遷移を書くことになり、片側の分岐しか持たなくなる（[invariants.md](../invariants.md) §9「式」）。予約の状態は API 層（`internal/api/handler.go` の `reservationState`）が読むたびに計算して返す。`active` / `detached` は `(rule_id, base)` から、`orphaned` は **「この予約の放送イベントに `never_scheduled_events` の欠測行があり、かつ同じイベントの `recordings` 行が 1 つも無いか」**（`GetReservationFull` / `ListReservationsFull` の `never_recorded` 列）から導出する。**「schedule が観測されなかった」は `epg_last_seen_at` のようなタイムスタンプからは導出できない** --- 観測側が事実として欠測行を書く必要がある。予約行の結合は不安定な導出 id ではなく、放送イベント `(site, network_id, service_id, event_id)` を使う（不変条件 9 の identity。`internal/db/queries/reservations.sql` のコメントが権威）。

| 値 | 意味 | 導出元 |
|---|---|---|
| `active` | 通常の desired 予約 | `rule_id IS NOT NULL`（または base が無い manual 予約） |
| `detached` | ルールがマッチしなくなったが `record` 意図または上書きがある行（= `program_investments` view に行がある）。base は凍結され、実質 manual として動く（`intent{skip}` なら録画しない detached） | `rule_id IS NULL AND base IS NOT NULL` |
| `orphaned` | **この予約に対応する放送イベントについて、一度も schedule が観測されなかった欠測行があり、本物の録画試行は 1 行も無い**。mirakc 由来の途中失敗は欠測表に入らない（再試行経路を壊さない）。即削除せず残して「録れなかった」を説明可能にする | `never_scheduled_events` 表の EXISTS と、同じ放送イベントの `recordings` 全履歴に対する NOT EXISTS の積（`GetReservationFull` の `never_recorded`）。recordings は live 限定にしないため、本物の録画をごみ箱に入れても orphaned に戻らない。放送イベントキーは `program_snapshots` を経由して引く --- 予約行の導出 id を結合先にしてはならない（[invariants.md](../invariants.md) §9「identity」） |

- **行の物理削除（GC）は「番組の終了時刻を過ぎた後」のみ**。番組の終了時刻は `program_snapshots.start_at + duration_ms` で判定し（§3.7）、`reservations` は `program_snapshots` への FK が `ON DELETE CASCADE` なのでスナップショットが GC された瞬間に一緒に落ちる（active/detached/orphaned のいずれでも問わない）。`never_scheduled_events` は `program_snapshots` への FK を持たないので、同時には消えないが、放送地平を超える `retention_grace + 30日` で別途刈られる（[recordings.md](recordings.md) §5）
- 意図も上書きもない active 予約がルール・EPG から消えた場合は通常の宣言的動作として削除（ただし大量削除サーキットブレーカーの対象）。なお放送開始直前（`ruler.retract_grace` 以内）は猶予で削除しない --- 猶予中の行は `rule_id` が前パスのまま据え置かれるので、この表の `active` の導出（`rule_id IS NOT NULL`）はそのまま成立し続ける。専用の状態は増やさない（[ruler.md](../recording/ruler.md)「直前 unmatch の猶予」）
- ルール再マッチで base 再計算のうえ `active` に戻る（overrides は無傷）

**同期対象かのフィルタに使ってよいのは「この予約に対応する放送イベントに `never_scheduled_events` の欠測行が無いこと」だけ**（`ListReservationsForSyncEvaluation` が絞る）。

1. **`active` / `detached` をフィルタにしてはならない。** どちらも UI 表示用の派生値であり、同期の可否を決めるのは `effective.skip` である。導出値を同期フィルタに使うと、ルールが外れた**手動予約が黙って録画されなくなる**（[invariants.md](../invariants.md) §9）
2. **同期除外は欠測表の行の存在だけを見る。** 一度欠測と判定された放送イベントは、本物の record が後から来ても、行が寿命内にある限り同期対象に戻らない。終了済み予約を再び schedule しないため。この表の読者はいずれも `reservations` 行を経由して届き、その `reservations` は番組終了 + `retention_grace` で先に GC されるので、行を `retention_grace + 30日` まで残しても読者からは観測されない
3. **表示（`never_recorded`）は欠測行に加えて、本物の `recordings` 行が無いことも見る。** record が来たら orphaned は消えるが、欠測行はその寿命内では残る。recordings の照合は live 限定にしないため、ごみ箱操作で orphaned に戻らない
4. **どちらも mirakc 由来の途中失敗だけでは成立しない。** failed 試行は `recordings` にだけ入り欠測表には入らないため、再試行経路を壊さない

### 書き込み所有権

| カラム | 書く人 |
|---|---|
| `reservations` の `base` / `rule_id` / dedup 根拠 2 列（INSERT/UPDATE） | ruler（毎パス） |
| `reservations` の DELETE（desired から外れ、`program_investments` に行が無い予約） | ruler（毎パス。ルールが base を供給している行は大量削除サーキットブレーカーの対象で `DeleteReservationsBySiteAndProgramIDs`、ユーザーが投資を手放す書き込みをしない限り起きない行はブレーカーの外で `DeleteReleasedReservationsBySiteAndProgramIDs`。分類の条件と境界は[録画エンジン](../recording.md) §3.2） |
| `reservations` の DELETE（ルール削除時、`program_investments` に行が無い予約のみ） | api（`DeleteRule`。上記の例外） |
| `program_snapshots` の GC（番組終了 + `epg.retention_grace` 経過）と、そこからの FK CASCADE による `reservations` / `program_intents` / `program_overrides` の GC | ruler のパス（`runGC`。§3.7） |
| `program_intents`（action）、`program_overrides`（overrides） | api |

api は `reservations` に INSERT/UPDATE しない。手動予約は `program_intents` に `action='record'` を書くだけで、行自体は次の ruler パスが `program_investments`（§3.5）を desired に含めることで生成する（[録画エンジン](../recording.md) §4.4）。

## 3.5 program_intents / program_overrides — 番組単位のユーザー意図（永続）

**api だけが書き、ruler は読むだけ**の 2 表。予約（導出）とユーザー意図（永続）を分離する（[録画エンジン](../recording.md) §4.2）。

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

**表を 2 つに分けるのは、ユーザーが番組について主張しうる 2 つのことが独立だから**である（①録る / 録るな ②パラメータの上書き）。1 表に同居させると `action NOT NULL` のために「パラメータだけ上書きした。録る録らないについては意見なし」が表現できず、行が空になったときに何を主張していた行かを行自身から読めなくなる。理由と具体的な誤動作は [録画エンジン](../recording.md) §4.2「overrides は `program_intents` とは別の表に置く」。

- **`action`**: `record`（録れ = 手動予約 / dedup skip の明示的な無効化）/ `skip`（録るな = 番組単位の除外）。**意図が skip で、かつ上書きが無い番組は `reservations` に行を持たない**（overrides があれば skip でも行は残る。下記「program_investments」参照）
- **`overrides` に CHECK を置かない。** `program_overrides` 自身のロジックが内容を一切使わない不透明なペイロードだから jsonb を許している。内容を検査する制約（`jsonb_strip_nulls(overrides) <> '{}'` 等）は技術的には可能だが、「クエリをしない一方で制約をする」という中途半端な状態を作らない。**空の上書き = 行が無い**で表し、マージも SQL ではなく Go 側で `reservation.Options` の型付きフィールドとして行う
- **書き込み所有権**: api のみ。ruler は base を再計算するだけでこの 2 表に触らない → 手動編集が構造的に上書きされない
- **GC**: `program_snapshots` への FK が `ON DELETE CASCADE` なので、番組終了後のスナップショット GC（§3.7）に連動して自動的に落ちる
- **site スコープ**: 「サイト A では録らない、B では録る」が N 予約の下では意味を持つため（[録画エンジン](../recording.md) §3.1）
- SSE ヒントはどちらも `reservations` トピックに寄せる（意図の変更は予約一覧・番組表の両方に現れる）

**どちらの表も行の存在が予約を存在させる**（ruler の desired に入る）。ただし `program_intents` については `action = 'record'` の行に限る --- `action = 'skip'` の行は単独では逆に予約を desired から外す側（§3「意図が skip で、かつ上書きが無い番組は予約行を持たない」）なので、行の存在で予約を保つのは `program_overrides` と同じ意味にならない。`program_overrides` に行があるだけで予約が保たれるのは「overrides あり → 削除せず detached で保持」（[録画エンジン](../recording.md) §4.3）の要求。この 2 条件（`record` 意図 ∪ overrides）は `program_investments` view が 1 箇所にまとめて定義している。

取消は**無条件に `intent{skip}` を書いて導出行を落とす**。行を消すだけでは「消された行」と「最初から無かった行」が ruler から区別できず、次の全量パスが復活させる。

### program_investments — 「この番組にユーザーの投資があるか」の一級述語

```sql
CREATE VIEW program_investments AS
SELECT site, program_id FROM program_intents WHERE action = 'record'
UNION
SELECT site, program_id FROM program_overrides;
```

削除ガード（ruler の導出削除・`DeleteRule` の detached 判定）と desired の union の第 2・3 項が同じ述語を指すのに、view 化前は 4 箇所に散在し 2 つの形（`program_intents` を `action` で絞るかどうか）が併存していた。パラメータを持たない集合演算なので view で足りる。定義を変えると 4 箇所の消費者すべてのテストが落ちる。

## 3.6 circuit_breakers — 大量削除ブレーカーのラッチ

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

- **行の存在そのものが「発動中」**。停止していない状態を表す行は無い（§3.5 の
  `program_overrides` と同じ規律）。再開は行の DELETE
- **このスキーマで唯一の「導出できない状態」。** 他のテーブルはすべて desired（ユーザーが
  書く）か observed（再取得できる）か導出結果だが、**誰かが確認したという事実は再取得
  できない**。レベルトリガー設計の例外として意図的に置く（[録画エンジン](../recording.md)
  §3.2「発動はラッチ」）
- `tripped_at` は再発動で更新しない。「いつから止まっているか」が運用上の関心事なので、
  パスごとに現在時刻へ進めてはならない
- `name` に CHECK を置かない。ブレーカーの追加をマイグレーションなしでできるようにする
  ため、値の権威は Go 側の定数（`internal/breaker`）。§1「型の規律」の「状態は text +
  CHECK」の例外だが、これは状態ではなく識別子である
- `detail` に内容の CHECK を置かない。**手動確認のための材料**であり、ブレーカー自身の
  ロジックは中身を一切使わない不透明なペイロード（UI が表示するだけ）
- SSE は専用の `breakers` トピック。既存の reservations / rules / recordings とは
  関心事が違う（「トピック名はテーブル名ではなくクライアントの関心事に揃える」）

## 3.7 program_snapshots — 番組の事実のスナップショット

EPG プロジェクション（§9）は使い捨てキャッシュなので、番組が射影から消えても `reservations` / `program_intents` / `program_overrides` の GC 判定と UI 表示は成立し続けなければならない。この「番組の事実」を持つのがこの表。**3 表がそれぞれコピーを持つ形にしてはならない** --- 書き込み時点が違うのでドリフトし、GC の判定が表ごとに違う時刻を使うことになる（[invariants.md](../invariants.md) §12）。

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

- **値の出所は EPG プロジェクションただ 1 つ。** 書き手は api（意図・上書きの作成時。`ensureProgramSnapshot` が `GetProgramSnapshotSource` で `epg_programs ⋈ epg_services` から引く）と ruler（毎パス、`UpsertProgramSnapshotsFromProjection`）の 2 人だが、両者とも射影から引くので値の権威は割れない。**クライアントからは受け取らない**（サーバー権威）。射影に番組がなければ 400
- **射影にある間は更新、消えたら凍結。** 延長・繰り下げで EPG 側の時刻は変わるので、射影に番組がある間は毎パス追従し、消えたときに凍結する。凍結しっぱなしにしないのは、GC 判定・容量超過判定（[データ層](../data.md) §6.5）が予約の時刻を需要区間として使うため。予約が無く skip 意図だけが行を支える場合も、skip 意図を導出値と混同せず、番組が射影にある限り ruler が追従させる
- **チャンネル識別はスナップショットする（programId を分解しない）。** Mirakurun 互換の programId は `NID*10^10 + SID*10^5 + EID` という合成規則を持つが、本番コードでこれを逆算してはならない。`network_id` / `service_id` / `channel_type` / `channel` は API のフィールドから素直に引く
  - reconciler の contentPath 生成はこのスナップショットを読む
  - 容量超過の判定（[データ層](../data.md) §6.5）の需要単位が `(channel_type, channel)` なので、使い捨ての EPG 射影への JOIN に頼らずここを読む
- **`event_id` / `service_name` も同じ経路でスナップショットする。** `reconciler.recordNeverScheduled` が `never_scheduled_events` に欠測を書くときに放送イベントの識別 `(network_id, service_id, event_id)` が要り、watcher が `recordings` を作るときに表示名 `service_name` が要る。`event_id` は他のチャンネル識別列と同様に `epg_programs.event_id` から素直に引き、**programId を分解して逆算しない**
- **チャンネル・放送イベント識別 6 列は NOT NULL。** 新規書き込みの 2 経路（`GetProgramSnapshotSource` / `UpsertProgramSnapshotsFromProjection`）はどちらも `epg_programs` / `epg_services` への INNER JOIN で NULL を書けない。reconciler 側の「NULL なら推測せず schedule を作らない / 試行行を作らない」という分岐も、この状態が表現不可能になったことで削除している（不変条件 10）
- **GC はこの表からの 1 本の DELETE に集約されている**（`DeleteEndedProgramSnapshots`。条件は `start_at + duration_ms < now() - epg.retention_grace`）。`reservations` / `program_intents` / `program_overrides` はこの表への `(site, program_id)` FK を `ON DELETE CASCADE` で持つので、この 1 本の DELETE で 3 表とも一緒に落ちる。`recordings` は `reservations` への FK を持たないので、この削除で録画履歴（recordings/media_assets）が失われることはない
- **この表からの DELETE 経路は GC 1 本に限定すること。** 他の場所から消せると意図を巻き添えにする。特に「参照が 1 つも無いスナップショット行を掃除する」規則を足してはならない --- 掃除しないなら害はない（GC が拾う）が、掃除規則は intent の作成とレースする（ruler の導出削除が並行して作られた手動予約を消したのと同じ形。[invariants.md](../invariants.md) §9「適用の瞬間」）
- `recordings` はこの FK の対象外。録画時点のスナップショット（§5）として独立にコピーを持つため、番組終了後に `program_snapshots` が消えても録画履歴には影響しない
