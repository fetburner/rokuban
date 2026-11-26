> [operations.md](../operations.md) §5「k8s 運用」の一部。索引から辿る。

### healthz と readyz: liveness は依存を見ない、readiness は DB を見る

liveness probe は依存サービスを見ず、readiness probe は DB への ping だけを見る。

`/healthz` は liveness probe 専用である。依存サービス（DB・mirakc）の状態は一切チェックせず、プロセスが HTTP を返せる限り常に 200 を返す。

- api ロールから mirakc をチェックすることは構造的にできない
- ハイブリッド構成（[overview.md](../overview.md)）ではクラウド側 api から mirakc に到達できないのが正常状態
- liveness に依存チェックを入れると「依存ダウン → 全プロセス再起動ループ」になる（liveness probe の定番アンチパターン）
- DB は起動時に fail-fast で検証済み。ランタイムの DB 断は各ロールがリトライ / クラッシュで対処する（crash-only 原則）

mirakc の健全性は watcher が `observed_at` として DB に記録し、UI / アラートで可視化する。

readiness は `/readyz` に分けてあり、こちらは DB への ping を見る。api を Service の後ろに置く以上、DB に繋がっていない Pod にトラフィックが振られる窓が実際に開くためである。liveness と readiness で見るものが逆になるのは、問いが違うからである。liveness は「このプロセスは再起動すべきか」、readiness は「今トラフィックを受けられるか」を問う。

- 見るのは DB への ping だけ。mirakc への到達性は見ない（ハイブリッド構成ではクラウド側 api から mirakc に到達できないのが正常状態）
- プールが無ければ 503（fail-closed）。実バイナリではプールは常にある（起動時に fail-fast する）ので、これはルータの組み立てを誤ったときに 200 で隠さないための保険
- ping はプールからコネクションを 1 本取る。したがってプールが飽和している間もタイムアウトして 503 になりうる（未測定。コネクション取得が待つことからの結果）。**この応答から DB 断と輻輳は区別できない**
- 輻輳で 503 が続くと全レプリカが同時に Endpoints から抜ける（同じ DB を見ているのでレプリカを増やしても同時に落ちる）。probe の `failureThreshold` を上げて猶予を作ってはいるが、これは遅らせるだけである。この形の全断が起きるなら閾値でなく DB 側（プール上限・`statement_timeout`。[§3](database.md)）を見る
- ハンドラ側にも応答の上限を持つ（probe の `timeoutSeconds` はそれより長くする。同値以下だと kubelet が先に諦めるので、ハンドラが 503 を返す経路が一度も通らない）
- Host allowlist の免除対象に入れる。免除し忘れると readiness が 400 で落ち、Pod は永久に Service の後ろに入らない

<details><summary>根拠</summary>

- api が mirakc に問い合わせない: 不変条件 1
- ping がプールからコネクションを取る: `pgxpool.Pool.Ping` / `Acquire`

</details>

### DB 接続失敗: fail-fast + 明示ログ

DB 接続失敗はエラーを握り潰さず fail-fast + 明示ログとする（EPGStation#628 の教訓、crash-only 方針と整合）。

### ネットワーク FS ハング: ジョブストール検知 + 外部 liveness

ネットワーク FS のハング（EPGStation#721）は worker ジョブがストールしうる。対策は次の 2 つである。

- ジョブのストール検知: ingest のタイムアウトは総時間でなく**ストール検知**（N 秒間無進捗で切断扱い）。総時間タイムアウトは遅い回線の正常な転送を殺す
- 外部 liveness: k8s の liveness probe / systemd watchdog を推奨構成に含める

### Redis の喪失と切断（asynq 構成）

asynq 構成は Redis の中身を失ってよい。投入は commit 後のヒントで、失ったヒントは定期の reconcile が DB から拾い直す。Redis の冗長化と永続化はしない。回復しない経路が見つかったら、Redis を守る方向ではなく、reconcile が拾う方向で直す。

ここに書く値は、断りが無ければ 2026-11-26 に手元で測った値である。kind の e2e-asynq（Colima 4 CPU / 8 GB、ScaledJob の `pollingInterval` は base と同じ 30 秒）と、常駐 worker を compose で立てた構成で測った。lease や回収の巡回の間隔は asynq の公開設定ではない。asynq の版を上げたら測り直す。手順は [runbook/k8s.md](../runbook/k8s.md) §asynq の回復を測り直す にある。

#### 全消去からの回復

全消去で失った仕事は、その種別を拾う reconcile の次の周期で戻る。かかる時間は、reconcile の CronJob の周期に、reconcile の Job と子の仕事の Job が起きるまでの時間を足したものになる。

- CronJob の周期は schedule で決まる。値は `deploy/k8s/` のマニフェストを引く
- Job が起きるまでの実測: 全消去の後に reconcile を手で 1 本積むと、子の仕事は 29〜35 秒以内に走り出した。encode・thumbnail・seek_tiles・ts_scan・cm_detect・ingest の 6 種別で 1 回ずつ測った。全消去の後、reconcile を積むまでの 120 秒は何も進まなかったので、拾ったのは reconcile である
- 実行中の Work は全消去で止まらない。encode の実行中に全消去すると、Work は同じ試行のまま完走した（1 回）
- 全消去で合流のロックも消えるので、完走より前に reconcile が走ると同じ仕事が重ねて積まれる。encode の枠が 1 つの構成では、重複は完走の後に走り、試行を作らずに終わった。並行して走れる構成で、重複が試行を 1 つ使うかは未検証である
- cm_logo_candidate と storage_transfer は未検証である。e2e-asynq ではこの 2 種別を積めない（ロゴ領域の設定と、置き場 2 つが要る）

未解決: 全消去の後、常駐プロセス（api・watcher・notifier）が積んだヒントを、notifier の件数の口が数えない。同じキューへ新しいプロセス（CronJob の `rokuban enqueue` や ScaledJob の Job）が積むまで、ScaledJob の Job が起きない。kind では、api のヒントが 4 件ある間 75 秒以上、件数は 0 で Job も 0 件だった。新しいプロセスが 1 件積むと件数が戻り、Job が起きた。新しいプロセスはどのキューにも CronJob の周期のうちに積むので、遅れはその周期にとどまる見込みである（未検証）。常駐 worker の構成では処理は止まらない。worker はキューから直接取るので、件数の口が数えなくても仕事は進む（compose で確認）。

#### プロセス死からの再配送

実行中にプロセスが死んだタスクは、lease が切れてから 30 秒たった後に、回収の巡回で再配送される。測ったどの回も、adapter が見込む回収の猶予（2 分 10 秒）に収まった。

| 構成 | 回収する主体 | kill から再実行の開始まで |
|---|---|---|
| ScaledJob（encode の `maxReplicaCount` 1） | KEDA が起こした Job の起動直後の巡回 | 59.5〜116.7 秒（10 回） |
| 常駐 worker 2 台（1 台を kill -9） | 生き残った worker の 1 分ごとの巡回 | 80.3〜103.0 秒（5 回） |
| 常駐 worker 1 台（kill -9 してすぐ再起動） | 再起動した worker の巡回 | 62.0〜62.2 秒（3 回） |

- lease は kill から 28.6〜31.8 秒で切れた（ScaledJob の 10 回）
- ScaledJob では、孤児になってから 30 秒以内に起動した Job は回収できず、空振りで終わる。KEDA の次の巡回で起きた Job が回収するので、分布は約 60 秒と 96〜117 秒の 2 つに分かれた
- 導出上の上限は、lease 30 秒・回収の猶予 30 秒・巡回か KEDA の間隔を足した約 2 分である。測っていない位相で 2 分 10 秒を超えるかは未検証である
- プロセス死はドメインの試行を 1 つ数える（再実行の `attempt_count` は 2）

#### worker と Redis の切断

worker と Redis の間が 20 秒余り切れると、実行中の Work は cancel され、最初からやり直しになりうる。cancel はドメインの試行を数えない。

| 切断の長さ | cancel | 戻してから再実行の開始まで |
|---|---|---|
| 15 秒・20 秒・28 秒 | なし | — |
| 25 秒 | あり | 81.6 秒 |
| 32 秒 | あり | 73.8 秒 |
| 45 秒 | あり（2 回） | 60.8 秒（もう 1 回は別の encode が枠を取っていて測れていない） |

- 条件: compose の常駐 worker 1 台（CPU 1 個）で encode を走らせた。worker のコンテナの netns で、Redis との往復だけを iptables で落とした。応答が返らず接続がタイムアウトする形で、VPN の断に近い
- cancel の境目は 30 秒ではない。lease の延長は 5 秒ごとなので、切断が始まった時点の lease の残りは 25〜30 秒である。戻した後も、延長が通るまで数秒かかる。25 秒の切断では、戻してから 1 秒足らずで lease の期限が来て、延長が間に合わなかった。28 秒の切断では間に合った
- cancel された 4 回とも、試行の行は元に戻った（数えられなかった）。再実行を観測した 3 回は、試行回数が 1 のままだった。再実行は、その worker 自身の回収の巡回（1 分ごと）が拾った
- DB との間も同時に切れたとき（VPN の全断）は未検証である

**worker を自宅に残すなら、Redis も worker と同じ側に置く。** Redis だけをクラウドに置くと、VPN が 20 秒余り切れるだけで、実行中の encode が最初からやり直しになりうる。

#### 件数の口が止まると Job が起きない

ScaledJob の構成では、件数を出す notifier が止まると、滞留があっても Job は起きない。notifier の常駐 worker が引く ruler のヒントも、同じ間は止まる。

asynq 構成で notifier を止め、cleanup に 1 件積んでから戻すまでの 98 秒の間、Job は 0 件だった。notifier が Ready に戻ってから 21 秒後（次の巡回）に Job が起きた。

#### 空振りの頻度

ScaledJob の空振りは、孤児の回収待ちと、常駐 worker との取り合いで起きた。Snooze では起きなかった。

空振りは、1 件も取れずに `idle_timeout` で終わる Job である。kind で約 98 分、ScaledJob が起こした Job を数えた。

| 原因 | 空振り | 条件 |
|---|---|---|
| 孤児の回収待ち | encode で 8 件 | プロセス死 10 回。1 回あたり 0 件か 1 件 |
| Snooze | 0 件 | ingest の SHA-256 待ちの Snooze（30 秒）5 回。ingest の Job 11 件はすべて仕事を取った |
| 常駐 worker との取り合い | ruler 43 件 / 56 件、reconciler 7 件 / 10 件 | notifier と watcher の常駐 worker が同じキューを引く。e2e は epg-sync を毎分にしている |

- 期限前の Snooze を件数に入れないので、Snooze 中に Job は起きない。期限が来たタスクは、起きた Job が待機列へ戻して取った（5 回とも空振りなし）
- 取り合いの空振りは、KEDA が数えた直後に常駐 worker が先に取ったと推定している。1 秒ごとのサンプルで待機中のタスクを捉えたのは 50 件中 9 件で、残りはサンプルの間に消化されていた。本番の頻度は未検証である

<details><summary>根拠</summary>

- 回収の猶予: `asynqadapter.RecoveryGrace`
- lease の延長と回収の巡回（起動直後と 1 分ごと、lease 切れから 30 秒後に回収）: asynq v0.26.0 の `recoverer.go` と `heartbeat.go`。ソースで読んだもので、契約ではない
- 実行中の行を encode の候補から外さない: 関数 `encode_attempts_exhausted` の COMMENT（`internal/db/schema.sql`）
- 件数の口が名前の登録に頼る: `CountDeliverable`（`internal/jobq/asynqadapter/deliverable.go`）。asynq v0.26.0 の `RDB.Enqueue` はキュー名の登録をプロセスごとに 1 回だけ行う

</details>
