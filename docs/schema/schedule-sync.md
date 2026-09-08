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
- **削除対象の軸は `mirakc.IsOurs(tags)` である。** `IsOurs` が false（rokuban tag が無い = 外部産）の schedule だけを触らない（[reconciler.md](../recording/reconciler.md)「tags 対応付け」）。かつて存在した `reservation_id` 列は、書き手はいたが読み手が本番コードに 1 つも無かったため落とした
- mirakc の enum（state / failedReason）は text / jsonb のまま持ち、CHECK は付けない — mirakc 側の追加に追従するため

### schedule_sync_snapshots — サイト単位の全量観測マーカー

```sql
CREATE TABLE schedule_sync_snapshots (
    site         text PRIMARY KEY,
    snapshot_at  timestamptz NOT NULL DEFAULT now()
);
```

`snapshot_at` は mirakc への GET が成功した時刻ではなく、`schedule_sync` の全量
upsert、stale 行の削除、マーカー更新が 1 つのトランザクションでコミットされた時刻。
そのため、空の全量 snapshot と観測ループ停止を `schedule_sync` の行の有無だけで
推測しない。マーカーが無いサイトは「まだ一度も全量 snapshot を確定していない」。

この行の読み手は DB-backed metrics collector だけで、予約の desired/observed の
導出結果や mirakc の予約 ID は永続化しない。reconciler が停止した場合は
`snapshot_at` が古いまま残り、常駐プロセスの `/metrics` から観測できる。
