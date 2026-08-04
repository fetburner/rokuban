## 5. recordings — 録画履歴（永続資産）

**録画試行の永続履歴**。成功だけでなく失敗（`recording.failed`）も行として残す — 「録画品質の実測」（issue #2）と再放送待ち判断の入力になる。番組情報は mirakc record / schedule の program ペイロードから**非正規化スナップショット**し、EPG テーブルにも mirakc にも依存せず自己完結する。

**recordings に新しく列を足すときの基準は「試行の帰結の観測だけを持つ脊椎であること」**（[CLAUDE.md](../../CLAUDE.md) 不変条件 13。#156）。`media_assets`（下記 §6）はこの表を `recording_id` で指す衛星表 —— 判定基準・境界は [principles.md](principles.md) §9 参照。**これは既存の全列がこの基準を満たしていることを意味しない。** `keep_original` / `encode_profiles`（下記 CREATE TABLE 内、予約オプションの効力スナップショット。ingest worker が凍結し api が追記する）はこの基準を遡って適用すれば衛星に出すべき列だが、まだ切り出されておらず、切り出しは #159 が持っている。

```sql
CREATE TABLE recordings (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    reservation_id    bigint REFERENCES reservations (id) ON DELETE SET NULL,
    rule_id           bigint,        -- トレーサビリティ。00006 で FK 追加済み
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

    -- 予約オプションの効力スナップショット（ingest / retention reconcile が参照）
    keep_original     text NOT NULL DEFAULT 'always'
                           CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles   text[] NOT NULL DEFAULT '{}',   -- 設定ファイル定義のプロファイル名
    -- until_encoded を選ぶならプロファイルを最低 1 つ添える（issue #104 / 00020）。
    -- 無ければ削除 reconcile の派生物完備判定が空配列で恒真になり、原本が唯一の
    -- コピーのまま物理削除されうる（不変条件 10「あってはいけない組み合わせを
    -- 表現不可能にする」）
    CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0),

    -- 品質イベントログ（recording.failed / record-broken の理由。§8 参照）
    -- mirakc の reason を jsonb のまま保持し、構造化カラムにはしない（不変条件 7）
    quality_events    jsonb NOT NULL DEFAULT '[]',

    -- 「本物の record が推論に必ず勝つ」（issue #98 の決定、issue #129 症状 2）で
    -- 追加。この行が同一 active-event の枠を明け渡した不可逆な事実だけを持つ
    -- （§「同一イベントの重複防止」参照）。ユーザーの「ごみ箱送り」を表す
    -- deleted_at とは別列（不変条件 9: 2 つの異なる「消える理由」を同じ列に
    -- 混ぜない）。
    superseded_at     timestamptz,

    -- ごみ箱（録画単位の論理削除。原本 + 派生物 + サムネイルのグループごと）
    deleted_at        timestamptz,
    -- 即時物理削除の要求印（M3-7 / 00018）。ファイルは消さない。
    -- M3-8 の削除 reconcile が `purge_after <= now()` を拾って unlink する。
    -- 猶予経過による通常 purge とは独立した「前倒し」の合図。
    purge_after       timestamptz,
    -- 「完全削除が完了した」不可逆な事実（issue #135 / 00024）。削除 reconcile が
    -- パス末尾で、ごみ箱条件を満たしかつ物理削除待ちの media_assets が 1 行も
    -- 残っていない録画に一度だけ立てる。ごみ箱ビュー（ListTrashRecordings）は
    -- この列も IS NULL であることを要求するので、purge が完了した録画は
    -- ごみ箱一覧から外れる（[storage.md](../storage.md) §7）
    purged_at         timestamptz,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON recordings (reservation_id);
CREATE INDEX ON recordings (program_start_at DESC);        -- ライブラリ一覧
CREATE INDEX ON recordings (network_id, service_id, event_id);
CREATE INDEX ON recordings (deleted_at) WHERE deleted_at IS NOT NULL;  -- ごみ箱ビュー
CREATE INDEX ON recordings (purge_after) WHERE purge_after IS NOT NULL;  -- 即時 purge
CREATE INDEX ON recordings (purged_at) WHERE purged_at IS NULL;  -- ごみ箱一覧の絞り込み
-- 履歴ベース重複排除（M2-6）は title の trgm 類似度で判定するが、GIN は張っていない。
-- gin_trgm_ops が加速するのは % / <% / LIKE / 正規表現で、similarity() の関数呼び出しには
-- 使われない（% の閾値は GUC pg_trgm.similarity_threshold 由来でルール単位の閾値と噛み合わない）。
```

### 行の作られ方

- watcher が mirakc record を初観測（SSE または全量突き合わせ）→ record の program / service ペイロードからスナップショットして INSERT、`record_sync` 行から参照
- `recording.failed` で record が存在しないケース（start-recording-failed 等）→ status = `failed` の行を作り quality_events に理由を記録。**録画されなかった試行も履歴に残る**
- **番組終了時点で捕獲の試みが一度も記録されなかった場合（issue #98）** → reconciler（`recordNeverScheduled`）が status = `failed` の行を作り、quality_events に `recording.never-scheduled`（内訳付き）を記録する。watcher の 2 経路（mirakc 由来の失敗）とは書き手が異なる唯一の経路で、`recordings` に書き手が 2 人（watcher / reconciler）いることになるが、直列化契約は下記「同一イベントの重複防止」の一意索引が担う（CLAUDE.md 不変条件 12「1 つの表に書き手が 2 人いるのも同じ兆候」への回答）
- 同一 active-event に上記の failed 行（mirakc 由来・never-scheduled のどちらでも）が既にある状態で、後から成功 record が初観測されたとき → failed 行を supersede してから新しい行を INSERT する（下記「同一イベントの重複防止」の `superseded_at` 参照。issue #129 症状 2 / #98）
- ingest の完了は recordings の status ではなく **`media_assets` 行の有無**で表現する（コミット = DB 行。冗長な状態カラムを持たない）

### status の権威（issue #130 / #98）

**`status` の権威は「mirakc が報告したレコードの状態」であって、「Rokuban から見た録画の帰結」ではない。** 値は mirakc の `GET /api/recording/records` の `recording.status` をそのまま転記したもので、4 値（`recording` / `finished` / `canceled` / `failed`）で閉じている（mirakc-core の `RecordingStatus` が 4 バリアントの網羅的 enum であることをソースで確認済み。issue #130）。**issue #98 でこの権威を一段階だけ広げた**: mirakc が record を報告した行は報告値をそのまま持つ、reconciler が起こした行（never-scheduled）は `failed` + 理由は `quality_events` が権威、という 2 分岐になった。

- `canceled` は**取消**（録画開始後にスケジュールが削除される等）で、`failed`（**失敗**）とは別の事実。`canceled` を `failed` に丸めると録画一覧が「失敗した」と嘘をつくので、独立した 4 値目として持つ
- **これは不変条件 7「mirakc 固有の概念を永続テーブルに入れない」の違反ではない。** 既存 3 値（`recording`/`finished`/`failed`）はもともと mirakc の語彙であり、`canceled` の追加はその踏襲。不変条件 7 が禁じるのは mirakc の内部 ID・タグ形式・スケジュール状態（`RecordingScheduleState` 等）のような実装詳細に紐づく構造の持ち込みで、録画結果の語彙（成功/失敗/取消）はドメインの外部仕様として妥当な粒度
- **未知の値（mirakc が将来値を追加した場合）は 5 値目を足さず `failed` に丸める。** `internal/watcher.normalizeRecordingStatus` が正規化し、生の値は `record_sync.status`（CHECK 無し）にそのまま残るので観測は失われない。丸めるのは「分かっている 2 つの事実を潰す」`canceled`→`failed` の丸めとは異なり、「何が起きたか分からない」という状態そのものが事実であるケースなので、粗い `failed` への集約を許容している。次に mirakc が値を足したら `internal/watcher` の ERROR ログを起点にこの CHECK と `openapi.yaml` の enum を更新すること
- **`status` に 5 値目（`never-scheduled` 等）を足さない（issue #98 の決定）。** このスキーマでは「失敗の種別」はもともと `quality_events` の管轄で、`status` は粗い帰結だけを持つ（mirakc 由来の失敗も理由は `status` ではなく `quality_events` にある）。never-scheduled は失敗の**理由**なので、`status='failed'` + `quality_events` に `recording.never-scheduled` が既存の型に合致する。`canceled` を独立させた論法（「分かっている 2 つの事実を同じ値に潰すと一覧が嘘をつく」）の再演に見えるが違う --- 区別（mirakc 由来かreconciler由来か、そしてその理由）は quality_events に保存されるので嘘にならない
- never-scheduled 行は「`media_assets` を 1 行も持たない録画」になる。ユーザーがこれをごみ箱送り（`deleted_at` を立てる）にした場合でも、`media_assets` が 0 行なら `MarkPurgedRecordings`（issue #135 / 00024）の `NOT EXISTS` 判定は空集合で自明に真になり、正しく完全削除の対象として扱われる（[storage.md](../storage.md) §7 参照）

**never-scheduled 行の識別:** `reservation_id` で特定の予約に紐づく（`reservations` GC 後は `ON DELETE SET NULL` で `NULL` になる）ほか、`quality_events` の配列要素に `event = "recording.never-scheduled"` を持つことで機械的に判別できる（`internal/db.QualityEventNeverScheduled`）。API の `orphaned` 表示（`GetReservationFull` の `never_recorded`）と `ListReservationsForSyncEvaluation` 等の同期除外は、`never_scheduled_events` VIEW（`00030`、issue #157）が定義する核（`status='failed'` + このマーカー）を共有する。差は表示側だけが持つ live 限定（`deleted_at` / `superseded_at` が NULL）で、同期除外はこれを見ない —— mirakc 由来の途中失敗からの再試行を妨げないためで、意図的な差である（[reservations.md](reservations.md) §3 参照）。

### ごみ箱（issue #4 の削除エンジンコメント / M3-7 #69）

- UI の削除 = `deleted_at` を立てるだけ。ファイルには触れない。復元 = `deleted_at`（と `purge_after`）を消すだけ
- 「今すぐ完全削除」= `purge_after = now()` を立てるだけ（未 soft-delete なら `deleted_at` も同時に立てる）。**ファイルは消さない**
- 物理削除は削除 reconcile ループ（M3-8）が次のいずれかを拾ってアセット単位で実行する:
  - `purge_after IS NOT NULL AND purge_after <= now()`（即時要求）
  - `deleted_at + 猶予期間（既定 30 日）` 経過
- 物理削除後も recordings 行と media_assets の tombstone は残る → ごみ箱を空にしても録画履歴・ドロップ統計・重複排除は壊れない
- API: `DELETE /api/recordings/{id}` / `POST .../restore` / `POST .../purge` / `GET /api/recordings?trash=true`

### 同一イベントの重複防止（`00003` / `00023`）

```sql
CREATE UNIQUE INDEX recordings_unique_active_event
    ON recordings (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;
```

**録画試行の履歴は複数行を許すが、「生きている録画」は 1 イベントにつき 1 つ**。`deleted_at IS NULL AND superseded_at IS NULL` の部分インデックスなので、ごみ箱に入れた後で録り直すこともできるし（`deleted_at`）、後述の supersede で枠を明け渡した後で本物の record が録り直すこともできる（`superseded_at`）。

この制約があるため、watcher が同一 record を並行処理すると片方が制約違反で失敗する。`processRecord` は `record_sync` の行を先に確保して直列化することでこれを避けている（[録画エンジン](../recording.md) §3.3「record 処理は並行実行しても壊れない」）。

INSERT で `ON CONFLICT` を使うクエリ（`CreateFailedRecording` / `UpsertInPlaceRecording`）は、この索引と述語を一字一句一致させる必要がある。ずれると Postgres が「there is no unique or exclusion constraint matching the ON CONFLICT specification」で落ちる。

#### `superseded_at`: 本物の record が推論に必ず勝つ（issue #98 / #129 症状 2）

`need-rescheduling` 等で `status='failed'` の行がこの枠を占有したまま残っているところに、mirakc が同一 active-event を後で録り直して成功 record を報告することがある（delayed broadcast、mirakc 側の手動再録画等）。この成功 record は無条件で枠を得られなければならない —— 「本物の record が推論に必ず勝つ」という規則の最初の適用がこれで、issue #98 で実装された推論由来の行（never-scheduled。`reconciler.recordNeverScheduled` が作る）にも同じ規則が及ぶ。never-scheduled 行自身の書き込み条件（`CreateNeverScheduledRecording` の `ON CONFLICT ... DO NOTHING`）がこの規則の適用そのもの --- 生きている行（本物の record でも前パスの never-scheduled 行でも）が既にあれば何もしない。

watcher の `createRecording`（`internal/watcher/watcher.go`）は `CreateRecording` の直前に `SupersedeFailedRecording`（`internal/db/queries/recordings.sql`）を呼び、同一 active-event に生きている（`deleted_at IS NULL AND superseded_at IS NULL`）`status='failed'` の行があれば `superseded_at = now()` を立てて枠を明け渡させる。この 2 つは意図的に別々の SQL 文にしてある —— 1 つの `WITH` 句にまとめると、Postgres は「`WITH` 内のデータ変更文は主クエリと同時並行に実行され順序不定」であるため、UPDATE が INSERT より先に確定する保証がなく、実機のテストで実際に一意制約違反を起こすことを確認した。

- `status='failed'` の行だけを対象にする。`'recording'`/`'finished'`/`'canceled'` の生きている行と衝突する INSERT は、本当の重複 record（要調査対象の異常）として素の一意制約違反のまま従来どおりエラーにする
- `media_assets` を持つ failed 行（途中まで録れて failed になった行）でも扱いは同じ: superseded にするだけで `media_assets.recording_id` は書き換えない。ファイルの所有者は superseded になった旧 `recordings` 行のままで、物理削除は削除 reconcile が `recordings.deleted_at` を見て判断するため、superseded だけでは何も物理的に消えない
- 冪等: `processRecord` は `record_sync` の `(site, record_id)` 行ロックで、同一 record の 2 回目以降は `createRecording` 自体を呼ばない（[録画エンジン](../recording.md) §3.3）ので、`superseded_at` が二重に進んだり行が重複したりしない
- superseded にした行は `deleted_at` を立てない（ユーザーが消したわけではない）ので、録画一覧には失敗した旧行と成功した新行の両方が履歴として残り続ける

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
- 物理削除に至る 3 ソース（ごみ箱の猶予超過 / `until_encoded` の派生物完備 / 孤児回収）はすべて 1 本の削除 reconcile ループに集約し、一括削除サーキットブレーカーをループ全体に 1 つかける（issue #4）

