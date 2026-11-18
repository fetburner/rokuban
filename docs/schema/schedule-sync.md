> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§4）。

## 4. schedule_sync — mirakc schedule の観測（observed state）

schedule の観測表は、mirakc の schedule を全量取得した結果をそのまま写した使い捨ての表で、書くのは reconciler だけである。全量同期は、取得に含まれない行を観測表から消す。**mirakc の形をしてよい唯一の予約側テーブル**である。

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

<details><summary>根拠</summary>

- 取得元: `GET /api/recording/schedules`

</details>

### 全量同期と削除の範囲

全量同期は、サイト単位に、upsert と「今回観測されなかった行の削除」を 1 つのトランザクションで行う。あるサイトへの疎通断が、他サイトの観測を消さない。

観測表の削除は、外部産かどうかを区別しない。印の判定は、reconciler が mirakc 側の schedule を消すかどうかの判定に使う。印が無い外部産の schedule は、mirakc 側では触らない（[reconciler.md](../recording/reconciler.md)「tags 対応付け」）。かつてあった予約 id の列は、書き手はいたが読み手が本番コードに 1 つも無かったため落とした。

<details><summary>根拠</summary>

- 印の判定: `mirakc.IsOurs(tags)`。`IsOurs` が false（rokuban tag が無い = 外部産）の schedule は、reconciler が mirakc 側で触らない
- 落とした列: `reservation_id`

</details>

### mirakc の enum は CHECK を付けない

状態と失敗理由は、mirakc の値のまま text / jsonb で持ち、CHECK は付けない。mirakc 側の追加に追従するためである。

<details><summary>根拠</summary>

- mirakc の enum: `state` / `failedReason`

</details>

### schedule_sync_snapshots — サイト単位の全量観測マーカー

全量観測マーカーは、全量の反映が 1 つのトランザクションでコミットされた時刻を、サイトごとに 1 行持つ。取得が成功した時刻は使わない。空の全量取得と観測ループの停止を、観測表の行の有無から推測しないために持つ。マーカーが無いサイトは、まだ一度も全量 snapshot を確定していない。

```sql
CREATE TABLE schedule_sync_snapshots (
    site         text PRIMARY KEY,
    snapshot_at  timestamptz NOT NULL DEFAULT now()
);
```

コミットの単位は、全量 upsert・stale 行の削除・マーカー更新である。mirakc への GET が成功しただけでは、時刻は進まない。

このマーカーを `loop_success_markers` に入れないのは、意味が違うからである。`snapshot_at` は `schedule_sync` の中身がいつの観測かを表す観測の属性である。presync collector は `ScheduleSync.SnapshotAt` 経由で観測行と組にして読み、pending を信じてよいかを判断する。ループの成功時刻はパスの成否しか表さず、他の表の中身がいつ時点かは表さない。加えて、入れると `ScheduleSync` と `LoopMarkers` という 2 つの store 契約が同じ表に書くことになる。

この行の読み手は DB-backed metrics collector だけで、予約の desired/observed の導出結果や mirakc の予約 ID は永続化しない。reconciler が停止した場合はマーカーの時刻が古いまま残り、常駐プロセスの `/metrics` から観測できる。

<details><summary>根拠</summary>

- マーカーの時刻: `snapshot_at`
- 観測表: `schedule_sync`

</details>

### loop_success_markers — ループの成功時刻

ループの成功時刻は 1 つの衛星表に持つ。行は、ループの種類と scope の組ごとに、最後に成功した時刻を表す。

loop は ruler・record_sweep・reconciler・reconcile_pass・delete_reconcile のどれかである。scope は loop ごとに決まる。ruler・record_sweep・reconciler はサイトである。reconciler は予約同期のパスで、全件パスの reconcile_pass とは別の loop である。delete_reconcile は置き場の名前、reconcile_pass は全件パスの名前（encode / thumbnail / ts_scan）である。

書き手は、そのループのパスが完了した後だけ書く。行は次の成功で上書きされ、行数は scope の数で頭打ちになる。すべての loop の行は同じ寿命なので、1 つの表にまとめる（不変条件 12）。置き場を持たない全件パスは、scope に pass の名前を入れる。空の値を番兵にして同じ表の別の意味に使うことはしない。

行の存在は「その scope が一度は成功した」だけを主張する。未成功の scope には行を作らない。ループの鮮度を、予約・record の観測・録画履歴の行の有無から推測しないための表である。

<details><summary>根拠</summary>

- 持つ列: `(loop, scope)` PK と `last_success_at` だけ
- loop の集合: CHECK 制約で閉じる
- 推測の対象にしない表: `reservations` / `record_sync` / `recordings`
- 読み手: 監視の collector だけ（API・readyz・UI は読まない）

</details>
