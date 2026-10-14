> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§7）。

## 7. record_sync — mirakc record の観測（observed state）と drop_stats

この節は、mirakc の record の観測を持つ表と、原本の解析結果を持つ 2 つの表をまとめる。

### record_sync

record の観測表は、mirakc の全 record を写し、外部産の record も行を作って観測だけする。一覧取得が成功したとき、応答に含まれない record の行をサイト単位で削除する。書くのは watcher だけで、**mirakc の record id が存在するのはこの表だけ**である。永続側の録画履歴へは、録画履歴の id で片方向に指す。

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

<details><summary>根拠</summary>

- 観測の取得元: `GET /api/recording/records` と SSE
- 永続側へ指す列: `recording_id`（`recordings` を指す）
- mirakc の record id を持つ列: `record_id`（`record_sync` だけ）

</details>

#### 外部産 record の扱い

外部産の record は、行を作って観測だけし、消さない。録画 id が空なのは外部産の代理にすぎず、判別は mirakc の生の印で行う。

この表は mirakc の全 record を写す。watcher が変えるのは Rokuban の録画行を指すかどうかだけで、外部産 record（Rokuban の印が無い record）も行を作って観測だけする。友人の手動録画や他ツールの録画を消さないための境界であり、印の判定が単独で保証する。観測表のスキーマや、録画 id の NULL には依存しない。

録画 id が NULL の record は ingest の対象外である。判別の根拠は tags の印で、録画 id の NULL は、外部産と「録画履歴が物理削除された」を区別しない。

<details><summary>根拠</summary>

- 外部産の判定: `IsOurs`（tag 判定。`mirakc.IsOurs`）。判別の根拠は `tags`
- 外部産の行: `recording_id IS NULL`。物理削除でも NULL になる（`ON DELETE SET NULL`）

</details>

#### 行の寿命

ingest ジョブは、取り込む record をこの観測表から取る。行の寿命は mirakc 上の record と一致させる。watcher は一覧取得が成功したとき、応答に含まれない record の行を、サイト単位で削除する。

削除は、snapshot の処理より前に行う。後に置くと、一覧取得の後に SSE が作った行を巻き込む。削除対象は、観測時刻の比較では決めない。SSE と sweep の処理が並行し、トランザクションの開始時刻とコミット時刻がずれるためである。

<details><summary>根拠</summary>

- ingest の取得キー: `(site, record_id)`
- 一覧取得: `ListRecords`
- snapshot の処理: `processRecord`
- 比較に使わない列: `observed_at`

</details>

#### 未 ingest の集計

「未 ingest record 総量」メトリクスは、この表の集計である。ingest のサイト単位の同時実行キャップも、site 列で分ける。このメトリクスが数えるのは Rokuban 自身の滞留だけなので、集計は外部産を除く。

<details><summary>根拠</summary>

- 外部産を除く条件: `recording_id IS NOT NULL`

</details>

### drop_stats — PID 別ドロップ統計（永続資産）

ドロップ統計は、原本の解析結果を PID ごとに持つ永続資産で、原本アセットに紐づき、tombstone 化した後も残る。PID の分類は PAT と PMT のストリーム種別までで、記述子は読まないので、字幕と文字スーパーは区別できない。分類できなかった PID は空値で持ち、空文字は永続化しない。

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

<details><summary>根拠</summary>

- 解析ジョブ: `ts_scan`（188 バイト境界、読み取り専用）
- 計測時のサイズの記録: `media_asset_ts_scans`

</details>

#### 書き手と PID 種別

原本のコミット後に、解析ジョブが書く。解析記録は計測時のサイズを持ち、保持エンジンは、原本の現在サイズと一致する解析記録を要求する。

PID 種別に CHECK は置かない。値の権威は Go 側の公開定数で、ブレーカー名と同じ理由、つまり分類の追加をマイグレーションなしでできるようにするためである。値は、映像・音声・その他・固定 PID 名・PMT の PID に分かれる。その他は PMT に載っているが映像でも音声でもない ES で、字幕・文字スーパー・データ放送はすべてここに入る。**分類できなかった PID は NULL にする。** 空文字は永続化しない。

<details><summary>根拠</summary>

- 値の権威: `internal/tsstat` の公開 const。同じ理由の例は `circuit_breakers.name`
- 値集合: `video` / `audio` / `other` / 固定 PID 名 `pat` `cat` `nit` `sdt` `eit` `tot` / `pmt`（PAT が PMT の在り処として指した PID）
- 対象の列: `pid_type`

</details>

#### 分類の限界

分類は PAT から PMT のストリーム種別までで、記述子は読まない。そのため字幕と文字スーパーは区別できない。ARIB では両方が同じストリーム種別になる。境界の理由は[録画エンジン](../recording.md) §1「例外の境界」にある。

PMT の更新（バージョン更新・番組の境目での PID 再割り当て）で同一 PID の分類が変わった場合は、最後に見たものを採用する。分類の変化回数は保存しない。

<details><summary>根拠</summary>

- ストリーム種別: `stream_type`。ARIB では字幕と文字スーパーの両方が `stream_type = 0x06`

</details>

### drop_positions — ドロップ位置（原本解析の観測結果）

ドロップ位置は、原本の解析がドロップを検知した位置を持つ観測結果で、録画開始からの経過時間で持つ。放送時刻では持たない。録画の開始時刻はチューナーを開いた時刻で、予定より早いので、放送時刻として扱うとずれが乗る。

合計だけでは、全域が壊れているのか、1 か所で数秒詰まっただけかが分からないために持つ。解析は、ドロップ統計と同じトランザクションで位置を置き換える。

```sql
CREATE TABLE drop_positions (
    media_asset_id bigint NOT NULL REFERENCES media_assets (id),
    byte_offset    bigint NOT NULL,
    pid            integer NOT NULL,
    elapsed_ms     bigint,             -- 録画開始からの経過。PCR 未観測なら NULL
    PRIMARY KEY (media_asset_id, byte_offset)
);
```

<details><summary>根拠</summary>

- 合計を持つ表: `drop_stats`
- 位置を書く解析: `ts_scan`

</details>

#### 位置の残り方

統計は、原本が残っている間は解析を再実行して作り直せる。原本の削除は、現在のサイズと一致する計測記録を待つので、原本の削除後も位置は解析時の観測として残る。エンコード後の出力からは欠落位置を復元できない。デコーダの error concealment が、隠蔽の跡を残さないためである。

<details><summary>根拠</summary>

- 削除の待ち条件: `until_encoded` の削除は、現在の `media_assets.size_bytes` と一致する計測記録を待つ

</details>

#### 主キーと列の形

主キーはメディアアセットとバイト位置の組である。1 パケットは常に 1 PID にしか属さないので、同じ原本内の同じバイト位置で 2 つの PID がドロップを起こすことはなく、連番の代理キーは要らない。

経過時間は、最初に観測した PCR を録画開始とみなした相対経過である。PCR を 1 つも観測していなければ NULL にし、採れなかったことを 0 のような値にしない。PID 種別を NULL にするのと同じ理由である。

保存件数を「真の drops」と別の列で持たない。保存件数が上限に達したかどうかは、保存した行数とドロップ統計の合計を比べれば導出できる。値を複製する列は作らない。

<details><summary>根拠</summary>

- 主キー: `(media_asset_id, byte_offset)`
- 経過時間の列: `elapsed_ms`。PCR 未観測なら NULL。`drop_stats.pid_type` を NULL にするのと同じ理由
- 持たない別列: `truncated` など。導出は `count(*) < drop_stats.drops`

</details>

#### 経過時間での持ち方

ドロップ位置は絶対放送時刻では持たず、録画開始からの経過で持つ。録画履歴の開始時刻は mirakc がチューナーを開いた時刻で、予定時刻より約 15 秒早い。これは構造的なずれで、実測もある。開始時刻に経過時間を足して放送時刻として扱うと、この誤差が乗る。そのため、API も UI も経過時間のまま扱う。

<details><summary>根拠</summary>

- 録画の開始時刻: `recordings.started_at`
- 誤差が乗る計算: `started_at + elapsed_ms`

</details>
