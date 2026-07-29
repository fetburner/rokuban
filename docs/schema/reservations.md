## 3. reservations — 予約（desired state）

ルール評価の純粋な出力。手動予約も同じテーブルで `source` が違うだけ。**M1 では ruler が存在しないため全行が manual（base は NULL、全フィールドが overrides）**。

```sql
CREATE TABLE reservations (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    site              text   NOT NULL,          -- 設定ファイル定義のサイト名
    program_id        bigint NOT NULL,          -- mirakc/Mirakurun の programId（site 単位のスコープ）
    source            text   NOT NULL CHECK (source IN ('rule', 'manual')),
    rule_id           bigint,                   -- M2 で REFERENCES rules(id) を追加
    state             text   NOT NULL DEFAULT 'active'
                             CHECK (state IN ('active', 'detached', 'orphaned')),

    -- 予約オプションの導出側（issue #2 の base/overrides コメント）。
    -- ユーザーの上書きは program_overrides 表にあり、この行には載らない（§3.5）
    base              jsonb,                    -- ruler だけが書く。manual では NULL

    -- 番組情報の非正規化（reconciler の contentPath 生成・GC 判定・UI 表示を
    -- EPG プロジェクションの刈り取りと独立させるための最小限のスナップショット）
    title             text   NOT NULL DEFAULT '',
    program_start_at  timestamptz NOT NULL,
    program_duration_ms bigint NOT NULL,

    -- チャンネル識別のスナップショット（M2 で追加。00009）。
    -- nullable なのは移行前の行を救えない場合があるためで、新規行は api が必ず埋める
    network_id        integer,
    service_id        integer,
    channel_type      text CHECK (channel_type IS NULL OR channel_type IN ('GR','BS','CS','SKY')),
    channel           text,

    -- 重複排除の判定根拠（M2-6 で追加。00013）。ruler が毎パス作り直す導出列で、
    -- base と同じ凍結規則に従う。FK は張らない（後述）
    dedup_match_recording_id bigint,
    dedup_similarity         real,
    CHECK ((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL)),

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    UNIQUE (site, program_id)   -- desired 予約は site x programId につき最大 1 つ。detached にも適用
);

CREATE INDEX ON reservations (state);
CREATE INDEX ON reservations (rule_id);
```

- 同一の BS/CS 番組が複数サイトの EPG に現れた場合、site が違えば別予約になる（両サイトで録る、が表現可能）。サイト横断の重複排除はルール側の関心事（M2 の履歴ベース重複排除で扱う）

### 番組情報のスナップショットは「射影にある間は更新、消えたら凍結」

`title` / `program_start_at` / `program_duration_ms` / チャンネル 4 列を予約行に持つのは、**EPG プロジェクションが使い捨てだから**。番組が射影から消えても次の 3 つが成立し続ける必要がある。

| 依存するもの | 射影を JOIN する設計だと |
|---|---|
| GC 判定（番組終了後のみ削除） | 射影が先に刈られると削除時期が判定できず行が残り続ける |
| `orphaned` の説明可能性 | 「何の予約だったか」を UI に出せない |
| contentPath 生成の再現性 | 生成の根拠（題名・時刻・サービス）が失われる |

「ルール由来なら rule_id だけ持てば復元できる」は成立しない。ルールからは番組の事実（題名・時刻・チャンネル）が復元できないので、結局射影に依存する。

**ただし凍結しっぱなしにはしない。** 延長・繰り下げで EPG 側の時刻は変わるので、**射影に番組がある間は ruler が毎パス更新し、消えたときに凍結する**（base と同じ扱い）。凍結しっぱなしだと、容量超過の判定（[データ層](../data.md) §6.5）が予約行の時刻を需要区間として使うため帯がずれる。

### チャンネル識別はスナップショットする（programId を分解しない）

Mirakurun 互換の programId は `NID*10^10 + SID*10^5 + EID` という合成規則を持つが、**本番コードでこれを逆算してはならない**。mirakc 固有の合成規則への依存になり、同じ情報が API のフィールド（`networkId` / `serviceId` / `eventId`）として素直に手に入るため。

`CreateReservation` が EPG プロジェクション（`epg_programs` ⋈ `epg_services`）から引いて 4 列に焼き付ける。**クライアントからは受け取らない**（サーバー権威）。射影に番組がなければ 400 を返す。

- reconciler の contentPath 生成はこのスナップショットを読む。`service_id` が NULL なら**推測せず schedule を作らない**（誤ったパスで録画するより同期対象から外してアラートする）
- 容量超過の判定（[データ層](../data.md) §6.5）の需要単位が `(channel_type, channel)` なので、使い捨ての EPG 射影への JOIN に頼らずここを読む

### 重複排除の根拠に FK を張らない（M2-6）

`dedup_match_recording_id` は `recordings(id)` を指すが **FK を張っていない**。`REFERENCES recordings (id) ON DELETE SET NULL` と上の CHECK は両立しないためである。FK アクションは FK 側の列しか NULL にできないので、参照されている `recordings` 行を物理削除すると `(NULL, 0.87)` という行ができて CHECK に違反し、**DELETE 自体が中断する**（後片付けの機構が安全網に引っかかって削除を不可能にする）。

外すのは FK の方にした。

- 根拠 2 列は ruler が毎パス作り直す導出値（不変条件 9）。参照先が消えても次のパスで両方 NULL に戻るので、**孤立は自己修復する**
- `recordings.id` は `GENERATED ALWAYS AS IDENTITY` で再利用されないため、孤立した id が別の録画を指すことは構造的に起きない（FK を外して一番怖い失敗モードが無い）
- §5 の tombstone 契約により、本番では `recordings` 行を物理削除しない。FK が守っているのは起きない事象
- CHECK は「あってはいけない組み合わせを表現不可能にする」（不変条件 10）を担っている。仕事をしているのはこちら

### base / overrides の意味論

- **effective = COALESCE(base, '{}') ⊕ program_overrides.overrides**。reconciler が mirakc へ同期し、ingest / encode が参照するのは常に effective。解決は `db.EffectiveOptions` の 1 箇所に集約し、jsonb の Unmarshal 失敗を握りつぶさない
- base と overrides は**同形の jsonb ドキュメント**（§8）。ruler は EPG 更新のたびに base を丸ごと再計算してよく、**overrides は別表なので構造的に触れない**
- **`skip` は overrides のキーではなく `program_intents.action`**。列なので base 側の skip に対する優先順位が明示的に決まる（`action = 'skip'` が勝つ）。skip された番組は**予約行を持たない**

### state のライフサイクル

| state | 意味 | 遷移 |
|---|---|---|
| `active` | 通常の desired 予約 | — |
| `detached` | ルールがマッチしなくなったが意図または上書きがある行。base は凍結され、実質 manual として動く（`intent{skip}` なら録画しない detached） | ルール再マッチで base 再計算のうえ `active` に戻る（overrides は無傷） |
| `orphaned` | **番組終了時刻を過ぎたのに mirakc に schedule が観測されなかった行**（= 録画されずに終わった）。即削除せず残して「録れなかった」を説明可能にする | 番組終了時刻経過で GC |

- **行の物理削除（GC）は「番組の終了時刻を過ぎた後」のみ**。`program_start_at + program_duration_ms` で判定できるため EPG テーブルに依存しない
- 意図も上書きもない active 予約がルール・EPG から消えた場合は通常の宣言的動作として削除（ただし大量削除サーキットブレーカーの対象）

**`state` を「mirakc への同期対象か」のフィルタに使ってはならない**（M2-4 で修正）。`active` / `detached` は
`(rule_id, base)` からの導出値（`detached ⟺ rule_id IS NULL AND base IS NOT NULL`）で、独立した事実ではない。
同期の可否を決めるのは `effective.skip` であり、state で外してよいのは `orphaned` だけ（番組が終了しているので
schedule を作る意味がない）。`reconciler.listDesired` が `state = 'active'` で絞っていたため detached の予約に
schedule が作られず、**手動予約 → たまたまルールがマッチ → そのルールを編集して外す**という経路で
ユーザーの手動予約が黙って録画されなくなっていた（[録画エンジン](../recording.md) §4.3）。

`orphaned` の意味は「EPG から消えた」ではない。M1 の実装（`reconciler.markOrphaned`）は
**番組終了後に schedule が観測されなかった予約**を marking しており、EPG の欠損とは無関係。
EPG フリッカー対策（issue #2 §3.2）は別の機構（大量削除サーキットブレーカー）が担う。

> **列を残したことで 2 件が再発した**（[#30](https://github.com/fetburner/rokuban/issues/30)）。①ruler は導出の式ではなく**前パスの `rule_id` を見た遷移**を書くので、ルールを**削除**した経路（FK の `ON DELETE SET NULL` が先に `rule_id` を落とす）では `detached` にならず、`DELETE /api/rules/{id}` が返す `detachedReservations` の件数と予約一覧のバッジが一致しない。②`MarkReservationOrphaned` に `AND state = 'active'` が残っているため、**detached 予約は永久に `orphaned` にならない**（M2-4 では `listDesired` 側だけを直した）。どちらも `active` / `detached` を列から外せば消える。

この意味論のため、`orphaned` は `epg_last_seen_at` のようなタイムスタンプからは導出できない
（「schedule が観測されなかった」という観測側の情報が必要）。状態機械を導出に寄せる案
（[issue #18](https://github.com/fetburner/rokuban/issues/18) の案 B）を検討する際の制約になる。

### 書き込み所有権

| カラム | 書く人 |
|---|---|
| `reservations` の base / チャンネル列 / 番組スナップショット / state（active・detached 遷移） | ruler（M2〜） |
| `reservations` / `program_intents` / `program_overrides` の GC（番組終了 + `epg.retention_grace` 経過） | ruler のパス |
| `program_intents`（action）、`program_overrides`（overrides）、手動予約の作成・取消 | api |
| state（orphaned 遷移） | reconciler |

## 3.5 program_intents / program_overrides — 番組単位のユーザー意図（永続）

**api だけが書き、ruler は読むだけ**の 2 表。予約（導出）とユーザー意図（永続）を分離する（issue #18 の案 A、[録画エンジン](../recording.md) §4.2）。

```sql
CREATE TABLE program_intents (
    site       text   NOT NULL,
    program_id bigint NOT NULL,
    action     text   NOT NULL CHECK (action IN ('record', 'skip')),
    -- GC 用スナップショット（EPG 射影の刈り取りと独立させる）
    program_start_at    timestamptz NOT NULL,
    program_duration_ms bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

CREATE INDEX ON program_intents (program_start_at);

-- パラメータの上書き（M2-4 / 00010 で program_intents から分離）
CREATE TABLE program_overrides (
    site       text   NOT NULL,
    program_id bigint NOT NULL,
    overrides  jsonb  NOT NULL,   -- 上書きしたキーのみの疎なドキュメント
    program_start_at    timestamptz NOT NULL,
    program_duration_ms bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

CREATE INDEX ON program_overrides (program_start_at);
```

**表を 2 つに分けるのは、ユーザーが番組について主張しうる 2 つのことが独立だから**である（①録る / 録るな ②パラメータの上書き）。1 表に同居させると `action NOT NULL` のために「パラメータだけ上書きした。録る録らないについては意見なし」が表現できず、行が空になったときに何を主張していた行かを行自身から読めなくなる。理由と具体的な誤動作は [録画エンジン](../recording.md) §4.2「overrides は `program_intents` とは別の表に置く」。

- **`action`**: `record`（録れ = 手動予約 / dedup skip の明示的な無効化）/ `skip`（録るな = 番組単位の除外）。**skip された番組は `reservations` に行を持たない**
- **`overrides` に CHECK を置かない。** `program_overrides` 自身のロジックが内容を一切使わない不透明なペイロードだから jsonb を許している。内容を検査する制約（`jsonb_strip_nulls(overrides) <> '{}'` 等）は技術的には可能だが、「クエリはしないが制約はする」という中途半端な状態を作らない。**空の上書き = 行が無い**で表し、マージも SQL ではなく Go 側で `db.ReservationOptions` の型付きフィールドとして行う
- **書き込み所有権**: api のみ。ruler は base を再計算するだけでこの 2 表に触らない → 手動編集が構造的に上書きされない
- **GC**: 番組終了後（`program_start_at + duration < now()`）。意図と上書きの寿命を放送の寿命に揃える
- **site スコープ**: 「サイト A では録らない、B では録る」が N 予約の下では意味を持つため（[録画エンジン](../recording.md) §3.1）
- SSE ヒントはどちらも `reservations` トピックに寄せる（意図の変更は予約一覧・番組表の両方に現れる）

**どちらの表も行の存在が予約を存在させる**（ruler の desired に入る）。`program_overrides` に行があるだけで予約が保たれるのは §4.3「overrides あり → 削除せず detached で保持」の要求。

取消は**無条件に `intent{skip}` を書いて導出行を落とす**。行を消すだけでは「消された行」と「最初から無かった行」が ruler から区別できず、次の全量パスが復活させる。

`program_start_at` / `program_duration_ms` は `reservations` にも同じ意味・同じ出所で存在し、この分離で 3 箇所目になる。しかも ruler は `reservations` 側だけを延長に追従させるため既にドリフトしている（`epg.retention_grace` の 24h が吸収している）。`program_snapshots` への抽出は別タスク。

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

