> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§4）。

## 4. schedule_sync — mirakc schedule の観測（observed state）

`GET /api/recording/schedules` の全量取得結果をそのまま写像した使い捨てテーブル。reconciler だけが書く。**mirakc の形をしてよい唯一の予約側テーブル**。

```sql
CREATE TABLE schedule_sync (
    site           text   NOT NULL,
    program_id     bigint NOT NULL,             -- mirakc schedule のキー（site 単位のスコープ）
    state          text   NOT NULL,             -- mirakc の状態をそのまま (scheduled/tracking/recording/…)
    options        jsonb  NOT NULL,             -- 観測された RecordingOptions そのまま
    tags           text[] NOT NULL DEFAULT '{}',
    failed_reason  jsonb,                       -- mirakc の FailedReason そのまま
    observed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);
```

- 全量同期はサイト単位に、upsert + 「今回観測されなかった行の削除」を 1 トランザクションで行う（あるサイトへの疎通断が他サイトの観測を消さない）
- **削除対象の軸は `mirakc.IsOurs(tags)` である。** `IsOurs` が false（新旧いずれの rokuban tag も無い = 外部産）の schedule だけを触らない（[reconciler.md](../recording/reconciler.md)「tags 対応付け」）。かつて存在した `reservation_id` 列を落とした経緯は [00028_schedule_sync_drop_reservation_id.sql](../../internal/db/migrations/00028_schedule_sync_drop_reservation_id.sql) 参照
- mirakc の enum（state / failedReason）は text / jsonb のまま持ち、CHECK は付けない — mirakc 側の追加に追従するため

