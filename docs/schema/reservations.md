## 3. reservations — 予約（desired state）

ルール評価の純粋な出力。手動予約もルール由来の予約も同じテーブルに入り、区別はテーブルの列ではなく `program_intents.action` の有無から導出する（`action='record'` があれば手動予約。issue #26）。

Phase 1（#27 / #28 / #30）で「導出できないもの」を全て別表・別列に引き剥がした結果、この表に残るのは**ruler の 1 パスの出力**（`rule_id` / `base` / dedup 根拠 2 列）だけになった（CLAUDE.md 不変条件 12「表は行の寿命で割る」）。Phase 1 では不可逆な観測（`orphaned_at`）がまだ残っていたが、issue #98 でこれも `recordings` の試行行（§5「行の作られ方」）に移設され、`reservations` は文字どおり「ruler の 1 パスの出力」だけになった --- **書き手が ruler ただ 1 人**になったことで、不変条件 12「1 表 = 1 つの書き手 = 1 つの寿命」が完全に成立する。

```sql
CREATE TABLE reservations (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    site              text   NOT NULL,          -- 設定ファイル定義のサイト名
    program_id        bigint NOT NULL,          -- mirakc/Mirakurun の programId（site 単位のスコープ）
    rule_id           bigint,                   -- 勝者ルール。REFERENCES rules(id) ON DELETE SET NULL

    -- 予約オプションの導出側（issue #2 の base/overrides コメント）。
    -- ユーザーの上書きは program_overrides 表にあり、この行には載らない（§3.5）
    base              jsonb,                    -- ruler だけが書く。manual では NULL

    -- 重複排除の判定根拠（M2-6 で追加。00013）。ruler が毎パス作り直す導出列で、
    -- base と同じ凍結規則に従う。FK は張らない（後述）
    dedup_match_recording_id bigint,
    dedup_similarity         real,
    CHECK ((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL)),

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    UNIQUE (site, program_id),  -- desired 予約は site x programId につき最大 1 つ
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots (site, program_id)
        ON DELETE CASCADE
);

CREATE INDEX ON reservations (rule_id);
```

**`orphaned_at` 列は issue #98 で廃止した（00025）。** Phase 1（#28 / #30）でこの列は「番組終了後に mirakc へ schedule が観測されなかった」という不可逆な観測を持ち、書き手は reconciler の `markOrphaned` だけだった。この観測は「捕獲試行の履歴」そのものであり、このスキーマには履歴を置く場所が既に決まっている（`recordings`。§5「録画されなかった試行も履歴に残る」）。`reservations` 側に残す理由は無く、`recordings` に置けば `recordings_unique_active_event`（§5「同一イベントの重複防止」）が「1 放送イベントに active な試行は 1 行」を宣言的に強制するという副産物も得られる。詳細な設計判断は issue #98 のコメントと §5「行の作られ方」参照。

- 同一の BS/CS 番組が複数サイトの EPG に現れた場合、site が違えば別予約になる（両サイトで録る、が表現可能）。サイト横断の重複排除はルール側の関心事（M2 の履歴ベース重複排除で扱う）
- **番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）はこの表にはない。** `(site, program_id)` の FK で参照する `program_snapshots`（§3.7）に抽出された（#27）

### 重複排除の根拠に FK を張らない（M2-6）

`dedup_match_recording_id` は `recordings(id)` を指すが **FK を張っていない**。`REFERENCES recordings (id) ON DELETE SET NULL` と上の CHECK は両立しないためである。FK アクションは FK 側の列しか NULL にできないので、参照されている `recordings` 行を物理削除すると `(NULL, 0.87)` という行ができて CHECK に違反し、**DELETE 自体が中断する**（後片付けの機構が安全網に引っかかって削除を不可能にする）。

外すのは FK の方にした。

- 根拠 2 列は ruler が毎パス作り直す導出値（不変条件 9）。参照先が消えても次のパスで両方 NULL に戻るので、**孤立は自己修復する**
- `recordings.id` は `GENERATED ALWAYS AS IDENTITY` で再利用されないため、孤立した id が別の録画を指すことは構造的に起きない（FK を外して一番怖い失敗モードが無い）
- §5 の tombstone 契約により、本番では `recordings` 行を物理削除しない。FK が守っているのは起きない事象
- CHECK は「あってはいけない組み合わせを表現不可能にする」（不変条件 10）を担っている。仕事をしているのはこちら

### base / overrides の意味論

- **effective = COALESCE(base, '{}') ⊕ program_overrides.overrides**。reconciler が mirakc へ同期し、ingest / encode が参照するのは常に effective。解決は `db.EffectiveOptions` の 1 箇所に集約し、jsonb の Unmarshal 失敗を握りつぶさない。`db.ReservationOptions.IsSkipped()` が `effective.skip` の判定に名前を付けている
- base と overrides は**同形の jsonb ドキュメント**（§8）。ruler は EPG 更新のたびに base を丸ごと再計算してよく、**overrides は別表なので構造的に触れない**
- **`skip` は overrides のキーではなく `program_intents.action`**。列なので base 側の skip に対する優先順位が明示的に決まる（`action = 'skip'` が勝つ）。skip された番組は**予約行を持たない**

### active / detached / orphaned は API が都度導出する（Phase 1。#28 / #30）

`state` という列はもう存在しない。予約の状態は API 層（`internal/api/handler.go` の `reservationState`）が読むたびに計算して返すが、issue #98 で材料が変わった: `active` / `detached` は引き続き `(rule_id, base)` から、`orphaned` は `orphaned_at` 列（Phase 1）ではなく **「この予約に `status='failed'` の `recordings` 行が存在するか」という EXISTS 判定**（`GetReservationFull` / `ListReservationsBySite` の `never_recorded` 列）から導出する。

| 値 | 意味 | 導出元 |
|---|---|---|
| `active` | 通常の desired 予約 | `rule_id IS NOT NULL`（または base が無い manual 予約） |
| `detached` | ルールがマッチしなくなったが `record` 意図または上書きがある行（= `program_investments` view に行がある。issue #162）。base は凍結され、実質 manual として動く | `rule_id IS NULL AND base IS NOT NULL` |
| `orphaned` | **この予約について捕獲の試みが失敗した（一度も schedule が作られなかった、または途中で失敗した）行**。即削除せず残して「録れなかった」を説明可能にする | `EXISTS (SELECT 1 FROM recordings WHERE reservation_id = r.id AND status = 'failed')` |

- **行の物理削除（GC）は「番組の終了時刻を過ぎた後」のみ**。番組の終了時刻は `program_snapshots.start_at + duration_ms` で判定し（§3.7）、`reservations` は `program_snapshots` への FK が `ON DELETE CASCADE` なのでスナップショットが GC された瞬間に一緒に落ちる（active/detached/orphaned のいずれでも問わない）。`recordings` は `program_snapshots` への FK を持たないので、GC された後も orphaned だったという記録は `recordings` 側に残り続ける（§5「行の作られ方」）
- 意図も上書きもない active 予約がルール・EPG から消えた場合は通常の宣言的動作として削除（ただし大量削除サーキットブレーカーの対象）
- ルール再マッチで base 再計算のうえ `active` に戻る（overrides は無傷）

**同期対象かのフィルタに使ってよいのは「この予約に never-scheduled の `recordings` 行が無いこと」だけ**（`ListReservationsForSyncEvaluation` が絞る。issue #98 で `orphaned_at IS NULL` から置き換わった。API 表示用の `never_recorded` より狭い述語であることに注意 --- 同期除外は「一度も schedule が作られなかった」という `recording.never-scheduled` マーカーだけを見て、mirakc 由来の途中失敗（`recording.failed`）までは除外しない。理由は下記コラム参照）。`active` / `detached` は UI 表示用の派生値であり、同期の可否を決めるのは `effective.skip` である。かつて `state` 列が両方を兼ねていたため、`reconciler.listDesired` が `state = 'active'` でしか絞らず detached の予約に schedule が作られない、という「手動予約 → たまたまルールがマッチ → そのルールを編集して外す → ユーザーの手動予約が黙って録画されなくなる」不具合があった（M2-4 で修正。[録画エンジン](../recording.md) §4.3）。

> **`state` 列を残したことで、同じクラスの不具合が 2 件再発した**（[#30](https://github.com/fetburner/rokuban/issues/30)）。①ruler は導出の式ではなく**前パスの `rule_id` を見た遷移**を SQL の CASE で書いていたため、ルールを**削除**した経路（FK の `ON DELETE SET NULL` が先に `rule_id` を落とす）では `detached` にならず、`DELETE /api/rules/{id}` が返す `detachedReservations` の件数と予約一覧のバッジが一致しなかった。②`MarkReservationOrphaned` に `AND state = 'active'` が残っていたため、**detached 予約が永久に `orphaned` にならなかった**（M2-4 では `listDesired` 側だけを直した）。**Phase 1 で `state` 列そのものを撤去し、active/detached を読むたびに評価する形にしたことで、この 2 件は構造的に再発しなくなった**（前パスの遷移を保存する列自体が無い）。`orphaned` も issue #98 で `orphaned_at` 列自体を廃したことで同じ形の再発余地が構造的に無くなった。

`orphaned` の導出が `EXISTS` で recordings を毎回問い合わせる形になったのは Phase 1 の逆方向の教訓でもある: 「schedule が観測されなかった」という事実は `epg_last_seen_at` のようなタイムスタンプからは導出できず、**観測側が事実として書いた行**が必要（状態機械を導出に寄せる案。[issue #18](https://github.com/fetburner/rokuban/issues/18) の案 B）。#98 の決定は「その行をどこに置くか」を `reservations.orphaned_at`（列）から `recordings`（別表の恒久行）へ動かしただけで、「観測側が能動的に書く」という性質自体は変わっていない。

### 書き込み所有権

| カラム | 書く人 |
|---|---|
| `reservations` の `base` / `rule_id` / dedup 根拠 2 列 | ruler（毎パス） |
| `program_snapshots` の GC（番組終了 + `epg.retention_grace` 経過）と、そこからの FK CASCADE による `reservations` / `program_intents` / `program_overrides` の GC | ruler のパス（`runGC`。§3.7） |
| `program_intents`（action）、`program_overrides`（overrides）、手動予約の作成・取消 | api |

`reservations` に不可逆な観測を書く列はもう無い（issue #98 で `orphaned_at` を廃止。§5「行の作られ方」の `recordings` が代わりにこの観測を持つ）。

## 3.5 program_intents / program_overrides — 番組単位のユーザー意図（永続）

**api だけが書き、ruler は読むだけ**の 2 表。予約（導出）とユーザー意図（永続）を分離する（issue #18 の案 A、[録画エンジン](../recording.md) §4.2）。

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

-- パラメータの上書き（M2-4 / 00010 で program_intents から分離）
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

- **`action`**: `record`（録れ = 手動予約 / dedup skip の明示的な無効化）/ `skip`（録るな = 番組単位の除外）。**skip された番組は `reservations` に行を持たない**
- **`overrides` に CHECK を置かない。** `program_overrides` 自身のロジックが内容を一切使わない不透明なペイロードだから jsonb を許している。内容を検査する制約（`jsonb_strip_nulls(overrides) <> '{}'` 等）は技術的には可能だが、「クエリはしないが制約はする」という中途半端な状態を作らない。**空の上書き = 行が無い**で表し、マージも SQL ではなく Go 側で `db.ReservationOptions` の型付きフィールドとして行う
- **書き込み所有権**: api のみ。ruler は base を再計算するだけでこの 2 表に触らない → 手動編集が構造的に上書きされない
- **GC**: `program_snapshots` への FK が `ON DELETE CASCADE` なので、番組終了後のスナップショット GC（§3.7）に連動して自動的に落ちる。以前はこの 2 表がそれぞれ独自に `program_start_at` / `program_duration_ms` を持ち、GC の比較対象にしていたが、Phase 1（#27）でこの列を落として `program_snapshots` に一本化した
- **site スコープ**: 「サイト A では録らない、B では録る」が N 予約の下では意味を持つため（[録画エンジン](../recording.md) §3.1）
- SSE ヒントはどちらも `reservations` トピックに寄せる（意図の変更は予約一覧・番組表の両方に現れる）

**どちらの表も行の存在が予約を存在させる**（ruler の desired に入る）。ただし `program_intents` については `action = 'record'` の行に限る --- `action = 'skip'` の行は逆に予約を desired から外す側（§3「skip された番組は予約行を持たない」）なので、行の存在で予約を保つのは `program_overrides` と同じ意味にならない。`program_overrides` に行があるだけで予約が保たれるのは §4.3「overrides あり → 削除せず detached で保持」の要求。この 2 条件（`record` 意図 ∪ overrides）は `program_investments` view（issue #162）が 1 箇所にまとめて定義している。

取消は**無条件に `intent{skip}` を書いて導出行を落とす**。行を消すだけでは「消された行」と「最初から無かった行」が ruler から区別できず、次の全量パスが復活させる。

### program_investments — 「この番組にユーザーの投資があるか」の一級述語（issue #162）

```sql
CREATE VIEW program_investments AS
SELECT site, program_id FROM program_intents WHERE action = 'record'
UNION
SELECT site, program_id FROM program_overrides;
```

削除ガード（ruler の導出削除・`DeleteRule` の detached 判定）と desired の union の第 2・3 項が同じ述語を指すのに、view 化前は 4 箇所に散在し 2 つの形（`program_intents` を `action` で絞るかどうか）が併存していた。パラメータを持たない集合演算なので view で足りる。定義を変えると 4 箇所の消費者すべてのテストが落ちる。

## 3.6 circuit_breakers — 大量削除ブレーカーのラッチ（M2-5）

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
  関心事が違う（`00005` の「トピック名はテーブル名ではなくクライアントの関心事に揃える」）

## 3.7 program_snapshots — 番組の事実のスナップショット（Phase 1。#27）

EPG プロジェクション（§9）は使い捨てキャッシュなので、番組が射影から消えても `reservations` / `program_intents` / `program_overrides` の GC 判定と UI 表示は成立し続けなければならない。この「番組の事実」のスナップショットは元は 3 表（`reservations` / `program_intents` / `program_overrides`）に重複して持たれていたが、書き込み時点がそれぞれ違うため**既にドリフトしていた**（同じ番組について異なる開始時刻が保存され、GC の判定が表ごとに違う時刻を使っていた）。Phase 1（#27）でこの 3 重化を 1 表に集約した。

```sql
CREATE TABLE program_snapshots (
    site        text        NOT NULL,
    program_id  bigint      NOT NULL,
    title       text        NOT NULL DEFAULT '',
    start_at    timestamptz NOT NULL,
    duration_ms bigint      NOT NULL,
    -- チャンネル・放送イベント識別（issue #101。00026 で NOT NULL 化）。
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

- **値の出所は EPG プロジェクションただ 1 つ**（#27 の決定）。書き手は api（意図・上書きの作成時。`ensureProgramSnapshot` が `GetProgramSnapshotSource` で `epg_programs ⋈ epg_services` から引く）と ruler（毎パス、`UpsertProgramSnapshotsFromProjection`）の 2 人だが、両者とも射影から引くので値の権威は割れない。**クライアントからは受け取らない**（サーバー権威。移行前の api は title / 開始時刻 / 尺をリクエストボディから受けており、それが GC の比較対象になっていた）。射影に番組がなければ 400
- **射影にある間は更新、消えたら凍結。** 延長・繰り下げで EPG 側の時刻は変わるので、射影に番組がある間は毎パス追従し、消えたときに凍結する。凍結しっぱなしにしないのは、GC 判定・容量超過判定（[データ層](../data.md) §6.5）が予約の時刻を需要区間として使うため
- **チャンネル識別はスナップショットする（programId を分解しない）。** Mirakurun 互換の programId は `NID*10^10 + SID*10^5 + EID` という合成規則を持つが、本番コードでこれを逆算してはならない。`network_id` / `service_id` / `channel_type` / `channel` は API のフィールドから素直に引く
  - reconciler の contentPath 生成はこのスナップショットを読む
  - 容量超過の判定（[データ層](../data.md) §6.5）の需要単位が `(channel_type, channel)` なので、使い捨ての EPG 射影への JOIN に頼らずここを読む
- **`event_id` / `service_name` も同じ経路でスナップショットする（issue #98）。** `reconciler.recordNeverScheduled` が `recordings` に never-scheduled の試行行（§5「行の作られ方」）を作るとき、放送イベントの識別 `(network_id, service_id, event_id)` と表示名 `service_name` が要る。`event_id` は他のチャンネル識別列と同様に `epg_programs.event_id` から素直に引き、**programId を分解して逆算しない**（00009 が本番コードから追放した依存そのもの）
- **チャンネル・放送イベント識別 6 列は NOT NULL（issue #101。00026）。** 元々は「00009 以前の残骸を救えず nullable のままの行がありうる」（4 列。#27）「射影から既に消えていて backfill できなかった行がありうる」（`event_id` / `service_name` の 2 列。#98）という理由で nullable だったが、この表の行寿命（放送 + `epg.retention_grace`）により移行時の残骸はとっくに GC 済みで、新規書き込みの 2 経路（`GetProgramSnapshotSource` / `UpsertProgramSnapshotsFromProjection`）はどちらも `epg_programs` / `epg_services` への INNER JOIN で NULL を書けない。00026 が NULL 行を DELETE してから 6 列を NOT NULL 化した。reconciler 側の「NULL なら推測せず schedule を作らない/試行行を作らない」という分岐（`resolveContentPath` / `recordNeverScheduled`）もこの状態が表現不可能になったことで削除している
- **GC はこの表からの 1 本の DELETE に集約された**（`DeleteEndedProgramSnapshots`。条件は `start_at + duration_ms < now() - epg.retention_grace`）。`reservations` / `program_intents` / `program_overrides` はこの表への `(site, program_id)` FK を `ON DELETE CASCADE` で持つので、この 1 本の DELETE で 3 表とも一緒に落ちる。**移行前は 3 本の DELETE がそれぞれ別のスナップショット列を見ており、ドリフトしていたので表ごとに違う時刻で GC していた**（#27 が解消した核心）。`recordings.reservation_id` は `ON DELETE SET NULL` なので、この削除で録画履歴（recordings/media_assets）が失われることはない
- **この表からの DELETE 経路は GC 1 本に限定すること。** 他の場所から消せると意図を巻き添えにする。特に「参照が 1 つも無いスナップショット行を掃除する」規則を足してはならない --- 掃除しないなら害はない（GC が拾う）が、掃除規則は intent の作成とレースする（ruler の導出削除が並行して作られた手動予約を消したのと同じ形。#29）
- `recordings` はこの FK の対象外。録画時点のスナップショット（§5）として独立にコピーを持つため、番組終了後に `program_snapshots` が消えても録画履歴には影響しない
