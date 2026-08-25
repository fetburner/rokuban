> [data.md](../data.md) §1 §2 §3 §7 の一部。索引から辿る

## 1. 方針: PostgreSQL 一本

### MySQL との比較

| 機能 | PostgreSQL | MySQL 8.0 |
|---|---|---|
| SKIP LOCKED | あり | あり（同等） |
| アドバイザリロック | `pg_advisory_lock` | `GET_LOCK()`（代用可能） |
| コミット連動の pub/sub | **LISTEN/NOTIFY** | **なし**（ポーリングか外部ブローカー） |
| ジョブキューライブラリ | River / pg-boss / Oban / Solid Queue 等が成熟 | この層がほぼ空白 |
| 日本語検索 | pg_trgm / PGroonga | InnoDB FTS + ngram（見劣り） |
| JSON | JSONB + GIN | JSON 型 + 生成列（やや不便） |

調停プリミティブ（SKIP LOCKED / アドバイザリロック相当）だけなら MySQL でも成立する。決定打は **LISTEN/NOTIFY の不在**（ブローカーレス設計の根幹。MySQL では変更伝搬がポーリングしかなく、反応性を上げるほど DB を無駄に叩く）と、**その上に載る既製ライブラリの層が MySQL には育っていない**こと。

## 2. ジョブキュー: River（Postgres バックエンド）

エンコード・取り込み・サムネイル・掃除はすべてジョブ。[River](https://github.com/riverqueue/river) を採用する。

- ワーカーは `FOR UPDATE SKIP LOCKED` で 1 件確保。複数ワーカーが同時に来ても行ロックで排他され、同一ジョブの二重実行はトランザクション分離の性質として起きない
- 実行中はリース（ハートビート）を延長。ワーカー死亡（OOM、プリエンプト）はリース切れで検出し、queued に戻す
- at-least-once なのでジョブは冪等に書く（出力は一時パスに書いて完了時に公開、DB 登録は `ON CONFLICT` で吸収）
- 常駐シングルトンロール（watcher）は `pg_advisory_lock` によるリーダー選出。セッション断で自動解放されるのでフェイルオーバーも自然に付く。k8s の Lease API に依存しないため monolithic mode でも同じコードが動く
  - **watcher のシングルトン性は「正しさ」の要件ではない**。record 処理は行ロックで冪等化されており、複数の watcher が同一 record を並行処理しても `recordings` は重複しない。シングルトンなのは「mirakc に N 本の SSE を張らない」という接続数の配慮に過ぎない。詳細は [録画エンジン](../recording.md) §3.3
- **ルール評価（ruler）と reconciler はシングルトンではなくジョブ**。定期・冪等・DB のみ（reconciler は mirakc への HTTP を伴うが、パスを跨ぐ接続や状態は持たない）・重複実行不可という性質が epg_sync と同じなので、どちらも River のジョブとして扱い、排他は advisory lock ではなく**ジョブロック + `UniqueOpts`**（args にサイトを含めるためサイト単位。別サイトの並行実行は正常）で担保する。機構が 1 つに減る

### 定期実行の契機はデプロイ形態に委ねる

River の `PeriodicJobs` は**リーダーに選出されたクライアントだけが投入する**。worker を KEDA で 0〜N にするとクライアントが 0 個になり、**誰も定期ジョブを投入しなくなる**。スケールアップのたびに `RunOnStart` が走るのも望ましくない。

そこで定期実行の契機をアプリの外に出せるようにする。

| デプロイ形態 | 定期投入の担い手 |
|---|---|
| Docker（monolith） | プロセス内の River `PeriodicJobs`（既定で有効） |
| k8s | **CronJob が `rokuban enqueue <job>` を叩く**（insert-only クライアントで 1 件投入して即終了） |

`PeriodicJobs` の登録は設定で切れるようにし、k8s では無効にする（両方有効だと二重投入になる。`UniqueOpts` で合流するので害は小さいが意図が曖昧になる）。副次的な利点として、k8s では**何がいつ走るかが CronJob の spec に一元化される**（Go のコードと YAML に散らない）。

### River のジョブ一意性の注意

`UniqueOpts.ByState` は**必ず明示する**。River の既定（`UniqueOptsByStateDefault`）は
`completed` と `discarded` を含むため、既定のままだと「一度終わった引数のジョブは
二度と投入できない」になる。定期ジョブ（`epg_sync`）は実質ワンショットになり、
破棄された ingest も再投入できない。Rokuban は
`available / pending / retryable / running / scheduled` に限定している
（同時実行を防ぐのが目的で、処理済みかどうかは DB の状態が真実）。

`unique_states` は**行ごとに保存される** — 一意索引は
`river_job_state_in_bitmask(unique_states, state)` を条件に持つ。つまり
**古い設定で投入された行は設定を直しても鍵を占有し続ける**。この設定を変更した際は
既存の該当ジョブ行を一度削除する必要がある:

```sql
DELETE FROM river_job WHERE kind = 'epg_sync' AND state = 'completed';
```

**`UniqueOpts.ByQueue` も明示する（既定 `false`）。** 既定では一意キーが
kind + args だけで組み立てられ、Queue を含まない（`ByArgs` と `ByQueue` は独立の
軸）。site 単位のキュー（`ingest` / `epg` / `reconciler` / `watcher`）と `cleanup` の
`InsertOpts` は `ByQueue: true` を立てている ---
**キュー名を変える（リネーム・site 修飾の追加）だけで、Queue を一意キーに
含めていないと旧キューの残骸が新キューへの Insert を `UniqueSkippedAsDuplicate`
として黙って塞ぐ**（エラーを返さないのでログにも出ない）。トラブルシュート手順は
[runbook/troubleshooting.md](../runbook/troubleshooting.md) 「デプロイ直後、旧キューの
残骸が `river_job` に残っている」を参照。

### Redis バックエンド（Sidekiq 系）を採用しない理由

1. **dual-write 問題**: 「録画完了を DB に登録」と「エンコードジョブを積む」は常にセットだが、書き込み先が DB と Redis に分かれると、コミット前 enqueue（ワーカーが未コミットデータを読む race）かコミット後 enqueue（隙間のクラッシュでジョブ消失）かの二択になる。真面目に解決すると outbox パターン = 結局 DB 上のキューを作ることになる
2. **ステートフル基盤が増える**: ミニ PC にはコンテナ 1 つ、クラウドにはマネージド Redis が 1 つ増え、バックアップ・監視・障害モードが 2 系統になる
3. **スループットが要らない**: このシステムの負荷は「1 日数十件、1 件数十分」のジョブ。キューに必要なのはスループットではなく耐久性と可視性（再起動で消えない・SQL で覗ける・ドメイン更新と同一トランザクション）で、これは RDB の得意領域
4. 傍証: Rails 8 が Solid Queue（DB バックエンド）をデフォルトに変えた。中規模まででは Redis を挟む価値が薄いという業界側の再評価

なお DB キューの一般的な弱点（数千 jobs/sec で vacuum・ロック競合が苦しい、コネクション数がワーカー数に比例）はこのワークロードでは遠く及ばない。

## 3. イベント通知: LISTEN/NOTIFY

NOTIFY はコミット時にのみ配送される組み込み pub/sub。各コンポーネントは NOTIFY を「何か変わった」のヒントとして受け、**実データは必ずテーブルから読み直す**（レベルトリガー）。通知の取りこぼしは定期 reconcile で収束するため、配送保証に依存しない。

**狭い例外は、テーブルに載せない揮発テレメトリ（エンコード進捗）だけ。** ffmpeg の
`out_time` を実入力 duration に対する割合にして最大 1 回/秒で NOTIFY し、notifier の
EventHub が接続中のブラウザへ SSE として渡す。**テーブルへ保存しない**のは、秒単位で
変わる復元不要の値を永続化すると再起動後に WAL・dead tuple・vacuum 対象を増やすため。
**完了判定には使わない**のは、NOTIFY に配送保証が無く、切断・遅参・notifier 停止で
欠落しても真実は `recording_encode_attempts` / `media_assets` の再読で復元できなければ
ならないため（不変条件 5）。

フロントエンド（[フロントエンドアーキテクチャ](../frontend.md) 参照）の SSE も同じ構造:
durable な状態のイベントは TanStack Query の `invalidateQueries` に徹し、真実は常に REST
から再取得する。エンコード進捗だけは上記の揮発テレメトリとして payload を直接表示し、
durable 状態が `running` でなくなれば破棄する。

**ブラウザへの SSE 配送 (`/api/events`) の担い手は notifier ロール**（api ではない）。api ロールは mirakc にもファイルシステムにも依存しない（不変条件 1）のと同じ理由で、長寿命接続である LISTEN セッションも持たない --- サーバーレス（scale-to-zero）に載せるためには、そもそも常駐前提の LISTEN を api に置けない。notifier は「Postgres を LISTEN してブラウザへ配り直すだけ」の小さな常駐プロセスとして分離してある。

notifier は**シングルトンではない**（`cmd/rokuban/server.go` の `singletonRoles` に含まれない）。各レプリカが独立に LISTEN し、自分に繋がっている SSE クライアントにだけ配る。レプリカ間で配送内容を調停する必要が構造的にない（各クライアントは 1 つのレプリカにしか繋がっておらず、そのレプリカの LISTEN だけが届けば十分）ため、レプリカを増やしても Redis アダプタのような追加の配送基盤は不要。この点は reconciler/ruler のジョブ化によりシングルトンが watcher だけになったのとは別の理由づけである点に注意 --- あちらは「冪等だから複数動いても壊れない」、notifier は「配送先が自分にぶら下がる接続だけなので調停自体が要らない」。

**もう一つの長寿命接続（watcher が mirakc から受ける SSE 購読）とは意図的に分離したまま**。「長寿命接続を持ち続ける」という機構は同じでも、相手も向きも障害時の影響範囲も無関係なので、1 つの抽象/プロセスにまとめない。詳細は [api.md](../api.md) の「2 つの SSE を 1 つに集約しない」を参照。

## 7. DB 輻輳時の隔離

「ユーザー操作で DB が詰まったら録画やエンコードに影響しないか」という懸念への整理。**故障モードは常に「収束の遅れ」であり、仕事の喪失ではない**:

- **録画は影響を受けない（DB 全停止でも走る）**。スケジュールは mirakc 側の schedules.json に永続化済みで、録画実行は mirakc が自律的に行う。DB 輻輳の影響は「新規・変更予約の同期遅延」（reconciler のラグ）のみで、同期済みの録画には波及しない
- **実行中のエンコード・ingest も影響を受けない**。ジョブは claim / complete 時に小さなインデックス付きトランザクション（SKIP LOCKED）で DB を触るだけで、実行中（ffmpeg・転送）は DB に依存しない。輻輳の影響は「新規ジョブの着手遅延」のみ。着手が遅れた分はエッジのリングバッファが吸収する（サイジング指針は [メディアストレージ](../storage.md) 参照）
- **ルール評価は UI と同期しない**。ルール編集 API は編集を書いて再評価ジョブを投入するだけで即応答し、評価は ruler がバックグラウンドで実行。ユーザーが連打してもキューで直列化され、DB を占有する形にならない

### 飢餓防止の実装規律

実装済み。詳細と数値の根拠は [operations.md](../operations.md) §3「輻輳時の隔離」を参照:

- ロール別にコネクションプール上限を分ける（api が全コネクションを食い潰して worker/reconciler が待つ事態を防ぐ）。
  プロセスが持つプールは常に 1 個なので、実体は「そのプロセスが担う roles からその 1 個の `MaxConns` を決める」こと
- API 系クエリに `statement_timeout` を設定（api ロールを含むプロセスのプールにだけ適用）

### monolith モードの注意

実際に競合しやすいのはロックよりディスク I/O。Postgres のデータディレクトリとエンコードの scratch は同じディスクに置かない構成を推奨（インストールドキュメント事項）。

## 経緯と失敗事例

- notifier ロールの分離（api から LISTEN セッションを外し、SSE 配送を独立プロセスにする）の設計経緯は issue #24（M2-19）と issue #25 §4
- watcher の常駐/ジョブ分割（真実の定期突き合わせを `record_sweep` ジョブに切り出し、常駐に SSE 購読とヒント処理だけを残した）は M2-16 / M2-18。詳細は [録画エンジン](../recording.md) §3.3
- 「River のジョブ一意性の注意」の `ByQueue` の罠は、キューを site 修飾にリネームしたデプロイで旧キューの残骸が新キューへの Insert を黙って塞いだ実事故から（issue #185 M4-13）
- 飢餓防止（ロール別プール上限・`statement_timeout`）の実装は issue #90
