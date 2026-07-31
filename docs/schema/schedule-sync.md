## 4. schedule_sync — mirakc schedule の観測（observed state）

`GET /api/recording/schedules` の全量取得結果をそのまま写像した使い捨てテーブル。reconciler だけが書く。**mirakc の形をしてよい唯一の予約側テーブル**。

```sql
CREATE TABLE schedule_sync (
    site           text   NOT NULL,
    program_id     bigint NOT NULL,             -- mirakc schedule のキー（site 単位のスコープ）
    reservation_id bigint REFERENCES reservations (id) ON DELETE SET NULL,
                                                -- tag のパースを経由せず、schedule 自身が持つ program_id で
                                                -- reservations を (site, program_id) 引きして解決する。ただし
                                                -- IsOurs(tags) が false（新旧いずれの rokuban tag も無い）なら
                                                -- 埋めない。NULL は「外部産」または「自分が作ったが対応する
                                                -- 予約が既に消えている」のいずれか（後者は削除対象の判定には
                                                -- 使わない。下記参照）。tag 形式の詳細は
                                                -- [reconciler.md](../recording/reconciler.md)「tags 対応付け」参照
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
- **reconciler が削除対象にするかどうかの軸は `reservation_id` ではなく `mirakc.IsOurs(tags)`。** `IsOurs` が false（新旧いずれの rokuban tag も無い = 外部産）の schedule だけを触らない。`IsOurs` が true で `reservation_id` が NULL（= 自分が作ったが対応する予約は既に消えている）の schedule は desired に無ければ削除される — 予約が消えた後の schedule を残さないため、この経路こそが必要（[reconciler.md](../recording/reconciler.md)「tags 対応付け」）
- mirakc の enum（state / failedReason）は text / jsonb のまま持ち、CHECK は付けない — mirakc 側の追加に追従するため

