# ロール分割デプロイの受け入れ判定ハーネス（kind + KEDA）

**k8s デプロイには `pnpm test` に相当するものが無い。** kind を立てるまで何も
分からない。ここはその「立てて機械判定する」側を持つ。

実行手順は [docs/runbook/k8s.md](../../../docs/runbook/k8s.md)
§受け入れ判定ハーネス。**CI では回さない**（同節に理由と「いつ誰が回すか」）。

```sh
./deploy/k8s/e2e/run.sh                      # 5 項目を判定する
./deploy/k8s/e2e/run.sh --only 2,4           # 一部だけ（0 は返さない）
./deploy/k8s/e2e/run.sh --oracles            # 判定そのものを検査する（変異注入）
E2E_ORACLES_ONLY=3 ./deploy/k8s/e2e/run.sh --oracles   # オラクルも一部だけ
./deploy/k8s/e2e/run.sh --down               # クラスタを消す
```

## 出力は 3 値である

|  | 意味 |
|---|---|
| `PASS` | 判定が走り、期待どおりだった |
| `FAIL` | 判定が走り、期待と違った（**壊れている**） |
| `TODO` | 判定の**対象が無い**ので走らせていない（**まだ実装されていない**） |

終了コードは `0`（全部 PASS）/ `1`（FAIL あり）/ `2`（FAIL なし・TODO あり）。

**2 値にしない。** 「まだ作っていない」と「作ったが壊れている」を同じ赤に
すると、残りのワークロードを足していく途中で何が退行したのか出力から分から
なくなる。`TODO` のメッセージには**何が見つからなかったか**を書く（「未実装」
とだけ書かない）。

**`2` を `0` に丸めない。** TODO が 1 つでも残っている限りこのハーネスは成功を
返してはならない。同じ理由で、`--only` で一部だけ回したときも 0 は返さない。

**ただし `0` は「受け入れ 5 項目を判定できた」であって「ワークロードが揃った」
ではない。** `0` が保証**しない**もの:

- **CronJob は `epg-sync --site <A>` の 1 本しか見ていない。** `rokuban enqueue`
  の対象は複数ある（一覧は `rokuban enqueue --help`）。
  `worker.periodic_jobs: false` の下では全部が CronJob 側に移るので、
  1 本を除いて全部欠けていても 0 になる
- **ScaledJob は `epg_<site>` / `reconciler_<site>` / `encode` しか見ていない。**
  `ingest_<site>` / `watcher_<site>` が欠けていても 0 になる
- **Deployment は役ごとの存在と「宣言した数だけ Ready」だけ。** サイトごとの
  網羅は見ていない（site B の watcher が無くても判定 1 は緑）
- **役ごとの到達性は見ていない。** Service も Ingress も無い notifier でも、
  Deployment が Ready なら判定 1.2 は緑になる。`/api/events` が notifier に
  届くかは誰も測っていない
- **定期パスの網羅も見ていない。** `worker.periodic_jobs: false` の下では
  in-process の定期ジョブ 9 種が CronJob 側に移るが、判定が見るのは
  `epg-sync` の 1 本だけ
- **判定 2 が測るのは出荷される schedule ではない。** base の epg-sync は
  実運用の 10 分間隔だが、判定 2.3 は 180 秒以内の自然発火を要求するので、
  `overlays/e2e` が毎分に patch している。**出荷値のほうは
  `deploy/k8s/workloads_test.go` が固定している**（`TestCronSchedulesAreProductionValues`
  と `TestE2EOverlayShortensTheCronScheduleItMeasures` が対）--- 判定だけを見て
  いると、「判定が緑になるから」で base が毎分になったことに気付けない
- **ライブ視聴の streamer は見ていない**（そもそも出荷していない。理由は
  deploy/k8s/README.md §まだ無いもの）。判定 1.4 が Ready を見ている
  `component=streamer` は録画配信のほうである
- **レプリカ数と `resources.requests` は出荷値ではない。** `overlays/e2e` が
  1 ノードの kind に収まるように削っている。notifier / streamer は 1 で、
  requests は worker 10m / 常駐 25m。**削らないと判定が
  `Insufficient cpu` で止まる**（実測）。ScaledJob は滞留 1 件ごとに Job を
  起こすので、CronJob が毎分投入する構成では Pod が十数個並ぶ。
  出荷値は base 側にある
- **`encode` の同時実行本数は出荷値ではない。** base は 2（`encode_reconcile` /
  `encode_enqueue_hint` が同じキューに載るので、長いエンコードの裏で詰まらせ
  ないため）だが、`overlays/e2e` は 1 に絞っている --- 判定 3 は active な Job の
  1 つ目を追いかけるので、2 本目が起きると**短い方を観測して**「窓の中で
  完走した」= TODO に化ける
- **RWX は検証していない。** media の PVC は base では `ReadWriteMany` だが、
  `overlays/e2e` は kind の既定 StorageClass（rancher.io/local-path）に合わせて
  `ReadWriteOnce` に落としている。判定が緑でも「RWX が効いた」証拠にはならない
  （同じノード上のディレクトリを複数の Pod が開いているだけ）

**網羅を判定に入れる代わりに、`go test ./deploy/k8s/` が形で見ている**
（`deploy/k8s/workloads_test.go`）。クラスタを立てずに機械判定できるのは
次の 3 つ。

- 全キューに ScaledJob があるか
- `rokuban enqueue` の全ジョブに CronJob があるか
- トリガのクエリが物理キュー名か

一覧の権威は `internal/worker` と `cmd/rokuban/enqueue.go` にある。

**このハーネスが見るのは、そこから先の「実際に動くか」だけである。**

**判定が黙って死ぬのも 0 にしない。** 各判定は自分が記録するはずの id を
`plan` で先に宣言し、宣言と記録が食い違えば集計側が FAIL を書き足す。これが
無いと、全部 PASS になった後で「判定 5 が起動直後に落ちた」が exit 0 になる。

**環境の破損を TODO にしない。** 判定の前に preflight（0.1〜0.5）が走り、
クラスタ・名前空間・KEDA の CRD・製品マニフェスト・DB とマイグレーション・
身代わりの残骸を確かめる。ここが無いと、KEDA の導入に失敗しただけで全項目が
「まだ実装されていない」と**1 文字違わない出力**になる。

## 判定する 5 項目

| | 見るもの | 対象 |
|---|---|---|
| 1 | 全ロールが上がり、番組表が見えて予約が mirakc に反映される | api / notifier / watcher / streamer の Deployment、`epg_<site>` と `reconciler_<site>` の ScaledJob |
| 2 | worker 0 でも CronJob が投入し続け、KEDA が Job を起こして消化する（0 → 1 → 0） | `epg-sync --site A` の CronJob、`epg_A` の ScaledJob |
| 3 | 実行中の encode Job がスケールインで殺されない | `encode` の ScaledJob（実物のエンコードを 1 件走らせる。下記 producer） |
| 4 | watcher を 2 レプリカにしても二重に動かない（advisory lock の実効） | site A の watcher Deployment |
| 5 | サイト B の滞留でサイト A の Job が起きない | `epg_A` / `epg_B` の ScaledJob |

この表は判定を足したり対象を変えたりする人が更新する。**実測の結果はここに
書かない** --- 環境と対にして [docs/runbook/k8s.md](../../../docs/runbook/k8s.md)
§受け入れ判定ハーネス に置いてある（両方に書くと片方だけ古くなる）。

判定 1.6 は `epg_<site>` を、判定 1.7 は `reconciler_<site>` を消化する側を
それぞれ別に探す。1 つの鍵で両方を代表させると、epg の ScaledJob だけ先に
入った状態で 1.7 が「作っていないのに壊れている」に化ける（同じ ScaledJob が
両方に一致する構成でも通る。どうまとめるかは判定側で決めない）。

判定は**名前ではなく振る舞いと argv で対象を引く**（`lib/kube.sh`）。
`app.kubernetes.io/component=<役>` のラベル（base が既に採っている規約）と、
`--sites <site>` / キュー名への言及で探す。命名の合意まで要求しないため
（不変条件 11: 形を固定する前に判定基準を書く）。**一致が複数あったら
TODO でも PASS でもなく FAIL にする** --- 黙って 1 つ目を選ぶと、無関係な
オブジェクトを判定して緑を出す。

## クラスタの中身

```
kind クラスタ rokuban-e2e / 名前空間 rokuban-e2e
├── KEDA（keda 名前空間。判定 2 / 3 / 5 が要求する。版は lib/env.sh で固定）
├── 足場（cluster/scaffold.yaml。**製品のマニフェストではない**）
│   ├── postgres        使い捨て。base は DB を出荷しない
│   ├── mirakc-sitea    mirakc モック（mirakcmock/）
│   ├── mirakc-siteb    同上。判定 5 は 1 サイトでは測れない
│   └── e2e-toolbox     判定が curl と `rokuban enqueue` を打つ場所
└── 製品（deploy/k8s/overlays/e2e = base + site 2 組）
    migration Job / ConfigMap / media の PVC
    api・notifier・streamer の Deployment + Service + PDB
    watcher の Deployment ×2（サイトごと）
    worker の ScaledJob ×13（site 非依存 5 + site 束縛 4 ×2）
    投入側の CronJob ×14（site 非依存 4 + site 束縛 5 ×2）
```

**mirakc は実機ではなくモックで確認した。** 実機はチューナー資源を要求し、
EPG が揃うまで待たされるので kind に載せられない。モックが実装しているのは
rokuban が実際に叩く経路だけで、未実装のパスは 404 ではなく 501 を返す
（「モックが持っていない」と「mirakc が持っていない」を出力で区別するため）。

モックのレスポンスは **`internal/mirakc` の型をそのまま組み立てて**返す。wire 形を
書き写すと、JSON タグのズレを止めるものが無くなる（症状は「番組表が空」で、
モックを疑うまでがいちばん遠い）。`go test ./deploy/k8s/e2e/...` は**製品の
クライアントで**モックの全エンドポイントを読み、製品が実際に見るフィールドが
埋まっていることまで見る。

モックが持つハーネス固有の機能は 2 つ。1 つは **`/events` の同時接続数を数えて
`/mock/stats` で公開する**こと。判定 4 はこの数値だけで機械判定できる。
watcher の singleton 性が主張しているのは「mirakc に N 本の SSE を張らない」
ことそのものだからである（`internal/role/leader.go` のパッケージコメント）。
もう 1 つは **`POST /mock/reset`** で、周回ごとに録画予約を空へ戻す。

**判定は前の周回の残骸を消してから測る。** クラスタは使い回す設計なので、
消さないと判定 1 が「前回の EPG と予約が見える」ことを主張するだけになる。
同じ理由で Job の観測は `--watch-only` + 開始時点のスナップショットで
「**今回新しく現れた Job**」に限定している。`--watch` は既存の Job を ADDED で
吐くので、前回の完走 Job がそのまま今回の証拠になってしまう。

`worker.periodic_jobs` は **false** で出荷している（`overlays/e2e/config.yml`）。
true のままだと、判定 2 が「worker が自分で投入して自分で消化した」でも緑に
なりうる。

## オラクルの自己検査（`--oracles`）

**「判定が存在すること」と「判定が効くこと」は違う。** `--oracles` は
判定 1〜5 のそれぞれについて

1. 正しく動く**身代わり**（`fixtures/`）を立てて判定が緑になることを見る
2. **それを 1 か所だけ壊して**判定が赤になることを見る

の 2 本立てで確かめる。**2 が無ければ 1 は何も保証しない。**

身代わりは製品のワークロードではない。判定 2 / 3 / 5 が見ているのは
「KEDA のトリガが site 修飾されたキューを見て、滞留があるときだけ Job を
起こすか」であって Job の中身ではないので、身代わりの中身は psql 1 行で足りる。
**ここに製品のマニフェストを先に書かない**（書くと、判定が「自分で書いた正解」と
突き合わせるだけになる）。判定 4 だけは本物の rokuban イメージを動かす ---
`pg_try_advisory_lock` の実効を見る判定なので、身代わりにすると製品のコードを
一度も通らない。

身代わりは製品と同じ役ラベル・同じキュー名を名乗るので、オラクル 2〜5 は
**探索を身代わりに絞って**回す（`E2E_FIXTURE_SCOPE`）。絞らないと、製品の
ワークロードが入った瞬間に両方が一致して全部 AMBIGUOUS になる。

**絞るだけでは足りない。** 絞っているのは「どのオブジェクトを判定対象にするか」
だけで、River のキューも mirakc モックも共有のままである。製品のワークロードが
入った周回で実測した壊れ方が 2 つある。

- **O3.mut-rollout が FAIL**（判定 3.3 が期待の FAIL ではなく TODO）。製品の
  `encode` の ScaledJob が、身代わりのために積んだ encode ジョブを先に掴んだ
- **O5.control が FAIL**（判定 5.3）。positive control で サイト A に積んだ
  滞留を、製品の `epg_sitea` の ScaledJob が消化した

したがって**オラクル 2〜5 の間は製品のワークロードを止める**
（`pause_product_workloads`）。ScaledJob は pause、CronJob は suspend、
watcher の Deployment は 0 レプリカにする。止めたものだけをクラスタ側の
annotation で覚えて戻すので、中断しても次の `run.sh` が起動時に戻す。

**オラクル 1 は含めない。** あちらは製品の役が上がっていることを見る
オラクルなので、止めると判定 1.3 が「replicas が 0」で落ちる。

**判定 4 は止めないと嘘の緑になる。** 判定 4 が数えるのは mirakc モックの
`/events` の同時接続数なので、製品の watcher が 1 本張っていると、身代わりが
0 本でも「1 本」に見える（身代わりが壊れていても緑）。

| 変異 | どこを壊すか | 期待 |
|---|---|---|
| `O1.mut-selector` | api Service のセレクタを外す（Pod は生きたまま Endpoints が空） | 判定 1.5 が FAIL |
| `O2.mut-trigger` | ScaledJob のトリガのクエリが常に 0 を返す | 判定 2.4 が FAIL |
| `O2.mut-cron` | 投入側の CronJob を suspend | 判定 2.1 が FAIL |
| `O3.mut-rollout` | `rollout.strategy` を `immediate` に | 判定 3.3 が FAIL |
| `O3.mut-omitted` | `rollout` ごと省略する（書き忘れ） | 判定 3.3 が FAIL |
| `O4.mut-lock` | **`pg_try_advisory_lock` の戻り値を無視したイメージ**（`mutants/ignore-advisory-lock.py`） | 判定 4.2 が FAIL |
| `O5.mut-queue` | サイト A のトリガから site 修飾を外す（`queue LIKE 'epg%'`） | 判定 5.2 が FAIL |

**上の変異はすべて実際に注入して、期待どおり判定が赤くなることを確認してある。**
実測値（件数）は環境と対にして `docs/runbook/k8s.md` §受け入れ判定ハーネス に
置いてある。ここには書かない（両方に書いたら片方だけ古くなった）。

**効くことを確かめていない判定が 2 つある。** どちらも「変異表に無い」ことが
そのまま意味なので、ここに明記しておく。

- **判定 3.2（待ち行列が空になっても殺されない）には変異が無い。** ScaledJob の
  意味論上、KEDA はキューが空になっても実行中の Job を消さない。身代わりを
  壊して赤くする方法が思い付かなかった（3.3 と 3.4 は確かめてある）
- **判定 1.6 / 1.7（番組表が見える / 予約が mirakc に反映される）には変異が
  無い。** 製品の worker が入ったので、両方とも実際に緑になることは確認済み
  （`run.sh` の実測）。それ以前は「緑になったことも赤になったこともない判定」
  だった。ただし**壊して赤くなることは確かめていない。** 判定 4 が本物の
  イメージで身代わりを立てているのと同じやり方（`fixtures/watcher.yaml`）で、
  `epg_sync` と `reconcile_pass` を消化する身代わりを足せば、ここも変異まで
  通せる

変異イメージは**リポジトリの複製**（rsync したツリー）に当てて焼く。
`git stash` は使わない --- ワークツリーは他の作業と共有されうるし、隔離
worktree と併用すると stash が互いに干渉する。変異は**コンパイルが通る形**で
入れる（未使用変数でビルドが落ちると、判定が「FAIL になった」ではなく
「イメージが無い」で赤くなる）。

## 残りのワークロードを書くときに

ScaledJob 自体の書き方（トリガの接続先・`rollout.strategy`・切る軸）は
[docs/operations.md](../../../docs/operations.md) §5「worker: KEDA ScaledJob」が
権威。ここにはハーネス側の契約と未解決の穴だけ置く。

### 製品のワークロードが満たしている前提

**ここに挙がっているものは全部入っている**（`deploy/k8s/base` と
`deploy/k8s/site`）。書き換えるときに壊してはいけない形として残す。

- **判定 2.3 は CronJob が 180 秒以内に自然発火することを要求する。**
  base の epg-sync は実運用の 10 分間隔なので、`overlays/e2e` が毎分に
  patch している。**したがって判定 2 が測るのは出荷される schedule ではない**
  （上の「0 が保証しないもの」）。出荷値のほうは
  `deploy/k8s/workloads_test.go` が固定している

- **worker の Job は `--once` で 1 件消化して終了する。** `--roles worker` は
  常駐するので、そのまま載せると Job が `succeeded` に到達せず判定 2.4 が
  永久に FAIL する。`--once` は成功・失敗を問わず exit 0 なので
  `backoffLimit: 0` / `restartPolicy: Never` と組ませる。
  `--once-idle-timeout`（既定 30 秒）は **1 件も掴めなかった場合にしか効かない**
- **キューは `--queues <名前>` で絞る**（config の `worker.queues` との両方指定は
  起動エラー）。`--once` はちょうど 1 キューを要求する。中央の encode は
  `--sites= --queues=encode`（`Dockerfile.full` のイメージ。公式イメージは
  ffmpeg 非同梱で fail-fast する）
- **`rokuban enqueue delete-reconcile` が使える。** 以前は enqueue に載って
  おらず、`worker.periodic_jobs: false` ではこのパスが一度も走らなかった

### ハーネス側の契約と、残っている穴

- **`insert_probe_job` が入れる `e2e_probe` は、実物の worker には
  「登録されていない kind」である。** 掴んだ worker は 1 回失敗させて試行回数を
  1 つ潰す（実データには触れない）。`--once` の worker はそこで終了する
  （ログの `outcome=job_unhandled`）。単体では
  `TestServerCmd_OnceModeExitsOnUnhandledJobKind` が同じ形を固定している。
  つまり **`e2e_probe` は「長時間 Job」にはならない**ので、判定 3 の
  既定 producer は実物の encode ワークロードに対しては使えない
  （`E2E_ENCODE_PRODUCER` の差し替えが要る。判定 3 の冒頭コメント）。
  **判定 5 は `discarded` では壊れない。** 5.1 はサイト B の ScaledJob を
  pause してから積むので誰も消化せず、5.3 は待ち行列ではなく「新しく現れた
  Job 名」を見る（`checks/05` の 85-86 行 / 157-159 行）。ただし実物の worker
  込みでハーネスを 1 周させた確認はまだ無い。
- **判定 1.7 は `ruler` キュー（site 修飾されない）を引く消化側も要求する。**
  判定が探す鍵は `epg_<site>` と `reconciler_<site>` の 2 つだけである。
  ところが intent の PUT が入れるのは `ruler_pass` のヒントである
  （`internal/api/rules.go` の `insertRulerPassHint`）。`reconcile_pass` は
  その `ruler_pass` が入れる（`internal/worker/ruler_pass.go`）。
  `ruler` を引く Pod が無いと 1.7 は TODO ではなく **240 秒待って FAIL** する。
  しかもメッセージは「予約が mirakc に届かない … reservations=0」なので
  **reconciler を疑わせる**。
- **判定 3 の producer は既定を差し替えてある**（`lib/env.sh` の
  `E2E_ENCODE_PRODUCER` = `lib/kube.sh` の `produce_real_encode_job`）。
  既定の `insert_probe_job` では緑にならない --- `e2e_probe` は実物の worker には
  未登録 kind なので数秒で終わり、「3.1 は PASS、3.2 / 3.3 / 3.4 が `completed`
  分岐で TODO」になる（TODO が 1 件でも `summary` は exit 2 を返す）。
  差し替えが噛み合わせているものは 5 つで、**どれか 1 つでも欠けると判定 3 は
  「殺された」ではなく TODO で抜ける**:
    - `overlays/e2e/config.yml` の `encode.profiles`（`e2e-slow`）。**狙って
      遅くしてある。** 3.2 と 3.3 がそれぞれ `2 × pollingInterval` の窓を
      取るので、エンコードはその合計より長く走り続ける必要がある
    - ffmpeg 入りイメージ。触る場所は 3 つある（`lib/env.sh` の
      `E2E_FULL_IMAGE`、`lib/cluster.sh` の build と `kind load`、
      `overlays/e2e/kustomization.yaml` の `images:`）
    - media ボリュームの上の原本（`cluster/media-seed-job.yaml`）。
      **ツールボックスからは書けない。** 製品の PVC はツールボックスより後に
      立つので、挿すと足場が Pending で止まる
    - その原本を指す `recordings` / `media_assets` の行
    - `encode` キューへの直 INSERT（**`rokuban enqueue` に `encode` は無い**。
      あるのは DB を読んで投入する `encode-reconcile`）
- **前の周回が残した `retryable` 1 件で判定 2 が二重に落ちうる。** `retryable` は
  滞留（`riverBacklogStates`）に数えられるので、2.2 の「待ち行列が空」が
  180 秒粘って FAIL する。同時に `pendingJobStates` にも入るので、
  `enqueue` が投入をスキップして 2.3 も落ちる。`--once` の Job がリーダーになれば River の
  `JobScheduler` が昇格させるので自己回復するが、**その所要時間は測っていない**。
- **トリガが数える River の状態は `available` / `retryable`。** ハーネスの
  「滞留」の定義（`lib/kube.sh` の `riverBacklogStates`）と同じ集合にすること。
  ずれると、失敗して指数バックオフ中（`scheduled`）のジョブ 1 件で判定 2 が
  誤 FAIL し、判定 5 が「滞留を作れない」で落ちる
- **env の解決に失敗した ScaledJob は、原因を直しても spec が変わらなければ
  再 reconcile されない**（接続文字列を直した後も 3 分間
  `ScaledJobCheckFailed` のままだった。実測）。作り直すのが早い
- **判定 3 が置いていく残骸が 2 つある。** 3.4 が Job を消すので、掴まれていた
  `river_job` の行は **`running` のまま残る**（回収する `JobRescuer` は
  リーダーだけが動かす保守サービスなので、ロール分割構成では誰も回収しない ---
  これは製品の壊れ方そのものであって、ハーネスの都合ではない）。それと
  media ボリュームの原本。どちらも `produce_real_encode_job` が周回の頭で
  消してから測り直す。**`$producer` は関数名でもコマンド文字列でも受ける**
  （引用せずに展開する）ので、別の作り方を試すときは env で差し替えればよい
- **未解決: 判定 3 は encode を KEDA ScaledJob で回す形を前提にしている。**
  別の形（Deployment + HPA 等）を採るなら、この判定は永久に TODO のままになる。
  つまり受け入れ 3 が一度も検査されないまま終わる。形を変えるなら判定も同じ
  PR で書き換えること


## ファイル

```
run.sh                 入口。クラスタの用意 → 判定 → 集計
oracles.sh             --oracles の中身（fixture と変異）
lib/env.sh             名前・版・パスワードの唯一の出どころ
lib/log.sh             PASS / FAIL / TODO と終了コード
lib/kube.sh            クラスタを触る共通関数（**時間で待たない**）
lib/cluster.sh         kind / イメージ / KEDA / 足場の用意
cluster/scaffold.yaml  postgres / mirakc モック / ツールボックス
checks/0*.sh           判定 1〜5
lib/selftest.sh        lib の純粋な部分のユニットテスト（クラスタ不要。CI で回す）
fixtures/              オラクル自己検査用の身代わり（製品ではない）
                       製品と同じ役ラベルを名乗るので、残っていると判定が
                       これを製品と見なす。preflight が残骸を落とす
mutants/               変異（イメージを焼き直す種類のもの）
mirakcmock/            mirakc モック（Go。`go test ./deploy/k8s/e2e/...` が見る）
```

変更したら次の 2 つを回す:

```sh
# どちらもリポジトリのルートから
./deploy/k8s/e2e/lib/selftest.sh   # クラスタ不要。CI でも回る
shellcheck -x -e SC1091,SC2034,SC2317,SC2329 \
  deploy/k8s/e2e/run.sh deploy/k8s/e2e/oracles.sh \
  deploy/k8s/e2e/lib/*.sh deploy/k8s/e2e/checks/*.sh
```

SC2317 / SC2329（到達しない・使われないように見えるコード）を外しているのは、
判定の述語を `retry_until` と `trap` に**間接的に**渡す形が多いため。番号が
2 つあるのは、shellcheck の版によってどちらで報告されるかが違うから（CI は
版を固定している）。

**`lib/` の純粋な部分にはテストがある**（対象の探索・watch ログの読み取り・
終了コード）。そこが無かったせいで、レビュー修正で入れた「複数一致は FAIL」の
経路が**大域変数の代入が `$( )` の子シェルに閉じるせいで一度も動いていな
かった**。実機 2 周でも変異注入でも捕まえられなかった（変異がその状態を
作らなかったため）。判定の外側（クラスタが要る部分）は変異注入で、内側は
ここで見る。
