> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§5 / §6）。

## 5. recordings — 録画履歴（永続資産）

**録画試行の永続履歴**。成功だけでなく失敗（`recording.failed`）も行として残す — 「録画品質の実測」と再放送待ち判断の入力になる。番組情報は mirakc record / schedule の program ペイロードから**非正規化スナップショット**し、EPG テーブルにも mirakc にも依存せず自己完結する。

**recordings に新しく列を足すときの基準は「試行の帰結の観測だけを持つ脊椎であること」**（不変条件 13）。`media_assets`（下記 §6）はこの表を `recording_id` で指す衛星表 —— 判定基準・境界は [invariants.md](../invariants.md) §13。`keep_original` / `encode_profiles`（予約オプションの効力スナップショット。ingest worker が凍結し api が追記する）も同じ基準で衛星表 `recording_encode_policy` に切り出してある（下記「recording_encode_policy — 原本保持ポリシーの凍結」参照）。「今すぐ完全削除してほしい」というユーザーの要求（api が立てて api が取り消す）も同じ基準で衛星表 `recording_purge_requests` にある（下記「recording_purge_requests — 即時完全削除の要求（衛星表）」参照）。書き手が脊椎（watcher / reconciler）ではなく別の状態機械（ingest worker の凍結・api の事後追加 / 削除要求）である列は recordings 本体に残さない。**「隣に `deleted_at` があるから」は根拠にならない** —— あの 2 列（`deleted_at` / `superseded_at`）が本体にあるのは部分一意索引の述語が参照するからで、既存のテナントであることを根拠にすると間借りが次の間借りを正当化する。

```sql
CREATE TABLE recordings (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rule_id           bigint,        -- トレーサビリティ。rules への FK（ON DELETE SET NULL）
    source            text   NOT NULL CHECK (source IN ('rule', 'manual')),
    site              text   NOT NULL,  -- どのサイトで録画したか（履歴として snapshot）

    -- 番組情報スナップショット（ARIB/ISDB の概念であり mirakc 固有ではない）
    network_id        integer NOT NULL,
    service_id        integer NOT NULL,
    event_id          integer NOT NULL,
    service_name      text    NOT NULL,
    channel_type      text    NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel           text    NOT NULL,
    title             text    NOT NULL DEFAULT '',
    description       text,
    extended          jsonb,                     -- 拡張形式イベント（出演者等）
    genres            jsonb,                     -- [{lv1, lv2, un1, un2}]
    is_free           boolean NOT NULL DEFAULT true,
    program_start_at  timestamptz NOT NULL,
    program_duration_ms bigint NOT NULL,

    -- 録画実行の結果（mirakc の recording.status。詳細は下記「status の権威」）
    status            text NOT NULL CHECK (status IN ('recording', 'finished', 'canceled', 'failed')),
    started_at        timestamptz,               -- 実際の録画開始（record の startTime）
    ended_at          timestamptz,

    -- 品質イベントログ（recording.failed / record-broken の理由。§8 参照）
    -- mirakc の reason を jsonb のまま保持し、構造化カラムにはしない（不変条件 7）
    quality_events    jsonb NOT NULL DEFAULT '[]',

    -- 「本物の record が推論に必ず勝つ」ために追加。この行が同一 active-event の
    -- 枠を明け渡した不可逆な事実だけを持つ（§「同一イベントの重複防止」参照）。
    -- ユーザーの「ごみ箱送り」を表す deleted_at とは別列（不変条件 9: 2 つの
    -- 異なる「消える理由」を同じ列に混ぜない）。
    superseded_at     timestamptz,

    -- ごみ箱（録画単位の論理削除。原本 + 派生物 + サムネイルのグループごと）
    deleted_at        timestamptz,
    -- 即時物理削除の要求はここには無い（recording_purge_requests 衛星表。
    -- 下記「recording_purge_requests」節参照）。

    -- 「完全削除が完了した」不可逆な事実。削除 reconcile が
    -- パス末尾で、ごみ箱条件を満たしかつ物理削除待ちの media_assets が 1 行も
    -- 残っていない録画に一度だけ立てる。ごみ箱ビュー（ListTrashRecordings）は
    -- この列も IS NULL であることを要求するので、purge が完了した録画は
    -- ごみ箱一覧から外れる（[storage.md](../storage.md) §7）
    purged_at         timestamptz,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON recordings (program_start_at DESC);        -- ライブラリ一覧
CREATE INDEX ON recordings (network_id, service_id, event_id);
CREATE INDEX ON recordings (deleted_at) WHERE deleted_at IS NOT NULL;  -- ごみ箱ビュー
CREATE INDEX ON recordings (purged_at) WHERE purged_at IS NULL;  -- ごみ箱一覧の絞り込み
-- 履歴ベース重複排除は title の trgm 類似度で判定するが、GIN は張っていない。
-- gin_trgm_ops が加速するのは % / <% / LIKE / 正規表現で、similarity() の関数呼び出しには
-- 使われない（% の閾値は GUC pg_trgm.similarity_threshold 由来でルール単位の閾値と噛み合わない）。
```

### 行の作られ方

**`recordings` は mirakc が報告した録画試行だけを持つ。書き手は watcher だけ。**

- watcher が mirakc record を初観測（SSE または全量突き合わせ）→ record の program / service ペイロードからスナップショットして INSERT、`record_sync` 行から参照
- `recording.failed` で record が存在しないケース（start-recording-failed 等）→ status = `failed` の行を作り quality_events に理由を記録。**録画されなかった試行も履歴に残る**
- **番組終了時点で schedule が一度も観測されなかった場合は試行ではないため、`recordings` に行を作らない。** reconciler が後述の `never_scheduled_events` に欠測を書き、ライブラリには failed 録画として出さない
- 同一 active-event に mirakc 由来の failed 行が既にある状態で、後から成功 record が初観測されたとき → failed 行を supersede してから新しい行を INSERT する（下記「同一イベントの重複防止」の `superseded_at` 参照）
- ingest の完了は recordings の status ではなく **`media_assets` 行の有無**で表現する（コミット = DB 行。冗長な状態カラムを持たない）

### status の権威

**`status` の権威は「mirakc が報告したレコードの状態」であって、「Rokuban から見た録画の帰結」ではない。** 値は mirakc の `GET /api/recording/records` の `recording.status` をそのまま転記したもので、4 値（`recording` / `finished` / `canceled` / `failed`）で閉じている（mirakc-core の `RecordingStatus` が 4 バリアントの網羅的 enum であることをソースで確認済み）。record の無い行は `status` を持たない。

- `canceled` は**取消**（録画開始後にスケジュールが削除される等）で、`failed`（**失敗**）とは別の事実。`canceled` を `failed` に丸めると録画一覧が「失敗した」と嘘をつくので、独立した 4 値目として持つ
- **これは不変条件 7「mirakc 固有の概念を永続テーブルに入れない」の違反ではない。** 既存 3 値（`recording`/`finished`/`failed`）はもともと mirakc の語彙であり、`canceled` の追加はその踏襲。不変条件 7 が禁じるのは mirakc の内部 ID・タグ形式・スケジュール状態（`RecordingScheduleState` 等）のような実装詳細に紐づく構造の持ち込みで、録画結果の語彙（成功/失敗/取消）はドメインの外部仕様として妥当な粒度
- **未知の値（mirakc が将来値を追加した場合）は 5 値目を足さず `failed` に丸める。** `internal/watcher.normalizeRecordingStatus` が正規化し、生の値は `record_sync.status`（CHECK 無し）にそのまま残るので観測は失われない。丸めるのは「分かっている 2 つの事実を潰す」`canceled`→`failed` の丸めとは異なり、「何が起きたか分からない」という状態そのものが事実であるケースなので、粗い `failed` への集約を許容している。次に mirakc が値を足したら `internal/watcher` の ERROR ログを起点にこの CHECK と `openapi.yaml` の enum を更新すること
- **`status` に 5 値目（`never-scheduled` 等）を足さない。** schedule が一度も観測されなかった欠測は record の状態ではなく、後述の専用表で表す

### never_scheduled_events — schedule 欠測（永続の観測）

主キーは放送イベント `(site, network_id, service_id, event_id)`。**行の存在 = 「番組終了時点で、このイベントの schedule が一度も観測されなかった」**（不変条件 10）。書き手は reconciler の `recordNeverScheduled` だけで、`ON CONFLICT DO NOTHING` により最初の観測を永続化する。

- INSERT 条件は「番組終了」「今パスで schedule 非観測」「同じ放送イベントに live な `recordings` 行が無い」の積。live の定義は `recordings_unique_active_event` と同じ `deleted_at IS NULL AND superseded_at IS NULL`
- `program_snapshots` への FK は張らない。snapshot は放送 + 猶予で GC されるが、欠測は永続の観測である。CASCADE で消すと GC 後に同期除外が外れ、終了済み予約が再び schedule 対象になる
- `reservations.id` は持たない。予約は ruler の導出削除・再実体化で id が変わるため、読者も放送イベントキーで引く（不変条件 9「identity」）
- 本物の record が後から来ても欠測行は消さない。録画は録画、欠測は欠測として両方残る。表示は本物の `recordings` 行の存在で orphaned を消すが、同期除外は欠測行の存在だけを見て終了済み予約を対象に戻さない（[reservations.md](reservations.md) §3）
- mirakc 由来の `failed` は `recordings` にだけ現れ、この表には入らない。したがって録画途中の失敗からの再試行経路を妨げない
- `observed_at` は reconciler が欠測を書いた時刻。同期除外・表示の判定軸は時刻ではなく行の存在だけ

### ごみ箱

- UI の削除 = `deleted_at` を立てるだけ。ファイルには触れない。復元 = `deleted_at` を消し、即時削除の要求行を消すだけ
- 「今すぐ完全削除」= `recording_purge_requests` に行を入れるだけ（未 soft-delete なら `deleted_at` も同時に立てる）。**ファイルは消さない**
- 物理削除は削除 reconcile ループが次のいずれかを拾ってアセット単位で実行する:
  - `recording_purge_requests` に行がある（即時要求）
  - `deleted_at + 猶予期間（既定 30 日）` 経過
- 物理削除後も recordings 行と media_assets の tombstone は残る → ごみ箱を空にしても録画履歴・ドロップ統計・重複排除は壊れない
- API: `DELETE /api/recordings/{id}` / `POST .../restore` / `POST .../purge` / `GET /api/recordings?trash=true`

即時要求の表の形・書き手の判断は下記「recording_purge_requests — 即時完全削除の要求（衛星表）」参照。

### recording_purge_requests — 即時完全削除の要求（衛星表）

```sql
CREATE TABLE recording_purge_requests (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    requested_at timestamptz NOT NULL DEFAULT now()
);
```

**即時要求は `recordings` の列ではなく `recording_purge_requests` 衛星表に置く。行の存在 = 要求、取り消しは DELETE**（不変条件 10 / 13）。理由は書き手 —— この要求を定常運用で立てるのも取り消すのも api ロールで、`recordings` 本体を書く watcher / reconciler（試行の帰結の観測）ではない（rescue は別枠 —— 災害復旧で catalog ダンプから全表を書き戻すので、この表も他の表と同じように書く）。「`deleted_at` が本体にあるから隣に置く」は根拠にならない: `deleted_at` / `superseded_at` が本体に残っているのは部分一意索引 `recordings_unique_active_event` の述語がこの 2 列を参照し、述語が他表を参照できないからで、即時要求はその述語に出ない（枠が明くのは `deleted_at` / `superseded_at` だけ）。

- 完了後（`purged_at`）に要求行を掃除する経路は作らない。掃除役を足すと削除 reconcile が 2 人目の書き手になる（不変条件 12）。「ユーザーが即時削除を要求した」は完了後も真なので tombstone と一緒に残す
- `requested_at` は「いつ要求されたか」だけを持ち、判定には使わない。ここを `<= now()` で比較し始めたら、実質 boolean を timestamptz で持っていた旧列（`recordings.purge_after`）に戻る
- **復元（`deleted_at` を消す + 要求行を DELETE）は 1 文のデータ変更 CTE ではなくトランザクション内の 2 文で書く。** CTE はアーム全体が 1 つのスナップショットを共有するため、行ロックで UPDATE アームが待たされている間に commit された要求行が DELETE アームから見えず、「復元は成功したのに要求行だけ残る」が観測される。残った要求行はその場では何も起こさないが、次の普通の論理削除で猶予をバイパスする。2 文なら DELETE が UPDATE の後に新しいスナップショットを取るので要求行が見える
- **この表に行を入れる経路は、対象の `recordings` 行を先にロックする**（個別 purge は `recordings` の UPDATE アームがそれを兼ねている）。復元を 2 文に割っただけでは窓は閉じない —— DELETE が 0 行だったときロックは何も残らないので（READ COMMITTED に述語ロックは無い）、ロックせずに INSERT する経路（例: 一括 purge を `INSERT ... SELECT` で書く）を足すと、復元の DELETE 後・COMMIT 前に commit された要求行が残り、猶予バイパスが再発する

### 同一イベントの重複防止

```sql
CREATE UNIQUE INDEX recordings_unique_active_event
    ON recordings (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;
```

**録画試行の履歴は複数行を許すが、「生きている録画」は 1 イベントにつき 1 つ**。`deleted_at IS NULL AND superseded_at IS NULL` の部分インデックスなので、ごみ箱に入れた後で録り直すこともできるし（`deleted_at`）、後述の supersede で枠を明け渡した後で本物の record が録り直すこともできる（`superseded_at`）。

この制約があるため、watcher が同一 record を並行処理すると片方が制約違反で失敗する。`processRecord` は `record_sync` の行を先に確保して直列化することでこれを避けている（[録画エンジン](../recording.md) §3.3「record 処理は並行実行しても壊れない」）。

INSERT で `ON CONFLICT` を使うクエリ（`CreateFailedRecording` / `UpsertInPlaceRecording`）は、この索引と述語を一字一句一致させる必要がある。ずれると Postgres が「there is no unique or exclusion constraint matching the ON CONFLICT specification」で落ちる。

#### `superseded_at`: failed 行が後続 record に枠を譲る

`need-rescheduling` 等で `status='failed'` の行がこの枠を占有したまま残っているところに、mirakc が同一 active-event を後で録り直して成功 record を報告することがある（delayed broadcast、mirakc 側の手動再録画等）。この成功 record は無条件で枠を得られなければならない。欠測（`never_scheduled_events`）は別表なので supersede の対象ではない —— 本物の record が来ても欠測行は残り、`recordings` には試行行だけが増える。

watcher の `createRecording`（`internal/watcher/watcher.go`）は `CreateRecording` の直前に `SupersedeFailedRecording`（`internal/db/queries/recordings.sql`）を呼び、同一 active-event に生きている（`deleted_at IS NULL AND superseded_at IS NULL`）`status='failed'` の行があれば `superseded_at = now()` を立てて枠を明け渡させる。この 2 つは意図的に別々の SQL 文にしてある —— 1 つの `WITH` 句にまとめると、Postgres は「`WITH` 内のデータ変更文は主クエリと同時並行に実行され順序不定」であるため、UPDATE が INSERT より先に確定する保証がなく、実機のテストで実際に一意制約違反を起こすことを確認した。

- `status='failed'` の行だけを対象にする。`'recording'`/`'finished'`/`'canceled'` の生きている行と衝突する INSERT は、本当の重複 record（要調査対象の異常）として素の一意制約違反のまま従来どおりエラーにする
- `media_assets` を持つ failed 行（途中まで録れて failed になった行）でも扱いは同じ: superseded にするだけで `media_assets.recording_id` は書き換えない。ファイルの所有者は superseded になった旧 `recordings` 行のままで、物理削除は削除 reconcile が `recordings.deleted_at` を見て判断するため、superseded だけでは何も物理的に消えない
- 冪等: `processRecord` は `record_sync` の `(site, record_id)` 行ロックで、同一 record の 2 回目以降は `createRecording` 自体を呼ばない（[録画エンジン](../recording.md) §3.3）ので、`superseded_at` が二重に進んだり行が重複したりしない
- superseded にした行は `deleted_at` を立てない（ユーザーが消したわけではない）ので、録画一覧には失敗した旧行と成功した新行の両方が履歴として残り続ける

### recording_encode_policy — 原本保持ポリシーの凍結（衛星表）

`keep_original` / `encode_profiles`（予約オプションの効力スナップショット）は recordings 本体から `recording_id` で指す衛星表 `recording_encode_policy` に持つ。

```sql
CREATE TABLE recording_encode_policy (
    recording_id    bigint PRIMARY KEY REFERENCES recordings (id),
    keep_original   text   NOT NULL CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles text[] NOT NULL,
    CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
```

- **行の存在そのものが「凍結済み」を意味する**（不変条件 10）。凍結前はこの録画に対応する行が無く、`keep_original NOT NULL DEFAULT 'always'` のような列の既定値では「まだ凍結されていない」と「空として凍結された」を区別できなかった（recordings 本体にあった旧列の問題そのもの）。凍結後の行は空プロファイルであっても必ず INSERT される（`encode_profiles = '{}'` の行が存在しうる）
- **書き手は脊椎（watcher / reconciler）ではない**。ingest worker（`internal/worker/ingest.go` の `resolveAndSnapshotEncodePolicy`）が原本 media_asset をコミットする tx の中で凍結し、api（`POST /api/recordings/{id}/encode-profiles`）が追記方向にのみ書き換える。詳細は [storage.md](../storage.md) §6「原本 TS の保持ポリシー」参照
- **`recording_id` は `recordings.id`（脊椎の PK）への FK で、`recordings` と同時に生まれて同時に死ぬ**（不変条件 12）。until_encoded の CHECK は空プロファイルの until_encoded を表現不可能にする
- 既存録画への backfill は、原本 media_asset（`kind = 'original'`）の**有無**で「凍結済みかどうか」を判定して行を作る。列の値そのものは判定に使わない（不変条件 9）

### recording_ingest_progress — 転送の途中経過（衛星表）

原本の取り込み（ingest）が「どこまで書けたか」を持つ。書き手は ingest worker
（`internal/worker/ingest_progress.go`）だけ。

```sql
CREATE TABLE recording_ingest_progress (
    recording_id   bigint      PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    written_bytes  bigint      NOT NULL CHECK (written_bytes >= 0),
    expected_bytes bigint      CHECK (expected_bytes IS NULL OR expected_bytes >= 0),
    observed_at    timestamptz NOT NULL DEFAULT now()
);
```

- **行の存在そのものが「転送中」を意味する**（不変条件 10）。転送していないことを表す行は作らない。消えるのは 2 経路だけ —— 原本 `media_assets` を INSERT する tx（コミット = DB 行なので、原本行が生まれる瞬間に進捗行が消え、「原本があるのに取り込み中」が読者から見えない）と、`recordings` 行の削除（`ON DELETE CASCADE`）
- **書き手は脊椎（watcher / reconciler）ではない**ので本体の列にしない（不変条件 13。`recording_encode_policy` と同じ判断）。`record_sync` にも載せない —— あちらは mirakc 側の観測で書き手は watcher、こちらは Rokuban 側のファイルに何バイト書けたかで、1 表 2 書き手になる（不変条件 12）
- **`written_bytes` は単調増加しない。** ジョブ内リトライ（Range 再開）では積み上がるが、ジョブ再試行は部分ファイルを truncate してゼロから作り直す（[recording/ingest.md](../recording/ingest.md) §5.3 の層 2）ので 0 に戻る。「いまファイルに書けているバイト数」の観測であって累積の転送実績ではない
- **`expected_bytes` は `record_sync.content_length` のコピー**（転送開始時に読む）。mirakc が length を返さなければ NULL のままにする —— でっち上げた分母を置かない
- ジョブが失敗して River のバックオフ待ちに入ると、行は `observed_at` が古いまま残る。これは意図した挙動で、`river_job` を API 契約に露出させない代わりに停滞を読ませる唯一の材料になる（[recording/ingest.md](../recording/ingest.md) §5.6）

## 6. media_assets — メディアアセット（永続資産）

録画に紐づくファイルの台帳。**この行の存在が「公開済み」の定義**（ストレージ契約ルール 3）。

```sql
CREATE TABLE media_assets (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recording_id bigint NOT NULL REFERENCES recordings (id),
    kind         text   NOT NULL CHECK (kind IN ('original', 'encoded', 'thumbnail')),
    profile      text,                -- kind = 'encoded' のみ: エンコードプロファイル名
    CHECK ((kind = 'encoded') = (profile IS NOT NULL)),
    rel_path     text   NOT NULL,     -- ストレージルートからの相対パス（不変条件: 絶対パス禁止）
    size_bytes   bigint NOT NULL,

    -- 物理削除プロトコル: active → deleting → deleted（冪等。どこで落ちても reconcile が拾い直す）
    state        text NOT NULL DEFAULT 'active'
                      CHECK (state IN ('active', 'deleting', 'deleted')),
    deleted_at   timestamptz,         -- 物理削除の完了時刻（tombstone）

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- 1 録画につき original / thumbnail は 1 つ、encoded はプロファイルごとに 1 つ
    UNIQUE NULLS NOT DISTINCT (recording_id, kind, profile)
);

CREATE INDEX ON media_assets (recording_id);
CREATE INDEX ON media_assets (kind, state);
-- 生きているアセットのパス衝突を防ぐ（tombstone のパスは再利用可）
CREATE UNIQUE INDEX ON media_assets (rel_path) WHERE state <> 'deleted';
```

- INSERT するのは worker（ingest / encode / thumbnail ジョブ）のみ。`ON CONFLICT` で冪等に
- **deleted への遷移後も行は消さない**（tombstone）。`drop_stats` と元サイズは原本削除後も UI で見られる
- 物理削除に至る 3 ソース（ごみ箱の猶予超過 / `until_encoded` の派生物完備 / 孤児回収）はすべて 1 本の削除 reconcile ループに集約し、一括削除サーキットブレーカーをループ全体に 1 つかける

