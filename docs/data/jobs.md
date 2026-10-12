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

- ジョブ引数の型と投入・実行の契約は、両側が共有する。
- API と watcher はドメイン書き込みと同じトランザクションでジョブを投入する。CLI は単発で投入し、合流の有無を表示する。
- River 固有の client 変換は adapter に閉じ込める。worker 登録だけは River の公開 API が具体型を要求するため adapter を通し、他の adapter は共通の registry を使う。
- 本番コードから River adapter を直接 import することを禁止する。worker 登録とテスト DB 作成だけは例外にする。
- 構成の根は adapter を組み立てるため、import 境界の対象外にする。
- テストは River の公開 API で実挙動を確認する。KEDA の SQL テストも River の状態名を照合する。

契約は River と asynq の共通部分に絞る。queue・timeout・retry・enqueue・work・runner lifecycle だけを扱い、排他、一覧、job ID / 試行番号、定期投入、`--once`、購読は adapter 固有のまま外に置く。adapter の内部状態へ依存しない境界を先に作るためである。

- ワーカーは `FOR UPDATE SKIP LOCKED` で 1 件確保。複数ワーカーが同時に来ても行ロックで排他され、同一ジョブの二重実行はトランザクション分離の性質として起きない
- `Spec.Timeout` は必須の有限値で、締切で Work の context は `DeadlineExceeded` になる。実行中にプロセスが死んだ場合も、締切 + adapter の復旧猶予以内に再配送対象となる。古い Work が止まったことや同じ job ID であることは保証しない。`Spec.MaxAttempts` は通常エラー・締切超過・プロセス死の失敗回数で止める保険で、0 は adapter の既定、Snooze は回数を消費しない
- at-least-once なのでジョブは冪等に書く（出力は一時パスに書いて完了時に公開、DB 登録は `ON CONFLICT` で吸収）
- 常駐シングルトンロール（watcher）は `pg_advisory_lock` によるリーダー選出。セッション断で自動解放されるのでフェイルオーバーも自然に付く。k8s の Lease API に依存しないため monolithic mode でも同じコードが動く
  - **watcher のシングルトン性は「正しさ」の要件ではない**。record 処理は行ロックで冪等化されており、複数の watcher が同一 record を並行処理しても `recordings` は重複しない。シングルトンなのは「mirakc に N 本の SSE を張らない」という接続数の配慮に過ぎない。詳細は [録画エンジン](../recording.md) §3.3
- **ルール評価（ruler）と reconciler はシングルトンではなくジョブ**。定期・冪等・パスを跨ぐ状態を持たないという性質が epg_sync と同じである。現行 River runtime は `UniqueOpts` で同じ site の未完了ジョブを抑止する。これは River adapter の機能で、共通契約の排他保証ではない。両方の timeout は有限で、プロセス死後の再配送は adapter が担う。セッションロックへ移す利益は現在の River runtime にはない。未解決: River 以外の adapter には ruler の並走（古い評価結果による上書き）を止める機構が無い。
- **現行 River runtime では `UniqueOpts` が通常運用の重複抑止にも使われる**。worker の `MaxWorkers: 1` はプロセスごとの値で、k8s の ScaledJob では Pod が並列に立つ。定期実行とヒントの重なりを River 内で抑止しているが、この挙動を共通契約へ持ち込まない。River の `UniqueOpts` も行の重複を抑える機能であり、rescue 後に旧 Work が止まらなければ 2 実行が並走する
  - 並走すると壊れるもの（コード読解による。並走を起こしての確認はしていない）
    - ruler: パスの読み（`runPassForSite` の前半）と書き（後半のトランザクション）が別トランザクションなので、古いルール集合で評価したパスが後からコミットすると新しいパスの結果を上書きする。`label_rule_reconcile` が advisory xact lock で止めている形と同じである。これで投資の無い予約が消えて直前の番組を録り逃すかは未検証
    - reconciler: パス冒頭の一覧に基づく mirakc への DELETE / POST が、別のパスの適用と食い違う
- **共通契約の `Coalesce` と `Priority` はヒントである**。`Coalesce` は「合流してよい」という意思表示に限り、1 行にすることも並走を止めることも保証しない。`Priority` も順序を保証しない。正しさはこれらに依存させない。現行 River 投影では `Coalesce` を River の `UniqueOpts` に写し、重複 enqueue を減らす
- エンコード待機列はドメイン表から導出する。「不足プロファイル」を実行中の試行行の有無で `running` と `queued` に分ける。
  試行回数が上限に達した失敗行は worker が開始時に cancel して作られないので、どちらにも数えない。
  `GET /api/recordings` の `encodeState` 絞り込みも同じ行から録画 ID を取る。River の job 一覧は使わない。
- worker が encode 中に死ぬと、次の試行が始まるまで試行行は `running` のまま残る。River の rescue は締切後（実尺が不明なら最大 12 時間）なので、その間は `running` に数える。
- desired から外れた後も River に残るジョブは、待機列に数えない。ジョブエンジンの状態より、現在の desired と observed の差を「これから作るもの」の根拠にするためである

<details><summary>根拠</summary>

- ジョブ引数と共通契約: `internal/jobs`, `internal/jobq` の `Kind` / `Spec`
- API / watcher のトランザクション投入と CLI 投入: `EnqueueTx`, `Enqueue`, `Result.Coalesced`
- River adapter と登録・registry: `internal/jobq/riveradapter`, `InsertOpts`, `jobq.Registry`
- import 境界: `.golangci.yml` の depguard は `internal/` の本番コードを対象にする。例外は `internal/worker/worker.go` と `internal/testutil`。構成の根は `cmd/`
- 待機列の導出: `internal/api/encode_queue.go`, `ListMissingEncodeProfiles`; API の絞り込みは `GET /api/recordings` の `encodeState`
- River の job 一覧を使わないこと: `JobList`
- 待機列の分割・上限・observed の除外・cut 所有条件: `TestGetEncodeQueueSummaryFromDomainState`
- encodeState 絞り込みと待機列の一致: `TestListRecordingsEncodeStateMatchesQueueCounts`

</details>

### 定期実行の契機はデプロイ形態に委ねる

River の `PeriodicJobs` は**リーダーに選出されたクライアントだけが投入する**。k8s の worker は KEDA で 0〜N に伸縮する短命な client なので、定期投入の担い手に向かず、スケールアップのたびに `RunOnStart` が走るのも望ましくない。1 件消化モード（`--once`）は `periodic_jobs: true` だと起動を拒む。

そこで定期実行の契機をアプリの外に出せるようにする。

| デプロイ形態 | 定期投入の担い手 |
|---|---|
| Docker（monolith） | プロセス内の River `PeriodicJobs`（既定で有効） |
| k8s | **CronJob が `rokuban enqueue <job>` を叩く**（insert-only クライアントで 1 件投入して即終了） |

`PeriodicJobs` の登録は設定で切れるようにし、k8s では無効にする（両方有効だと二重投入になる。`UniqueOpts` で合流するので害は小さいが意図が曖昧になる）。副次的な利点として、k8s では**何がいつ走るかが CronJob の spec に一元化される**（Go のコードと YAML に散らない）。

**k8s の JobRescuer / JobScheduler は notifier Deployment の常駐 River client が担う。**
client は全 kind を登録する。購読するのは site 非依存の `ruler` queue だけである。
Ruler は DB 完結なので中央で実行できる。
`worker.periodic_jobs: false` は維持するため、定期ジョブの投入元は引き続き CronJob である。
載せ先と queue の判断は [k8s 運用](../operations/k8s.md) §5 にある。

### River のジョブ一意性の注意

現行の River 投影では `UniqueOpts.ByState` を**必ず明示する**。River の既定
（`UniqueOptsByStateDefault`）は
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

River projection は `UniqueOpts.ByQueue` を使い、現在は `Coalesce: true` の全ジョブで
キューを一意キーへ含める。`ByArgs` と `ByQueue` は別軸なので、片方だけでは意図した
identity にならない。**キュー名を変える（リネーム・site 修飾の追加）とき、Queue を
一意キーに含めていないと旧キューの残骸が新キューへの Insert を黙って塞ぐ**。この結果は
`UniqueSkippedAsDuplicate` で、エラーを返さないのでログにも出ない。トラブルシュート手順は
[runbook/troubleshooting.md](../runbook/troubleshooting.md) 「デプロイ直後、旧キューの
残骸が `river_job` に残っている」を参照。

合流の intended identity は **kind + 論理キュー + Args 全体の JSON** とする。River の部分フィールド選択を `river:"unique"` タグで指定すると adapter 固有になるため、タグは使わない。Args 全体なら adapter をまたいで同じ identity を再現できる。

導出値を Args に含めると、同じ仕事でも値の違いで別 identity になる。起こりうる条件と結果をまとめる。

| 条件 | 重複と結果 |
|---|---|
| deploy 直後 | 待ち行列と実行中の encode が 1 回ずつ重複しうる |
| 運用者が profile の rate を変更 | 同じ重複が起きる |
| 同時実行数が 2 以上、または複数 Pod | 重複側が試行番号を進め、先行 encode の公開が拒否されて ffmpeg の結果を捨てる |
| 既定の同時実行数 1 | 重複は後から実行され、公開判定で飛ばされる |

いずれも正しさは壊れない。

<details><summary>根拠</summary>

- 重複側が試行番号を進めて先行結果を拒否する処理: `beginEncodeAttempt`

</details>

### Redis バックエンド（Sidekiq 系）を採用しない理由

1. **dual-write 問題**: 「録画完了を DB に登録」と「エンコードジョブを積む」は常にセットである。だが書き込み先が DB と Redis に分かれると、コミット前 enqueue（ワーカーが未コミットデータを読む race）か、コミット後 enqueue（隙間のクラッシュでジョブ消失）かの二択になる。真面目に解決すると outbox パターン = 結局 DB 上のキューを作ることになる
2. **ステートフル基盤が増える**: ミニ PC にはコンテナ 1 つ、クラウドにはマネージド Redis が 1 つ増え、バックアップ・監視・障害モードが 2 系統になる
3. **スループットが要らない**: このシステムの負荷は「1 日数十件、1 件数十分」のジョブ。キューに必要なのはスループットではなく耐久性と可視性（再起動で消えない・SQL で覗ける・ドメイン更新と同一トランザクション）で、これは RDB の得意領域
4. 傍証: Rails 8 が Solid Queue（DB バックエンド）をデフォルトに変えた。中規模まででは Redis を挟む価値が薄いという業界側の再評価

なお DB キューの一般的な弱点（数千 jobs/sec で vacuum・ロック競合が苦しい、コネクション数がワーカー数に比例）はこのワークロードでは遠く及ばない。

## 3. イベント通知: LISTEN/NOTIFY

NOTIFY はコミット時にのみ配送される組み込み pub/sub。各コンポーネントは NOTIFY を「何か変わった」のヒントとして受け、**実データは必ずテーブルから読み直す**（レベルトリガー）。通知の取りこぼしは定期 reconcile で収束するため、配送保証に依存しない。

**狭い例外は、テーブルに載せない揮発テレメトリ（エンコード進捗）だけ**。ffmpeg の
`out_time` を実入力 duration に対する割合にして、最大 1 回/秒で NOTIFY する。notifier の
EventHub が接続中のブラウザへ SSE として渡す。**テーブルへ保存しない**のは、秒単位で
変わる復元不要の値を永続化すると再起動後に WAL・dead tuple・vacuum 対象を増やすため。
**完了判定には使わない**。NOTIFY に配送保証が無く、切断・遅参・notifier 停止で欠落しうる
からである。欠落しても真実は `recording_encode_attempts` / `media_assets` の再読で
復元できなければならない（不変条件 5）。

フロントエンド（[フロントエンドアーキテクチャ](../frontend.md) 参照）の SSE も同じ構造:
durable な状態のイベントは TanStack Query の `invalidateQueries` に徹し、真実は常に REST
から再取得する。エンコード進捗だけは上記の揮発テレメトリとして payload を直接表示し、
durable 状態が `running` でなくなれば破棄する。

**ブラウザへの SSE 配送 (`/api/events`) の担い手は notifier ロール**（api ではない）。api ロールは mirakc にもファイルシステムにも依存しない（不変条件 1）。同じ理由で、長寿命接続である LISTEN セッションも持たない --- サーバーレス（scale-to-zero）に載せるためには、そもそも常駐前提の LISTEN を api に置けない。notifier は「Postgres を LISTEN してブラウザへ配り直すだけ」の小さな常駐プロセスとして分離してある。

notifier は**シングルトンではない**（`cmd/rokuban/server.go` の `singletonRoles` に含まれない）。各レプリカが独立に LISTEN し、自分に繋がっている SSE クライアントにだけ配る。レプリカ間で配送内容を調停する必要が構造的にない（各クライアントは 1 つのレプリカにしか繋がっておらず、そのレプリカの LISTEN だけが届けば十分）ため、レプリカを増やしても Redis アダプタのような追加の配送基盤は不要。この点は、reconciler/ruler のジョブ化によりシングルトンが watcher だけになったのとは別の理由づけである点に注意。あちらは「冪等だから複数動いても壊れない」、notifier は「配送先が自分にぶら下がる接続だけなので調停自体が要らない」。

**もう一つの長寿命接続（watcher が mirakc から受ける SSE 購読）とは意図的に分離したまま**。「長寿命接続を持ち続ける」という機構は同じでも、相手も向きも障害時の影響範囲も無関係なので、1 つの抽象/プロセスにまとめない。詳細は [api.md](../api.md) の「2 つの SSE を 1 つに集約しない」を参照。

## 7. DB 輻輳時の隔離

「ユーザー操作で DB が詰まったら録画やエンコードに影響しないか」という懸念への整理。DB 輻輳から分離できる処理はあるが、**故障モードを常に「収束の遅れ」とは一般化しない**。保証は次の条件付きである:

- **録画は、mirakc に番組終了前まで同期済みの予約に限って DB 停止から分離される**。スケジュールは mirakc 側の `schedules.json` に永続化済みで、録画実行は mirakc が自律的に行う。ただし mirakc 自身、録画バッファ、チューナーが動作していることが条件であり、新規・変更予約は reconciler が期限内に同期できなければ録画されない
- **実行中の ingest は、転送中のバイト I/O だけを見れば DB の外側にあるが、ジョブ全体は DB に依存する**。開始時の `record_sync` 参照、進捗の書き込み、公開点である `media_assets` コミットが必要である。有限 slice の区切りでは checkpoint を保存し、同じ River job を snooze する。プロセス死では JobRescuer が retry を予約し、temp の flock と checkpoint を使って再開する。DB 障害で接続やコミットを失えば、録画バッファに record が残り、再試行できる範囲では収束する（詳細は [ingest](../recording/ingest.md) §5.3）
- **実行中の encode は ffmpeg のバイト処理中に DB の外で動き、公開は `media_assets` のコミットで決まる。**
  - 投入時に録画実尺と profile rate から締切を計算し、args に保存する。締切は `max(1 時間, 実尺 × rate)`。実尺は録画時刻を使い、取れなければ番組長を使う。
  - 保存済み締切を timeout として返す。実尺不明または締切が無い旧 args は 12 時間とし、誤った締切超過で失敗を積むより死亡検知が遅れる方を選ぶ。
  - プロセス死のあと、River の JobRescuer が締切後に再試行を予約する。
  - `recording_encode_attempts.attempt_count` はドメイン試行回数と公開時の fencing token を兼ねる。遅れて戻った旧試行の公開を拒否する。
  - Worker は River の job ID / attempt を scratch の識別に使わない。scratch は `(recording_id, profile, attempt_count)` で分け、掃除対象は `(recording_id, profile)` に限る。
  - 各試行は自分の scratch directory の flock を保持する。掃除は lock を取れた（死んだ）directory だけを消し、他 scope の encode には影響しない。並走中の試行は残す。
  - 停止による `Canceled` と snooze はドメイン試行に数えず、締切超過は数える。上限 25 に達したら `river.JobCancel` で River 上も cancelled にする。
  - `POST /api/recordings/{id}/encode-profiles` は failed 行を消して予算を戻す。停止による `Canceled` は River の attempt を消費するため、`MaxAttempts` 26 でも River の discard を防ぐ保証はない。その場合は reconcile が新しいジョブで回復する。
- **CM 検出とロゴ候補解析は、録画の実尺を args に入れて `Timeout()` を決める。**
  - 実尺は `recordings.ended_at - started_at`。どちらかが NULL（EPGStation 取り込み等）なら `program_duration_ms` を使う。
  - scratch は job ID / attempt ではなく、CM 検出は `(recording_id, attempt_count)`、ロゴ候補解析は `(recording_id, attempted_at)` で識別する。
  - 締切は実尺の 2 倍、最低 30 分。rescue までの待ち時間は通常の規則に従い、1 時間と締切の長い方とする。
- **CM 検出の試行回数は `recording_cm_attempts.attempt_count` で数える。**
  - 失敗・締切超過・プロセス死は試行として数える。プロセス死は次の開始時に running 行を進める。
  - 停止時のキャンセルは数えない。失敗が無い初回なら行を消し、失敗後なら回数を戻して `retrying` にする。
  - `failed` 後に新しいロゴや枠で desired になれば、予算を 1 から始める。結果保存は開始時の回数を照合し、古い試行を拒否する。
- **`attempted_at` から締切と rescue 猶予を過ぎたロゴ候補を failed にする。**
  - 締切は実行側と共通の関数で求める。録画実尺を引き、録画行が無ければ最小締切で回収する。
  - `MaxAttempts: 1` のため、失敗後に再解析するには利用者がロゴ領域を保存し直す。停止時のキャンセルは候補行を消して再 reconcile に任せる。設定値 0 は River の既定 1 時間として扱う。
- **長時間の滞留はポリシーを失うことがある**。ingest が `epg.retention_grace` を跨ぐと、予約から encode policy を解決できず既定値で凍結され、作成時点で予約も意図も無ければ `source` は `unattributed` になる。原本の保持・エンコードの扱い、回線断を含む滞留の測り方は [ストレージ運用](../operations.md) §4 と [ストレージ](../storage.md) §6 を参照する
- **ルール評価は UI と同期しない**。ルール編集 API は編集を書いて再評価ジョブを投入するだけで即応答し、評価は ruler がバックグラウンドで実行。ユーザーが連打してもキューで直列化され、DB を占有する形にならない

<details><summary>根拠</summary>

- Encode timeout と rescue 猶予: `Timeout()`, `max(1h, Timeout())`。試行の結果 fencing: `recording_encode_attempts.attempt_count`
- CM timeout と実尺の参照: `cmDetectionTimeout`, `GetCMRecordingDuration`
- encode scratch の scope: `TestRemoveStaleEncodeScratchScopesByRecordingAndProfile`
- 他 scope の実行中 encode の保護: `TestRemoveStaleEncodeScratchIgnoresLiveEncodeOfOtherScope`
- 並走中の scratch の保護: `TestEncodeWorker_ConcurrentScratchCleanupSkipsActiveAttempts`
- CM 実尺の取得元（`started_at` / `ended_at`）: `TestCMRecordingDurationQueriesUseStartedAndEndedAt`
- CM 締切式: `TestCMDetectionTimeoutUsesTwiceDurationWithThirtyMinuteMinimum`
- CM キャンセルと試行回数: `TestCMDetectDeadlineCountsAndCancellationDoesNotCount`
- 新しい CM 検出予算: `TestCMDetectionNewDesireAfterFailedStartsNewBudget`
- orphan ロゴ候補の締切・猶予: `TestFailOrphanCMLogoCandidatesWaitsForDeadlineAndGrace`
- ロゴ候補キャンセル: `TestCMLogoCandidateWorkerCancellationClearsRunningRow`

</details>

### 飢餓防止の実装規律

実装済み。詳細と数値の根拠は [operations.md](../operations.md) §3「輻輳時の隔離」を参照:

- ロール別にコネクションプール上限を分ける（api が全コネクションを食い潰して worker/reconciler が待つ事態を防ぐ）。
  プロセスが持つプールは常に 1 個なので、実体は「そのプロセスが担う roles からその 1 個の `MaxConns` を決める」こと
- API 系クエリに `statement_timeout` を設定（api ロールを含むプロセスのプールにだけ適用）

### monolith モードの注意

実際に競合しやすいのはロックよりディスク I/O。Postgres のデータディレクトリとエンコードの scratch は同じディスクに置かない構成を推奨（インストールドキュメント事項）。
