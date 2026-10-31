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

この行の読み手は DB-backed metrics collector だけで、予約の desired/observed の導出結果や mirakc の予約 ID は永続化しない。reconciler が停止した場合はマーカーの時刻が古いまま残り、常駐プロセスの `/metrics` から観測できる。

<details><summary>根拠</summary>

- マーカーの時刻: `snapshot_at`
- 観測表: `schedule_sync`

</details>

### ruler_pass_snapshots / record_sweep_snapshots — ループのパス鮮度

ループの鮮度は、サイトごとに最後に成功したパスの時刻を持つ 2 つの衛星表で見る。ruler と record_sweep のパスが正常に完了した最後の時刻だけを記録する。

ループの鮮度を、予約・record の観測・録画履歴の行の有無から推測しないための表である。それらの表に列を足さないのは、書き手も寿命もループの鮮度とは別だからである（不変条件 12）。

<details><summary>根拠</summary>

- 持つ列: `(site PK, last_success_at)` だけ
- 推測の対象にしない表: `reservations` / `record_sync` / `recordings`

</details>
