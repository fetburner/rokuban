> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§7）。

## 7. record_sync — mirakc record の観測（observed state）と drop_stats

### record_sync

`GET /api/recording/records` と SSE の観測結果。watcher だけが書く。**mirakc の record id はこのテーブルにしか存在しない** — 永続側（recordings）へは `recording_id` で片方向に指す。

```sql
CREATE TABLE record_sync (
    site           text   NOT NULL,
    record_id      text   NOT NULL,             -- mirakc の 32 桁 hex（site 単位のスコープ）
    recording_id   bigint REFERENCES recordings (id) ON DELETE SET NULL,  -- NULL = 外部産 record（触らない）
    program_id     bigint NOT NULL,
    status         text   NOT NULL,             -- mirakc の recordingStatus そのまま
    content_path   text,
    content_length bigint,
    tags           text[] NOT NULL DEFAULT '{}',
    failed_reason  jsonb,
    observed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, record_id)
);

CREATE INDEX ON record_sync (recording_id);
CREATE INDEX ON record_sync (status);
```

- ingest ジョブは (site, record_id) をここから取る。ingest コミット → エッジ record 削除 → 次回同期で行が消える（リングバッファの写像）
- `recording_id IS NULL`（rokuban タグのない record）は ingest 対象外
- 「未 ingest record 総量」メトリクスはこのテーブルの集計。ingest のサイト単位同時実行キャップも site 列で分割する

### drop_stats — PID 別ドロップ統計（永続資産）

ingest のインライン TS スキャン（188 バイト境界、読み取り専用）の結果。原本アセットに紐づき、**tombstone 化後も残る**。

```sql
CREATE TABLE drop_stats (
    media_asset_id bigint  NOT NULL REFERENCES media_assets (id),
    pid            integer NOT NULL,
    packets        bigint  NOT NULL,   -- 総パケット数
    drops          bigint  NOT NULL,   -- continuity counter 不連続
    errors         bigint  NOT NULL,   -- transport_error_indicator
    scrambled      bigint  NOT NULL,   -- scrambling_control ≠ 0（> 0 は B-CAS 系異常の別枠アラート）
    pid_type       text,               -- PID 種別。分類できなければ NULL
    PRIMARY KEY (media_asset_id, pid)
);
```

- ingest ジョブが media_assets のコミットと同一トランザクションで一括 INSERT
- `pid_type` に CHECK は置かない。値の権威は Go 側（`internal/tsstat` の公開 const）で、
  `circuit_breakers.name` と同じ理由（分類の追加をマイグレーションなしでできるようにする）。
  値集合は `video` / `audio` / `other`（PMT に載っているが映像でも音声でもない ES。
  字幕・文字スーパー・データ放送はすべてここ）/ 固定 PID 名 `pat` `cat` `nit` `sdt` `eit` `tot` /
  `pmt`（PAT が PMT の在り処として指した PID）。**分類できなかった PID は NULL**（空文字は永続化しない）
- 分類は PAT → PMT の `stream_type` までで**記述子は読まない**ため、字幕と文字スーパーは
  区別できない（ARIB では両方 `stream_type = 0x06`）。境界の理由は
  [録画エンジン](../recording.md) §1「例外の境界」
- PMT の更新（version 更新・番組の境目での PID 再割り当て）で同一 PID の分類が変わった場合は
  **最後に見たものを採用**する。変化の回数は ingest の転送完了ログ（`pid_type_changes`）にだけ残し、
  列にはしない（導出値であり、統計の正しさに影響しない）

### drop_positions — ドロップ位置（不可逆な観測）

`drop_stats` が畳んだ合計だけでは「全域が壊れているのか、1 か所で数秒詰まっただけか」が分からない。
ingest のインライン TS スキャンが drop を検知した瞬間の位置を、drop_stats と同一トランザクションで残す。

```sql
CREATE TABLE drop_positions (
    media_asset_id bigint NOT NULL REFERENCES media_assets (id),
    byte_offset    bigint NOT NULL,
    pid            integer NOT NULL,
    elapsed_ms     bigint,             -- 録画開始からの経過。PCR 未観測なら NULL
    PRIMARY KEY (media_asset_id, byte_offset)
);
```

- 原本のバイトが Rokuban のプロセスを通るのは ingest の 1 パスだけで、`keep_original='until_encoded'`
  により原本は既に削除される運用が前提にある。エンコード後の出力からは欠落位置を復元できない
  （デコーダの error concealment が隠蔽の跡を残さない）ため、位置は**再現できない導出値ではなく
  ingest の 1 回でしか採れない不可逆な観測**である
- 主キーが `(media_asset_id, byte_offset)` なのは、1 パケットは常に 1 PID にしか属さないため。
  同じ原本内の同じバイト位置で 2 つの PID がドロップを起こすことはなく、連番の代理キーは要らない
- `elapsed_ms` は最初に観測した PCR を録画開始とみなした相対経過で、PCR を 1 つも観測していなければ
  NULL にする（`drop_stats.pid_type` を NULL にするのと同じ理由。採れなかったことを 0 のような
  値ではなく NULL で表す）
- 保存件数を「真の drops」と別列（`truncated` 等）で持たない。`count(*) < drop_stats.drops` で
  上限に達したかどうかを導出できるため、値を複製する列を作らない
- 絶対放送時刻ではなく「録画開始からの経過」で持つ。`recordings.started_at` は mirakc がチューナーを
  開いた時刻で、予定時刻より約 15 秒早い（構造的なずれで、実測もある）。`started_at + elapsed_ms` を
  放送時刻として扱うとこの誤差が乗るため、API も UI も経過時間のまま扱う

