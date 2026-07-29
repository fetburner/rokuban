## M2: メトリクスと出口基準

手順そのものは [m2.md](m2.md)。ここは **M2 が終わったと言えるか**を判定する材料。

### M2 で見るメトリクス

| メトリクス | 読み方 |
|---|---|
| `rokuban_ruler_last_pass_timestamp_seconds` | `time() - この値` が周期（10 分）を大きく超えたら ruler が止まっている |
| `rokuban_ruler_reservations_total{action}` | `created` / `updated` / `deleted` / `gc`。差分書き込みなので収束後は増えないのが正常 |
| `rokuban_reconcile_last_pass_timestamp_seconds` | 同上（30 秒） |
| `rokuban_reconcile_pending_diff{action}` | `create` / `delete` / `update` は**ゼロに収束すべき**。`update_deferred` は録画中の番組を意図的に触っていないぶんで、**非ゼロが正常でありうる** |
| `rokuban_reconcile_schedules_total{action}` | 実際に差分を消した量（`created` / `deleted` / `recreated`） |
| `rokuban_reconcile_schedule_lost_total` | 再作成の DELETE 成功 → POST 失敗。**0 以外はアラート**（次パスまでに開始時刻を過ぎると取りこぼす） |
| `rokuban_reconcile_start_delayed{site}` | 開始遅延。ゼロに収束すべき |
| `rokuban_circuit_breaker_tripped{site,breaker}` | 1 の間、導出削除が止まっている。**時間ではなく即座に通知する対象** |
| `rokuban_tuners_projected{site}` / `rokuban_tuner_sync_last_success_timestamp_seconds{site}` | 射影が生きているか。0 / 古いなら容量判定は無効 |
| `rokuban_capacity_overages{site}` | 構成の余裕を眺めるゲージ。**非ゼロは信頼できるが、ゼロは保証ではない** |
| `rokuban_sweep_last_pass_timestamp_seconds` | `record_sweep`（5 分）が生きているか |

### 沈黙は保証ではない

M2 で足した観測のうち、**「出ていない = 大丈夫」と読んではいけない**ものを 1 箇所に
まとめる。設計上ここは全部「警告を見逃す」側に偏らせてある（過剰に警告しない代わりに
沈黙が保証にならない、という取引）。

| 沈黙 | 何を意味しないか | 何を見るか |
|---|---|---|
| `/api/capacity/overages` が空 | 収まるとは限らない。並走 EPGStation・ライブ視聴・EPG 収集は見えず、mirakc の `excluded_channels` は `/api/tuners` に載らないので**知る術がない** | `rokuban_tuners_projected` が 0 でないこと |
| 同上（射影が空） | 射影が 1 行も無いサイトは**何も主張しない**ので、同期が壊れると警告が黙って消える | `tuner_sync` の行と `tuner_sync_last_success` の鮮度 |
| `drop-stats` の `pidType` が無い | 分類できなかっただけで、ドロップ統計そのものは正しい | `packets` / `drops` は種別と独立に信頼できる |
| `pidType` が `other` | 音声でないとは限らない（LATM AAC は `other` に落ちる） | 4K/8K を録ったなら疑う |
| `/api/programs/{id}/overlaps` の `count = 0` | 録れるとは限らない（他サイトや mirakc の他の消費者は数えていない） | 手順 10 |
| `/api/breakers` が空 | 削除が正しかったとは限らない。**閾値を下回る削除は素通りする** | `rokuban_ruler_reservations_total{action="deleted"}` の増え方 |
| `rokuban_reconcile_start_delayed` が 0 | 録画が始まったことの確認ではない（猶予 3 分の内側は検出しない） | `recordings.started_at` |

### 既知の未解決事項（誤読しやすいもの）

**`orphaned` は「録れなかった」を意味しないことがある。** `markOrphaned`
（`internal/reconciler/reconciler.go` の `markOrphaned`）の判定は「番組終了時刻を過ぎた」と
「mirakc の schedule に観測されない」の 2 つだけで、**`recordings` 行の有無を見ていない**。
mirakc が録画完了後に schedule を落とすなら、**成功した録画の予約も `orphaned` に
なりうる**。`docs/schema.md` §3 の定義（録画されずに終わった行）と食い違う。

- **未検証。** 分岐点は「録画完了後に mirakc が `GET /api/recording/schedules` から
  schedule を落とすか」で、実機で確認していない。落とさないなら起きない
- 並走中に `orphaned` を見たら、まず `recordings` に対応行があるか確かめる。
  あるならこの既知の挙動で、録画は成功している
- GC は終了 + 24 時間なので、その間 UI には `orphaned` として見える
- 重複排除の自己一致を許していたらこの挙動は dedup の skip に隠れていた
  （`effective.skip` で `listDesired` から落ちるため）。自己一致を除外する決定に
  したので**隠れずに残っている**。切り分けはこちらの方が楽

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT r.id, r.program_id, s.title, r.orphaned_at,
          (SELECT count(*) FROM recordings rec WHERE rec.reservation_id = r.id) AS recordings
     FROM reservations r
     JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
    WHERE r.orphaned_at IS NOT NULL ORDER BY s.start_at DESC"
```

（`title` / `program_start_at` は Phase 1 で `program_snapshots` に抽出され、`reservations.state` は `orphaned_at` に置き換わった。[スキーマ](../schema.md) §3 / §3.7）

**`pidType` が `other` の音声 PID**（LATM AAC / `stream_type = 0x11`）も同種の
「誤読しやすい正常」。`gots` の `IsAudioContent()` の値域に従っているだけで、
自前の `stream_type` 表は作らない方針。観測したら 1 行で足せる。

**容量超過の帯 UI は未実装。** バックエンド（`/api/capacity/overages`）とグリッド側の
口（`overlay`）は揃っているので、超過はグリッドを見ても分からず API で確認する。

### 出口基準チェックリスト（M2）

M2 の出口基準は「ルールで録れ、除外・上書きが生き残り、予約差分がゼロ or 全件説明可能」
（[issue #6](https://github.com/fetburner/rokuban/issues/6) / #24 の M2-14）。

- [ ] `POST /api/programs/search` の結果と、同じ条件のルールが作る予約の集合が一致する
- [ ] `POST /api/rules` の後、**ヒント経路で**（10 分待たずに）`ruler: pass complete` が出て予約ができる
- [ ] その予約が mirakc に `rokuban:reservation=<id>` tag 付きで入り、実際に録画される
- [ ] ルールを `PATCH` で無効化（`enabled: false`）すると次の ruler パスで予約が消える（ruler は `enabled = true` のルールだけを評価する）
- [ ] `DELETE /api/rules/{id}` の応答の `deletedReservations` / `detachedReservations` が実際の予約の増減と一致する
- [ ] 上書きのない予約を `DELETE /api/reservations/{id}` すると**次の ruler パスで復活しない**（`program_intents` に skip が残る）
- [ ] 上書きのある予約を取消すと `skip = true` の説明行として現れる（消えたままにはならない。手順 5）
- [ ] `PATCH /api/reservations/{id}` の `priority` が mirakc の schedule に反映される（DELETE + POST の再作成）
- [ ] `reset` / `DELETE .../overrides` で上書きが戻り、ルールの値に戻る
- [ ] 同じ番組を 2 回録った後、重複排除が 2 回目を `skip` にし、`dedupMatchRecordingId` / `dedupSimilarity` が入る
- [ ] その予約を取消 → 再予約すると `skip` が false になり（`action='record'` が勝つ）、根拠 2 列は残る
- [ ] 重なる時間帯の番組で「同じ時間帯に N 件の予約があります」が予約前に見える
- [ ] `GET /api/breakers` が平常時は空配列である
- [ ] `ruler.max_deletes_per_pass` を 1 に下げてマッチ数の多いルールを無効化すると発動し、バナーが出る。**閾値を戻してから** `resume` すると消える（超えたまま再開すると次のパスで再発動する）
- [ ] `tuner_sync` に実機のチューナーが投影され、`rokuban_tuners_projected` が本数と一致する
- [ ] チューナー本数を超える予約を意図的に入れると `/api/capacity/overages` に区間が出て、`jammedTypes` が実際の種別と一致する
- [ ] 録画のドロップ統計に `pidType` が付き、`video` / `audio` / PSI テーブル名が妥当に見える
- [ ] **実ブラウザで**全サービス × 24 時間のグリッドがスクロールできる（前述の 3 項目）
- [ ] `rokuban shadow-diff` が **`RokubanOnly` / `EPGStationOnly` ともゼロ**、または全件が `Expected` で説明できる
- [ ] 数日放置しても `rokuban_ruler_last_pass_timestamp_seconds` / `rokuban_reconcile_last_pass_timestamp_seconds` が更新され続けている
- [ ] `rokuban_reconcile_pending_diff{action="create"}` と `{action="delete"}` がゼロに戻る（`update_deferred` は非ゼロでよい）

