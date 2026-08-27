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
  の対象は 8 種ある。`worker.periodic_jobs: false` の下では全部が CronJob 側に
  移るので、7 本欠けていても 0 になる
- **ScaledJob は `epg_<site>` / `reconciler_<site>` / `encode` しか見ていない。**
  `ingest_<site>` / `watcher_<site>` が欠けていても 0 になる
- **Deployment は役ごとの存在と「宣言した数だけ Ready」だけ。** サイトごとの
  網羅は見ていない（site B の watcher が無くても判定 1 は緑）
- **役ごとの到達性は見ていない。** Service も Ingress も無い notifier でも、
  Deployment が Ready なら判定 1.2 は緑になる。`/api/events` が notifier に
  届くかは誰も測っていない
- **定期パスの網羅も見ていない。** `worker.periodic_jobs: false` の下では
  in-process の定期ジョブ 9 種が CronJob 側に移るが、判定が見るのは
  `epg-sync` の 1 本だけ。`delete_reconcile` は `rokuban enqueue` に載って
  いないので、そもそも CronJob にできない（扱いは未決）

網羅を判定に入れるなら、`config.yml` の `mirakcs:` と `rokuban enqueue` の
ジョブ表から期待集合を導く判定を足すことになる。**いまは足していない**
（何を CronJob にするかはワークロードを書く側の判断で、判定側が先に固定すると
不変条件 11 に反する）。

**判定が黙って死ぬのも 0 にしない。** 各判定は自分が記録するはずの id を
`plan` で先に宣言し、宣言と記録が食い違えば集計側が FAIL を書き足す。これが
無いと、全部 PASS になった後で「判定 5 が起動直後に落ちた」が exit 0 になる。

**環境の破損を TODO にしない。** 判定の前に preflight（0.1〜0.5）が走り、
クラスタ・名前空間・KEDA の CRD・製品マニフェスト・DB とマイグレーション・
身代わりの残骸を確かめる。ここが無いと、KEDA の導入に失敗しただけで全項目が
「まだ実装されていない」と**1 文字違わない出力**になる。

## 判定する 5 項目

| | 見るもの | いま |
|---|---|---|
| 1 | 全ロールが上がり、番組表が見えて予約が mirakc に反映される | **部分的に緑**（api だけ） |
| 2 | worker 0 でも CronJob が投入し続け、KEDA が Job を起こして消化する（0 → 1 → 0） | TODO |
| 3 | 実行中の encode Job がスケールインで殺されない | TODO |
| 4 | watcher を 2 レプリカにしても二重に動かない（advisory lock の実効） | TODO |
| 5 | サイト B の滞留でサイト A の Job が起きない | TODO |

**2〜5 が TODO なのは、対象のワークロードがまだ無いからである**（notifier /
watcher / streamer の Deployment、worker の ScaledJob、投入側の CronJob）。
それを確認して残すのがこのハーネスの成果物である。この表は判定を足したり
緑にしたりする人が更新する。

**ワークロードを書けば緑になるわけではない。** 判定 2 と 3 が要求していた
製品バイナリ側の 2 つ（Job の自己終了 / キューを argv で絞る手段）は
`--once` と `--queues` で入ったが、判定 2.3 が要求する CronJob の schedule は
まだ決まっていない（下記「先に決めること」）。「TODO なのは対象が無いからだ」
とだけ読むと、ワークロードを書いてからその壁に当たる。

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
└── 製品（deploy/k8s/overlays/e2e）
    migration Job / ConfigMap / api Deployment + Service + PDB
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
**ただし絞っているのは探索だけである。** River のキュー・mirakc モック・
CronJob は製品と共有のまま。製品の CronJob が投入したジョブや、製品の
ScaledJob が消化した滞留が、身代わりの観測に混ざりうる（未検証。いま製品の
ワークロードが無いので踏んでいない）。**そのときは製品側を止めてから回すこと。**

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
- **判定 1.6 / 1.7（番組表が見える / 予約が mirakc に反映される）は一度も
  実行されていない。** オラクル検査 1 は製品の api だけを立てて回すので、
  `epg_sitea` を引く ScaledJob が無く、1.6 / 1.7 は必ず TODO 分岐で抜ける。
  **緑になったことも赤になったこともない判定**なので、worker を足したときに
  1.6 が赤くなったら、まず「ワークロードが壊れている」ではなく「判定が
  最初から動かない」を疑うこと。判定 4 が本物のイメージで身代わりを立てて
  いるのと同じやり方（`fixtures/watcher.yaml`）で、epg_sync と reconcile_pass を
  消化する身代わりを足せば、ここも変異まで通せる

変異イメージは**リポジトリの複製**（rsync したツリー）に当てて焼く。
`git stash` は使わない --- ワークツリーは他の作業と共有されうるし、隔離
worktree と併用すると stash が互いに干渉する。変異は**コンパイルが通る形**で
入れる（未使用変数でビルドが落ちると、判定が「FAIL になった」ではなく
「イメージが無い」で赤くなる）。

## 残りのワークロードを書くときに

ScaledJob 自体の書き方（トリガの接続先・`rollout.strategy`・切る軸）は
[docs/operations.md](../../../docs/operations.md) §5「worker: KEDA ScaledJob」が
権威。ここにはハーネス側の契約と未解決の穴だけ置く。

### 先に決めること（マニフェストを書き始める前に）

**未解決: 判定 2.3 は CronJob が 180 秒以内に自然発火することを要求する。**
epg_sync の実運用相当の間隔は 10 分なので、出荷する schedule のままだと
2.3 が FAIL になる。`overlays/e2e` で毎分に patch する方針を採るなら、
**判定 2 が測るのは出荷される schedule ではなくなる**ので、その旨を上の
「0 が保証しないもの」に足し、「base は正気の schedule、e2e overlay は毎分」を
`manifests_test.go` で固定すること。

決着済み（製品バイナリ側は入っている。ScaledJob / CronJob にはこう書く）:

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

- **未解決: `insert_probe_job` が入れる `e2e_probe` ジョブを実物の worker が
  どう扱うかは未検証**（実物の worker が居る状態でハーネスを回したことがない）。
  判定 3 の producer と判定 5 の positive control の**両方の土台**なので、
  破れると 3 と 5 がまるごと使えない。**最初にここを確かめること。**
- **トリガが数える River の状態は `available` / `retryable`。** ハーネスの
  「滞留」の定義（`lib/kube.sh` の `riverBacklogStates`）と同じ集合にすること。
  ずれると、失敗して指数バックオフ中（`scheduled`）のジョブ 1 件で判定 2 が
  誤 FAIL し、判定 5 が「滞留を作れない」で落ちる
- **env の解決に失敗した ScaledJob は、原因を直しても spec が変わらなければ
  再 reconcile されない**（接続文字列を直した後も 3 分間
  `ScaledJobCheckFailed` のままだった。実測）。作り直すのが早い
- **未解決: `E2E_ENCODE_PRODUCER` の差し替え先は、いまはまだ書けない。**
  実 encode を 1 件作るには `recordings` / `media_assets` の行と実ファイルが要る。
  しかし `rokuban enqueue` に `encode` は無く（あるのは DB を読んで投入する
  `encode-reconcile`）、足場にメディア用のボリュームも無い。判定 3 を緑にする
  には、まずメディアボリュームと最小の recording を仕込む手順を決める必要が
  ある。`$producer` は関数名でもコマンド文字列でも受ける（引用せずに展開する）
- **未解決: 判定 3 の「実行中の encode Job を作る」手順は実物に依存する。**
  ハーネスの既定は River の encode キューに 1 行入れるだけである。身代わり
  （長く寝る Job）ではこれで足りるが、**本物の encode ワークロードでこれが
  長時間 Job になるかは未検証**。`E2E_ENCODE_PRODUCER` で差し替えられるように
  してあるので、実際に時間のかかるエンコードを投入する手順に差し替えること。
  **判定そのもの（生存の観測と positive control）は身代わりで検証済み**なので、
  差し替えるのは「実行中の Job を 1 つ作る」ところだけでよい
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
