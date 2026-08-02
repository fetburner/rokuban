## 5. recordings — 録画履歴（永続資産）

**録画試行の永続履歴**。成功だけでなく失敗（`recording.failed`）も行として残す — 「録画品質の実測」（issue #2）と再放送待ち判断の入力になる。番組情報は mirakc record / schedule の program ペイロードから**非正規化スナップショット**し、EPG テーブルにも mirakc にも依存せず自己完結する。

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

    -- 品質イベントログ（recording.failed / record-broken の理由。§8 参照）
    -- mirakc の reason を jsonb のまま保持し、構造化カラムにはしない（不変条件 7）
    quality_events    jsonb NOT NULL DEFAULT '[]',

    -- ごみ箱（録画単位の論理削除。原本 + 派生物 + サムネイルのグループごと）
    deleted_at        timestamptz,
    -- 即時物理削除の要求印（M3-7 / 00018）。ファイルは消さない。
    -- M3-8 の削除 reconcile が `purge_after <= now()` を拾って unlink する。
    -- 猶予経過による通常 purge とは独立した「前倒し」の合図。
    purge_after       timestamptz,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON recordings (reservation_id);
CREATE INDEX ON recordings (program_start_at DESC);        -- ライブラリ一覧
CREATE INDEX ON recordings (network_id, service_id, event_id);
CREATE INDEX ON recordings (deleted_at) WHERE deleted_at IS NOT NULL;  -- ごみ箱ビュー
CREATE INDEX ON recordings (purge_after) WHERE purge_after IS NOT NULL;  -- 即時 purge
-- 履歴ベース重複排除（M2-6）は title の trgm 類似度で判定するが、GIN は張っていない。
-- gin_trgm_ops が加速するのは % / <% / LIKE / 正規表現で、similarity() の関数呼び出しには
-- 使われない（% の閾値は GUC pg_trgm.similarity_threshold 由来でルール単位の閾値と噛み合わない）。
```

### 行の作られ方

- watcher が mirakc record を初観測（SSE または全量突き合わせ）→ record の program / service ペイロードからスナップショットして INSERT、`record_sync` 行から参照
- `recording.failed` で record が存在しないケース（start-recording-failed 等）→ status = `failed` の行を作り quality_events に理由を記録。**録画されなかった試行も履歴に残る**
- ingest の完了は recordings の status ではなく **`media_assets` 行の有無**で表現する（コミット = DB 行。冗長な状態カラムを持たない）

### status の権威（issue #130）

**`status` の権威は「mirakc が報告したレコードの状態」であって、「Rokuban から見た録画の帰結」ではない。** 値は mirakc の `GET /api/recording/records` の `recording.status` をそのまま転記したもので、4 値（`recording` / `finished` / `canceled` / `failed`）で閉じている（mirakc-core の `RecordingStatus` が 4 バリアントの網羅的 enum であることをソースで確認済み。issue #130）。

- `canceled` は**取消**（録画開始後にスケジュールが削除される等）で、`failed`（**失敗**）とは別の事実。`canceled` を `failed` に丸めると録画一覧が「失敗した」と嘘をつくので、独立した 4 値目として持つ
- **これは不変条件 7「mirakc 固有の概念を永続テーブルに入れない」の違反ではない。** 既存 3 値（`recording`/`finished`/`failed`）はもともと mirakc の語彙であり、`canceled` の追加はその踏襲。不変条件 7 が禁じるのは mirakc の内部 ID・タグ形式・スケジュール状態（`RecordingScheduleState` 等）のような実装詳細に紐づく構造の持ち込みで、録画結果の語彙（成功/失敗/取消）はドメインの外部仕様として妥当な粒度
- **未知の値（mirakc が将来値を追加した場合）は 5 値目を足さず `failed` に丸める。** `internal/watcher.normalizeRecordingStatus` が正規化し、生の値は `record_sync.status`（CHECK 無し）にそのまま残るので観測は失われない。丸めるのは「分かっている 2 つの事実を潰す」`canceled`→`failed` の丸めとは異なり、「何が起きたか分からない」という状態そのものが事実であるケースなので、粗い `failed` への集約を許容している。次に mirakc が値を足したら `internal/watcher` の ERROR ログを起点にこの CHECK と `openapi.yaml` の enum を更新すること

**#98 との関係:** #98 は「schedule が一度も作られなかった予約」について Rokuban 自身の観測（never-scheduled）を `recordings` に残すことを検討しているが、それは上記の権威（mirakc が報告した状態）とは別の事実である。#98 を実装する際にこの列へ混ぜないこと —— 別列にするか、この列の意味自体を作り直すかは #98 側で決める。

### ごみ箱（issue #4 の削除エンジンコメント / M3-7 #69）

- UI の削除 = `deleted_at` を立てるだけ。ファイルには触れない。復元 = `deleted_at`（と `purge_after`）を消すだけ
- 「今すぐ完全削除」= `purge_after = now()` を立てるだけ（未 soft-delete なら `deleted_at` も同時に立てる）。**ファイルは消さない**
- 物理削除は削除 reconcile ループ（M3-8）が次のいずれかを拾ってアセット単位で実行する:
  - `purge_after IS NOT NULL AND purge_after <= now()`（即時要求）
  - `deleted_at + 猶予期間（既定 30 日）` 経過
- 物理削除後も recordings 行と media_assets の tombstone は残る → ごみ箱を空にしても録画履歴・ドロップ統計・重複排除は壊れない
- API: `DELETE /api/recordings/{id}` / `POST .../restore` / `POST .../purge` / `GET /api/recordings?trash=true`

### 同一イベントの重複防止（`00003`）

```sql
CREATE UNIQUE INDEX recordings_unique_active_event
    ON recordings (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL;
```

**録画試行の履歴は複数行を許すが、「生きている録画」は 1 イベントにつき 1 つ**。`deleted_at IS NULL` の部分インデックスなので、ごみ箱に入れた後で録り直すことはできる。

この制約があるため、watcher が同一 record を並行処理すると片方が制約違反で失敗する。`processRecord` は `record_sync` の行を先に確保して直列化することでこれを避けている（[録画エンジン](../recording.md) §3.3「record 処理は並行実行しても壊れない」）。

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

