## 4. schedule_sync — mirakc schedule の観測（observed state）

`GET /api/recording/schedules` の全量取得結果をそのまま写像した使い捨てテーブル。reconciler だけが書く。**mirakc の形をしてよい唯一の予約側テーブル**。

```sql
CREATE TABLE schedule_sync (
    site           text   NOT NULL,
    program_id     bigint NOT NULL,             -- mirakc schedule のキー（site 単位のスコープ）
    reservation_id bigint REFERENCES reservations (id) ON DELETE SET NULL,
                                                -- tags の rokuban:reservation=<id> から解決。NULL = 外部産 schedule
    state          text   NOT NULL,             -- mirakc の状態をそのまま (scheduled/tracking/recording/…)
    options        jsonb  NOT NULL,             -- 観測された RecordingOptions そのまま
    tags           text[] NOT NULL DEFAULT '{}',
    failed_reason  jsonb,                       -- mirakc の FailedReason そのまま
    observed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

CREATE INDEX ON schedule_sync (reservation_id);
```

- 全量同期はサイト単位に、upsert + 「今回観測されなかった行の削除」を 1 トランザクションで行う（あるサイトへの疎通断が他サイトの観測を消さない）
- `reservation_id IS NULL` の行は Rokuban 以外が入れた schedule。reconciler は**触らない**（削除しない）
- mirakc の enum（state / failedReason）は text / jsonb のまま持ち、CHECK は付けない — mirakc 側の追加に追従するため

