> [operations.md](../operations.md) §5「k8s 運用」の一部。索引から辿る。

## 5. k8s 運用

### ロールとキュー購読の関係

**worker ロールだけが River のキューを引く**。他のロールは、そのプロセスが実際に
worker ロールを持つかどうかに関わらず、ジョブを実行しない。ロール分割デプロイで
`--roles watcher` のような worker を含まない構成を組んだときに、
起動時検査が実態より広い安心を与える経路が過去にあった（末尾「経緯と将来の構想」）。
現在は次の 2 点で構造的に保証している:

1. **watcher 単独プロセスは River クライアントを Start しない**（`--roles worker`
   を含む場合だけ `river.Client.Start` を呼ぶ）。加えて watcher 単独プロセスは
   `worker.NewWorkers` のフルのワーカー群（`EncodeWorker` / `ThumbnailWorker` を
   含む）を登録しない --- api ロールと同じ `worker.NewInsertOnlyClient`（ingest
   ジョブの `InsertTx` 専用、`Queues` / `Workers` を持たないため `Start` 自体が
   呼べない構成）を使う。watcher が必要とするのは ingest ジョブの投入だけであり、
   実行（キューの購読）ではないので、これで用は足りる
2. **`worker.queues`（同じ値を argv で渡す `--queues` も同義）は worker ロールが
   引くキューの絞り込みであり、既定（どちらも無し）は全キュー購読**。worker ロールが無いプロセスはこの設定に関わらずキューを
   一切引かない（[configuration.md](../configuration.md) の `worker.queues`）

ffmpeg/ffprobe の起動時検査（不変条件 4）も、実際に encode/thumbnail キューを
購読するときだけ行う（`worker.RequiresEncodeTools`）。`worker.queues` で
encode/thumbnail を明示的に除外した worker Pod（例: ingest 専用 Pod）は ffmpeg
の存在を要求されない。逆に既定（全キュー購読）や encode/thumbnail を含む設定では
起動時に LookPath で検査し、無ければ即座に落ちる。ffmpeg が無い環境で
encode/thumbnail ジョブが River の再試行を焼き続けてから気付く、という壊れ方を防ぐ。

**`worker.queues` に書く名前は論理名（unqualified）である**。KEDA のスケーラが
引くキュー名は物理名に修飾される場合がある。mirakc への到達性を要する site 単位の
キュー（`ingest` / `epg`（`tuner_sync` も同じ）/ `reconciler` / `watcher`）だけが対象になる。
修飾名はプロセスが束縛されているサイト名で `<論理名>_<site>`（例: `ingest_tokyo`）となる。
`ruler` / `encode` / `thumbnail` / `cleanup`
（`delete_reconcile` / `catalog_export` のキュー）/ `storage`（`storage_sync` の
キュー）/ `default`。これらは site に依存しないので
修飾されない。修飾は `worker.queues` の
設定値・未知キューのエラーメッセージのどちらにも現れない。設定・エラー文言は
常に論理名のままで、実際にキューを引くプロセス（KEDA のスケーラ定義や Prometheus の
`river_job` メトリクス等）だけが物理名を見る。

キューごとの要求が置き場所を決める（ロールではなくキューの割り当てで置き場所が決まる。
[overview.md](../overview.md) §サーバーレスデプロイとハイブリッド構成）:

| キュー | 要求 | 置き場所（ハイブリッド構成） | site 軸 |
|---|---|---|---|
| `reconciler` / `epg`（+`tuner_sync`）/ `watcher` | mirakc への到達性 | 自宅側 | **束縛**（キュー名を `_<site>` で修飾） |
| `ingest` | mirakc への到達性 + ファイルシステム | 自宅側 | **束縛**（同上） |
| `encode` / `thumbnail` / `cleanup`（`delete_reconcile` / `catalog_export`） / `storage`（`storage_sync`） | ファイルシステム | 自宅側 | 非依存（アーカイブ・スクラッチは単一） |
| `ruler` | DB のみ | どちらでも | 非依存（`args.Site` はクエリの絞り込み） |

0 サイト束縛（中央プロセス、`server --sites=`）の worker は
site 単位のキューを一切購読できない（`worker.RequiresSiteBinding` が起動時に
強制する）。中央プロセスで動かせるのは `ruler` / `encode` / `thumbnail` /
`cleanup` / `storage` / `default` に `worker.queues` / `--queues` を絞った構成だけである。
ただし `cleanup` / `storage` は表の通りファイルシステムを要求するので、
「0 サイト束縛」（mirakc の site 数の話）と「ファイルシステム不要」を混同しないこと。
中央プロセスであっても media_dir/scratch_dir のマウントに到達できるノードで
動かす必要がある。

**PVC マウントの所有権は `fsGroup` で与える。** イメージは `/mnt/media` を実行ユーザー（uid/gid `65534`）所有で焼いてある。ただしこれが効くのは Docker の named volume の copy-up 経由だけである。**PVC には copy-up が無い**ので、所有権を与えないと `permission denied` になる。

参照実装は media をマウントする Pod に `securityContext.fsGroup: 65534` を書いている。対象は streamer と、`ingest` / `encode` / `thumbnail` / `cleanup` / `storage` の worker である。**kind（rancher.io/local-path）では実際に書けることを確認した** --- 受け入れ判定ハーネスの判定 3 が実際にエンコードを 1 件走らせて media に書く。**`fsGroup` を尊重しない CSI ドライバでは足りない**（そちらの手当ては未検証）。

**`fsGroupChangePolicy: OnRootMismatch` も対で書く。** 既定の `Always` は Pod を起こす前にボリューム全体を再帰的に chown する。ScaledJob はキューアイテムごとに Pod を作るので、数 TB のアーカイブでは毎回それを待つことになる。**kind では出ない**（ボリュームが空なので一瞬で終わる）。

### streamer のスケール

**streamer は録画配信（VOD・サムネイル）とライブ視聴の両方を担うが、置き場所も
スケールの仕方も別である**。ロールは分けない --- ロールを決めるのは「ソケットを
持ち続けるか」だけで、mirakc を触るかどうかは判定軸に入らない
（[overview.md](../overview.md) §ロール分類の基準）。同じロール・同じバイナリで、
デプロイのパラメータだけが違う（`worker.queues` でキューごとに置き場所が決まるのと
同じ構造）。

| | 置き場所 | レプリカ | 前段 | イメージ |
|---|---|---|---|---|
| 録画配信 / サムネイル | メディアストレージの隣 | 水平（N） | 素の round-robin | 公式（ffmpeg 不要） |
| ライブ視聴 | mirakc の隣（サイトごと） | 既定 1 | `(site, networkId, serviceId)` の consistent hash | `Dockerfile.full` |

**未解決: この表のとおりには 1 式に書けない。** `live.enabled: true` は **streamer ロールの全 Pod にちょうど 1 サイトの束縛を要求する**（`cmd/rokuban/server.go`）。ライブは `boundSite` を使うので、束縛が 0 個でも 2 個でも成り立たない。

config は全 Pod で 1 個共有なので、ライブを有効にした瞬間に**中央の録画配信 Deployment（`--sites=`）が起動しなくなる**。中央側にも `--sites <site>` を書けば動く（アーカイブは単一なので、どのサイトを書いても配れるものは変わらない）。ただしそれは「録画配信は site 非依存」という上の決定の暗黙の変更である。`deploy/k8s/` はライブの streamer を出荷していない。

#### 録画配信はセッション親和性を必要としない

録画配信は完全にステートレスで（DB からアセットを解決して `http.ServeContent` で
配るだけ。`internal/streamer`）、貼り付けるべき状態が無い。**sticky を Service /
Ingress に書かない**。書くと、数 GB の Range 読みという最も分散させたい経路で
分散が効かなくなる。X-Accel-Redirect でバイト転送を前段に委ねる構成とも噛み合わない。

アーカイブは単一である（[§4](storage.md) / [docs/storage.md](../storage.md) §5 の 2 階層。録画バッファは
サイトごとのエッジ、アーカイブは 1 つ）。したがって**複数サイト構成でも録画配信の
Pod は site 非依存**で、どの Pod でもどの録画を配れる。`recordings.id` は surrogate
なのでサイト間で曖昧にならず、URL に site を持つ必要もない。

再生中に Pod が落ちてもクライアントは Range で再接続して継続する。決めるべきは
`terminationGracePeriodSeconds` を進行中の転送を打ち切らない長さにすることだけ。

#### ライブ視聴は mirakc の隣、シャード鍵はサービスの同定子

生 TS は 17 Mbps 連続なので WAN に出さない。**トランスコードは mirakc と同じサイトで
行い、HLS になったものだけがサイトの外に出る**。

セッションを Pod に貼り付ける代わりに、**前段で `(site, networkId, serviceId)` の
consistent hash によって振る**。この鍵は既に資源同定の中にある
（`/api/sites/{site}/networks/{networkId}/services/{serviceId}/...`。
[api.md](../api.md) §ライブ視聴の HLS）。SI の service_id は network をまたぐと
一意でないので、鍵も 3 項になる。鍵が資源同定にあるので、レプリカを増やしても URL もクライアントも変わらない。

- **cookie による affinity は使わない**。外部プレイヤー（VLC 等）は cookie を
  持たない。OSS nginx に `sticky cookie` が無い（nginx-plus の機能）。
  ingress-nginx の既定 `affinity-mode: balanced` はスケール時に**生きている Pod
  からもクライアントを剥がす**
- **同じチャンネルの視聴者は同じ Pod に落ちる**ので、ffmpeg 1 本・チューナー 1 本を
  共有する。チューナーは録画と取り合う唯一の共有資源なので、これは偶然の利益ではなく
  鍵をサービスの同定子に取る理由そのもの
- **Pod が落ちても、ハッシュの担当が移っても自己修復する**。視聴者の再要求が新しい
  Pod に落ちてそこで ffmpeg が起き、旧 Pod は要求が来なくなるので idle GC が
  チューナーを解放する。クライアント側にセッションの概念が無いので、見えるのは
  再バッファリングだけ

#### 既定を 1 にする根拠と、増やす判定基準

**既定は replicas=1**。天井は Pod の CPU ではなく**サイトのチューナー数**である
（ライブ 1 本がチューナー 1 本を占有し、残りが録画に使われる）。1 サイトのチューナー
数ぶんのトランスコードが 1 Pod に収まる間は、増やす理由が無い。

収まらなくなったら **replica を増やし、前段に `(site, networkId, serviceId)` の consistent hash
を設定するだけでよい**。URL・クライアント・API は変わらない。この可逆性を保つために
実装側で守るのは次の 3 点:

1. **プレイリスト / セグメントの URL を固定深さにする**。前段が 1 つの nginx 変数で
   鍵を取り出せる形にする。OSS nginx なら
   `map $uri $live_key { ~^/api/sites/(?<s>[^/]+)/networks/(?<n>[^/]+)/services/(?<sv>[^/]+)/live/ "$s/$n/$sv"; }`
   → `hash $live_key consistent;`。可変長パスやクエリ文字列に鍵を置くと書けない
   （ingress-nginx の `upstream-hash-by` で同じキャプチャがどう書けるかは未検証）
2. **同時セッション上限はプロセスローカルであることを前提にする**。グローバルな
   天井はチューナー数であり、その裁定者は mirakc である。「アプリが握る唯一の
   グローバル上限」を作ると、レプリカを増やした瞬間に嘘になる
3. **セッション数のメトリクスは per-process gauge にする**。Prometheus 側で sum
   する。1 Pod の値を全体として読む UI を作らない

#### ライブのセグメントを録画バッファと同じディスクに置かない

エッジの録画バッファ（mirakc `recording.basedir`）は「I/O 飽和 = ドロップ直結」で、
要求は絶対帯域ではなくレイテンシである（[§4](storage.md)）。ライブの HLS セグメント書き出しが同じ
ディスクに乗ると、**視聴が録画を壊す**。セグメントは数 MB × 数本なので tmpfs
（k8s なら `emptyDir: {medium: Memory}`）で足りる。Postgres datadir とエンコード
scratch を分ける指針（[§3](database.md)）と同じ系列の規則。

### マニフェストの配布形式: 素の kustomize

参照実装は `deploy/k8s/` に置く。**中身の索引はそちらの [README](../../deploy/k8s/README.md)** で、ここには判断だけを置く。分け方は次の 2 つ:

- **`base/` は中央（site 非依存）1 式。** api / notifier / 録画配信 streamer / media の PVC / migration Job / ConfigMap が入る。site 非依存キューの ScaledJob と、site 非依存ジョブの CronJob もこちら
- **`site/` はサイト 1 組ぶん**（watcher / site 束縛キューの ScaledJob / site 束縛ジョブの CronJob）。overlay がサイトの数だけ生やす

**base は複数サイトを知らない。** レジストリは常に 1 要素、どの Pod も site に束縛しない（`--sites=` の明示的な空を渡す）。サイトを増やす差分は「`mirakcs:` に 1 要素 + Pod セット 1 組」に保つ。`site/` は site 名 `default`（`base/config.yml` の `mirakcs:` 1 要素目と同じ名前）で書いてあり、単一サイトの overlay はそのまま使える。

**Helm chart は持たない。** `values.yaml` は後から狭められない公開 API で、どの値を露出するかを決める前に形を固定することになる（不変条件 11）。加えてサイトごとの差分は argv（`--roles` / `--sites`）だけなので、overlay がサイト単位で薄く書ける。Helm が要るなら kustomize の出力を写す形で後から足す。

**ConfigMap は 1 個で、全 Pod が同じ config.yml を共有する。Pod ごとに違うのは argv だけ**にする。ここを Pod 別の config キーにすると、サイトを増やすたびに ConfigMap が増える（[configuration.md](../configuration.md) の「ロール別 Deployment でも config は同一ファイルを共有する」が崩れる）。

**config は generator で作る（素の ConfigMap にしない）。** rokuban は config を起動時に 1 回しか読まない（設定変更は再起動。[configuration.md](../configuration.md) §やらないこと）。素の ConfigMap は名前が変わらないので Pod テンプレートも変わらず、apply しても rollout が起きない --- つまり**設定変更が黙って効かない**。generator が付ける内容ハッシュが、この「読み直さない」という決定と apply を噛み合わせる唯一の部品である。

**env（`${VAR}`）で渡すのは機微情報だけにする。** 非機微の値を env にすると、唯一の `envFrom`（Secret）へ非機微値を詰めることになる。環境差は overlay の patch で当てる。

もう 1 つの理由は名前の衝突である。k8s は同一 namespace の Service ごとに `<SVCNAME>_PORT` / `<SVCNAME>_SERVICE_HOST` 等を全 Pod に注入する（service links）。`${VAR}` を増やすとこの注入と衝突しうる。実測: `postgres` Service が `POSTGRES_PORT=tcp://10.96.x.x:5432` を注入した。これが `port: ${POSTGRES_PORT:-5432}` に入り、api も migrate も config のパースで落ちた。Pod 側でも `enableServiceLinks: false` で注入を止める。

**中央（site 非依存）の Pod には `--sites=`（明示的な空）を書く。** 省略しても単一サイト構成では動くが、レジストリに 2 サイト目を足した瞬間に起動しなくなる（束縛の暗黙の「全部」を許していない）。つまり気付くのが最も遅い形で壊れるので、単一サイト構成のうちから明示する。

**Secret はマニフェストに出荷しない。** プレースホルダを `resources` に入れると、参照実装を apply するだけで外部管理の本物のパスワードを上書きしてしまう。上書きした瞬間は動いている Pod が死なない（env は起動時に読まれている）ので気付かず、次の rollout やノード退避で初めて全 Pod が起動不能になる --- そのときクラスタからパスワードも消えている。要求する形は apply されない参考ファイルに書き、供給は運用者（または overlay の generator）に委ねる。

**レプリカを増やすだけでは可用性にならない。** PodDisruptionBudget が無い Pod は `kubectl drain` が無条件に退去させるので、ノード 1 台の退避・アップグレードで全レプリカが同時に落ちる。Service の後ろに置く役には PDB を対で書く。

マニフェストの検査は「YAML として組めるか」（`kustomize build`）と「スキーマに合うか」（kubeconform）だけでは足りない。**オブジェクト間の参照はどちらも検出しない。** 存在しない ConfigMap を `envFrom` に書いても、probe が指す名前付きポートが消えても、Service の selector がどの Pod にも当たらなくても緑になる。ConfigMap に入れる config 本体の中身（strict なパーサが弾くキーの typo）も見ない。これらの検査は別に持つ（`deploy/k8s/manifests_test.go`）。

### worker: KEDA ScaledJob（長時間ジョブ保護）

長時間バッチ（数時間のエンコード / ingest）には Deployment + HPA ではなく **KEDA ScaledJob** を使う。キューアイテムごとに k8s Job を起こす形にすると、**ジョブは完走するまで殺されない** --- スケールインは「新しい Job を起こさない」ことで実現され、実行中の犠牲者選定という問題自体が消える。

River の at-least-once / 冪等性は「殺されても正しい」を保証済みであり、この決定は「殺されても安い」を足すもの。

**ScaledJob はロールではなくキュー単位に作り、切る軸は「寿命 × site 束縛」の 2 つ。**

- **長時間ジョブと短いジョブを混ぜない**（`ingest` / `encode` は実行中に殺せない、`ruler` / `reconciler` は殺してよい）。`terminationGracePeriodSeconds` が桁で違うので、混ぜると短い側に合わせて長いジョブが殺されるか、長い側に合わせて全体のスケールインが遅くなる
- **site 束縛キューと site 非依存キューを混ぜない**。混ぜると中央のジョブがサイト側で起きる。結果、site 束縛キュー（`ingest_<site>` / `epg_<site>` / `reconciler_<site>` / `watcher_<site>`）の ScaledJob だけサイト数ぶん複製する
- **スケーラが引くキュー名が site 修飾されていること**を確かめる。共有キューを見ていると、サイト A のスケーラがサイト B の滞留で Job を起こし、起きた Job は自分のサイトの仕事が無いまま終わってまた起きる

**Job は 1 件消化して自分で終了する（`rokuban server --roles worker --once`）。** ScaledJob は Job の自己終了を前提にした機構である。常駐する `--roles worker` をそのまま載せると Job が `succeeded` に到達せず、「0 → 1 → 0」が成立しない。`--once` の形は次に固定してある。

- ジョブ 1 件の Work を抜けたらプロセスを畳む。**成功・失敗を問わず exit 0**（リトライは River が持つので、k8s 側は `backoffLimit: 0` / `restartPolicy: Never` にして二重にリトライしない）
- **`--once-idle-timeout`（既定 30 秒）は実行中のジョブを打ち切らない。** 「1 件も掴めないまま待った時間」にしか効かないので、数時間のエンコードは対象にならない。KEDA が滞留を過大評価して起こした空振りの Job を終わらせるためだけの値である（`worker.OnceGate` / `TestOnceGate_IdleTimeoutDoesNotCutRunningJob`）
- **打ち切りうるのは SIGTERM のほうである。** ノード退避・ローリング更新で SIGTERM を受けた Job は、`--soft-stop-timeout`（既定 5 秒）を超えた時点で実行中のジョブの ctx を切る。**長時間キュー（`encode` / `ingest`）の ScaledJob では `--soft-stop-timeout` を明示する。** 既定のままだと、猶予を 300 秒に取った ScaledJob でも drain は 5 秒で終わる。`terminationGracePeriodSeconds` と対で引き上げること（下記「Deployment 併用時」の足し算。**上限を外すつもりで `0` と書くと起動エラー** --- 0 は「無制限」ではなく「待たない」である）
- 引くキューはちょうど 1 つで、その `MaxWorkers` は 1 に落とす（KEDA 側が「1 Job = 1 アイテム」で数を合わせるため）。`worker.periodic_jobs: true` との併用は起動エラー —— 1 件で終わる Job がリーダーになると、定期投入の間隔が Job の起動回数で決まってしまう
- argv の制約は 3 つで、いずれも起動エラーになる
    - `--roles worker` 単独であること（常駐する役と同居させない）
    - `--once-idle-timeout` が正の値であること
    - `--once-idle-timeout` を `--once` 無しで書かないこと
- encode の ScaledJob だけ `Dockerfile.full` のイメージを使う。公式イメージは ffmpeg を同梱しないので、encode / thumbnail キューを購読する worker は起動時に fail-fast する

**停止の順序が「実行中を打ち切らない」を支えている。** 1 件消化モードは、先に River の graceful stop を撃つ（producer の fetch を止めて実行中を待つ）。そのあとでプロセスを畳む。畳む側は Start に渡した ctx の cancel である。順序は `TestStopOnceProcess` が固定している。

逆順にすると、掴んでいたジョブが完走に使える時間が `--soft-stop-timeout` の猶予まで縮む（下記「Deployment 併用時」）。graceful stop を先に撃つほうが、猶予を消費せずに完走できる。

既知の窓: Work を抜けてから producer が止まるまでの間に次のジョブが fetch されると、1 つの Job が 2 件消化して終わりうる。`MaxWorkers` が 1 なので同時には走らない。**打ち切られるのは `--soft-stop-timeout` の猶予を超えたときだけ**で、超えればその 2 件目は試行を 1 つ潰して `available` に戻る。これは上記の順序と River の実装からの導出であって、測定ではない（`Stop` は producer を止めてから実行中の完了を待ち、猶予が切れたら work ctx を cancel する）。実害は KEDA の数が 1 つずれることと、その再試行だけである。実測では 25 回試してこの窓に入らなかった。**窓の中の挙動そのものは未測定**である。

失敗するジョブ（到達不能な mirakc への `epg_sync`）でも **exit 0** になる。ジョブは River 側で未完了のまま（`attempt=1`）残る（`TestServerCmd_OnceModeExitsZeroOnJobFailure`）。**次の Job がそれを引き直すところまでは測っていない。**

**登録されていない kind のジョブを掴んだ場合は 1 回失敗させて終了する**（ログの `outcome` 属性が `job_unhandled`）。River の executor はこの場合ワーカーのミドルウェアを通らない。そのため購読した終了イベントで観測している（`worker.SubscribeOnceEvents`）。版ずれで踏む --- 新しいイメージの CronJob / api が、古い worker の知らない kind を投入する形である。`TestServerCmd_OnceModeExitsOnUnhandledJobKind` が固定している。

**スケーラのクエリは `available` だけでなく `retryable` も数える。** 失敗したジョブを `retryable` から `available` に戻すのは River の `JobScheduler` である。これはリーダーに選出されたクライアントだけが動かす保守サービスである。ロール分割構成では常駐する River クライアントが 1 つも無い（api / watcher / `enqueue` はいずれも insert 専用で `Start` しない）。そのため `available` だけを数えると **失敗したジョブが永久に止まる** --- Job が起きないので誰も昇格させず、昇格しないので Job も起きない。

**キューは argv で絞る（`--queues`）。** ScaledJob はキュー単位に作るのに ConfigMap は 1 個である。キューを config キー（`worker.queues`）でしか指定できないと、ScaledJob の数だけ ConfigMap が増える（上記「マニフェストの配布形式」の決定が崩れる）。`--queues` と `worker.queues` の**両方指定は起動エラー**にしてある --- どちらが勝つかを覚えておく形にすると、monolith と k8s で購読集合の出所が分かれる。`--queues=`（明示的な空）も起動エラーである。「全キュー」に化けると、site 束縛キューまで掴んで `verifySite` で全滅する Pod が黙って生まれる。

**この排他は共有 ConfigMap と結合している。** `--queues` を使う構成では、共有する config.yml に `worker.queues` を書いてはならない。書いた瞬間に、`--queues` を渡している worker Pod が**すべて**起動エラーになる。ConfigMap を 1 個に保つ決定（上記「マニフェストの配布形式」）と組み合わせると、この 1 行が全 worker を落とす形になるので、キューの指定は argv 側に一本化する。

**スケーラが `retryable` を数えるので、バックオフ中のジョブに対して空振りの Job が起きる。** `retryable` の行は `scheduled_at` が来るまで claim できない。起きた Job は仕事を掴めず `--once-idle-timeout` で終了する。River の `JobScheduler` が昇格させれば次の Job が引くが、**昇格までの所要時間は測っていない**。実害は空振り Job の増加だけである。

**「実行中の Job は殺されない」は無条件ではない。** `rollout.strategy` の書き方に依存する。KEDA (v2.20.2) が受け付ける値は `gradual` と `immediate` の 2 つ。kind での実測は次のとおり。

| `rollout.strategy` | Pod テンプレートを更新したとき |
|---|---|
| `gradual` | 実行中の Job は生き残る |
| `immediate` | 実行中の Job が消える |
| 省略 | 実行中の Job が消える |

ScaledJob 本体への annotation だけでは消えない。KEDA が rollout と見なすのは Pod テンプレートの変更である。**つまり `rollout.strategy: gradual` は書き忘れてはならない 1 行である。**

表と annotation の件は [deploy/k8s/e2e](../../deploy/k8s/e2e/README.md) の `O3.mut-rollout` / `O3.mut-omitted` で実測した。運用でこれを踏むのはイメージのタグを上げたときである。症状は「デプロイしたら録画のエンコードが飛ぶ」になる。上の「実行中の犠牲者選定という問題自体が消える」はこの一行に依存している。

**postgresql トリガの接続先はクラスタ内 FQDN で書く。** 接続を張るのは Job の Pod ではなく **keda 名前空間の operator** なので、同じ名前空間のつもりで短い Service 名を書くと解決されない。このとき ScaledJob は `Ready=False / ScaledJobCheckFailed` のまま Job を一度も起こさず、**症状は「KEDA が壊れた」ではなく「いつまでもスケールしない」**になる（kind で実測）。

site 修飾と `rollout.strategy` は [deploy/k8s/e2e](../../deploy/k8s/e2e/README.md) のハーネスが機械判定する（判定 5 / 3）。**接続先の FQDN には判定が無い** --- 症状はスケールしないことなので判定 2 / 3 / 5 の赤として現れるが、原因を名指しはしない。

### 定期投入: `rokuban enqueue` の CronJob

`worker.periodic_jobs: false` で出荷し、定期ジョブは CronJob から `rokuban enqueue <ジョブ名>` で投入する。River の `PeriodicJobs` はリーダーだけが投入するので、worker が 0 にスケールする構成では誰も投入しなくなる（[§1](monitoring.md) 参照）。

- **argv は平たい要素で書く。** `sh -c "rokuban enqueue ..."` でくるむと、判定ハーネスが投入側の CronJob を見つけられない。探索は argv の要素として `enqueue` と ジョブ名 と `--site <site>` を見る
- 対象は `rokuban enqueue --help` が出す一覧が権威。site 束縛のもの（`--site` が要る）はサイトごとに 1 本ずつ作る
- `enqueue` は同じジョブが待機中なら投入せず exit 0 を返すので、重ねて叩いても安全（CronJob が失敗扱いにならない）
- **schedule は base に実運用の間隔を書く。** 受け入れ判定は 180 秒以内の自然発火を要求するので、判定用の overlay 側で毎分に patch する。base の値が判定の都合で短くなるのを防ぐため、**両方を `deploy/k8s/workloads_test.go` が対で固定している**。**したがって判定 2 が測るのは出荷される schedule ではない**
- **間隔は in-process の既定に合わせる**（`worker.periodic_jobs: true` の monolith）。揃っていないと「docker-compose では 15 分で走るのに k8s では走らない」形になる。`reconcile-pass` だけは合わせられない。in-process の既定は 30 秒だが CronJob の分解能は 1 分である。レベルトリガーなので取りこぼしは次のパスが拾い、差は「予約が mirakc に届くまでの遅延の上限」にだけ効く
- **`rokuban enqueue` にあるジョブは全部 CronJob にする。** 1 つ落とすと、その定期パスが**一度も走らない構成**が黙って立つ（`worker.periodic_jobs: false` では投入経路がここしかない）。網羅は `deploy/k8s/workloads_test.go` の `TestCronJobsCoverEveryEnqueueJob` が見ている

### Deployment 併用時: SIGTERM drain + pod-deletion-cost

Deployment 型で worker を運用する場合（またはその併用）の定石:

- SIGTERM で **drain**（実行中ジョブは完走、新規 claim 停止）+ 長い `terminationGracePeriodSeconds`
- busy な worker が `controller.kubernetes.io/pod-deletion-cost` を上げてスケールイン犠牲者から外れる

**drain の猶予は `--soft-stop-timeout`（既定 5 秒、`river.Config.SoftStopTimeout`）である。** SIGTERM を受けてから実行中のジョブを打ち切るまでの時間で、その内側に終われば完走する。実バイナリで両方向を確認した。猶予 60 秒に対して、SIGTERM の 40 秒後に終わるジョブは `completed` になった。猶予 5 秒に対して、同じジョブは 5 秒で ctx を切られて `available` に戻った（`attempt=1` / `error="… stop initiated"`）。テストは `TestServerCmd_SigtermDrainsRunningJob`（実 DB + mirakc モック、両方向）。

`--soft-stop-timeout` の検査は 2 つあり、`--once` かどうかに関わらず全 `server` 起動に効く。**0 以下は起動エラー**、そして **worker ロールを持たないプロセスでの指定も起動エラー**である。キューを引くのは worker ロールだけなので、`--roles watcher --soft-stop-timeout 5m` は何も待たずに畳む。効かない argv を黙って無視すると、ScaledJob の argv から写し間違えた Pod が「drain するつもりで drain しない」形になる。

**0 は「無制限」ではなく「待たない」**である。River は `SoftStopTimeout` が 0 のとき work ctx を start ctx から継ぐ。`signal.NotifyContext` の ctx を `Start` に渡しているこの構成では、SIGTERM が `StopAndCancel` 相当になる（この節が長く「未解決」として抱えていた壊れ方そのもの）。

**真の上限は `terminationGracePeriodSeconds` 経過後の SIGKILL であり、それは River の外である。** したがって猶予は k8s 側の猶予の内側に置く。外に出すと、猶予が切れる前に SIGKILL が来て、実行中のジョブの行は `running` のまま残る。回収するのは `JobRescuer`（リーダーだけが動かす保守サービス）である。ロール分割構成では常駐する River クライアントが 1 つも無いので**誰も回収しない**（この節の「スケーラのクエリ」と同じ族の問題）。内側に置けば、プロセスが自分でエスカレートして行を `available` に戻してから終わる。**「試行を 1 つ潰す」と「誰も引き直せない」の差**である。

**worker ロールを走らせる Pod**（Deployment でも KEDA ScaledJob が起こす Job Pod でも同じ）のプロセス側の最悪値は次の足し算になる:

```
preStop の sleep + 10s + --soft-stop-timeout + 10s      ← 既定なら preStop 抜きで 25 秒
                   停止待ち        drain        畳み
```

**`terminationGracePeriodSeconds` はこれより長く取る。等号にしない** --- 上の値はプロセスが消えるまでの最悪値そのものなので、等号だと SIGKILL と同着になる。しかも同着になるのは予算を使い切ったケース、つまりここで避けようとしている「行が `running` のまま残る」経路そのものである。既定なら 30 秒（`docker-compose.yml` の `stop_grace_period` がこの値）。

- **worker ロールを持たない Pod では後ろ 2 項が落ちる**（River クライアントを Start するのは worker ロールだけ）。api がこれで、要求は `preStop の sleep + 10s` になる。実際の値は `deploy/k8s/base/api.yaml` の 30 秒で、`deploy/k8s/manifests_test.go` の `TestTerminationBudgetCoversPreStop` が固定している
- 2 項目は **errgroup の停止待ち**で、**drain と足し合わせる**。2 つは SIGTERM を契機に重なって進むので、実際の停止はもっと早いことが多い（River の soft stop タイマーは SIGTERM の瞬間から走る）。それでもプロセスが `Stop` に課す締切は errgroup が畳み終わってから測るので、**上界は和**になる。重なるぶんは安全側のマージンとして残す。**worker 単独の Pod ではこの上限を決めるのは HTTP の停止**（`http.Server.Shutdown` の 10 秒）だけで、それが 10s の出どころである。`--all`（monolith）では ffmpeg の終了待ち（ライブ視聴）や watcher の畳みも同じ待ちに入る。**それらの所要は測っていない**ので、`--all` で 10s に収まる根拠は無い
- SSE（`/api/events`）を掴んだクライアントが居ると HTTP の停止はこの上限を使い切る。`Shutdown` は実行中のリクエストの ctx を cancel せず、SSE ハンドラは自分の接続が切れるまで抜けないからである（機構からの導出。実測はしていない）
- 3 項目は**猶予が切れたあと畳み終えるぶん**（ctx を切られたジョブが `Work` から戻り、River の completer が結果を書く）。**所要は測っていない** --- HTTP の停止と同じ大きさに揃えただけである
- 数時間のエンコード / ingest は既定の 5 秒では完走しない。**`--soft-stop-timeout` と `terminationGracePeriodSeconds` は対で引き上げる**（片方だけ上げても、短い側が先に効く）
- **既定が短いのは「何も書かなかった人が SIGKILL されない」ためである。** プラットフォーム側の既定の猶予は Docker が 10 秒、k8s が 30 秒しかない。実測: 既定（5 秒）のプロセスは停止に 5.09 秒しか使わない。かつての既定 30 秒では 30.09 秒必要で、k8s の既定猶予に 0.09 秒負けた。負けると行は `running` のまま残る。回収は `JobRescuer`（既定 1 時間。ロール分割構成では動かす常駐クライアントが無いので誰も回収しない）に委ねられる。**長い drain は、猶予を明示的に書いたデプロイだけが手に入れる**
- **ローリング更新の間は worker の DB コネクション budget が二重に乗る**（旧 Pod が drain のあいだプールを握り続ける。[§3](database.md) のロール別 budget）。猶予を数時間に取る構成では、その時間ぶん重なる
- プロセス側の待ちが猶予から導かれていることは `TestStopRiverForShutdown_DeadlineFollowsSoftStopTimeout` が固定している。猶予より長いことは `TestShutdownBudget_CoversTheSoftStop` が固定している。ここが固定値だと、猶予の内側で完走するはずのジョブを**プロセスが先に抜けることで**打ち切る（このときジョブの ctx は cancel すらされないので、上記の「誰も回収しない」形になる）

**バイナリを戻すときは argv からも消す。** `--soft-stop-timeout` を書いたマニフェストのままイメージを古い版に戻すと `unknown flag` で起動しない（実測）。Deployment なら CrashLoopBackOff、ScaledJob なら Job が即 failed になり、KEDA は滞留を見続けて Job を作り続ける。DB スキーマは触っていないので、argv さえ戻せば可逆である。

`docker-compose.yml` の `stop_grace_period` も同じ形で置いてある。**Docker の既定は 10 秒**なので、書かないと drain の途中で SIGKILL される。

**SIGTERM を 2 発撃てば強制終了できる。** drain 中に 2 発目を受けると、シグナルの登録は既に外れているので既定動作でプロセスが落ちる。数時間の猶予を設定した worker を手で止めるときの逃げ道である。**代償は SIGKILL と同じ**である --- River の completer は結果を書けないので、実行中だったジョブの行は `running` のまま残る。急いで止めたいときだけ使う。実測（猶予 600 秒・drain 中）: 2 発目で即座に exit 143。**登録を外さないと 2 発目は捨てられ**、同じ条件でプロセスは生き残ってジョブの完走まで走り続けた（手順は [runbook/testing.md](../runbook/testing.md)）。

1 件消化モードの graceful stop にも同じ猶予が効く（`--once` の Job もノード退避やローリング更新で SIGTERM を受ける）。ジョブ 1 件を消化したあとの正常終了の経路も同じで、**取りこぼしのジョブ（上記「既知の窓」）を掴んでいた場合、その完走を待つのは猶予までである**。ctx を見ないワーカーはそれでも止まらないので、最終的な上限が SIGKILL であることは変わらない。

### シングルトンロール: pg_advisory_lock リーダー選出

watcher はシングルトンロール。`pg_try_advisory_lock` による監督ループでリーダーを選出する（[データ層](../data.md) §2）。ruler / reconciler / record_sweep はジョブなので対象外。watcher の singleton 性はもはや「正しさ」の要件ではなく、「mirakc に N 本の SSE を張らない」という接続数の配慮に過ぎない。`processRecord` は冪等化済みである（[データ層](../data.md) §2、[録画エンジン](../recording.md) §3.3）:

**したがって `replicas: 1`（`deploy/k8s/site/watcher.yaml`）は既定であって要件ではない。** 2 レプリカにしても壊れない --- 負けた側は待機したまま mirakc に繋がない。

これは推測ではなく kind での実測である。受け入れ判定ハーネスの判定 4 が、2 レプリカのあいだ mirakc モックの `/events` の同時接続数を数えている（`deploy/k8s/e2e/checks/04-watcher-singleton.sh`）。最大値は 1 だった。

**watcher はサイトごとに 1**。advisory lock のキーはロール名だけでなく束縛サイトも含む（`watcher:<site>`）。多サイト構成で 2 サイトの watcher プロセスを立てると、両方が自分のサイトのロックを取得して両方の mirakc の SSE を購読する。同じロックキーだと片方が「role already held by another process」で待機に入り、負けた側の mirakc の SSE を誰も購読しなくなる（ログ上は正常に見えるので気付きにくい）。同一サイトで 2 プロセス立てた場合は従来どおり片方だけが動く。

1. ロールごとに goroutine を立て、`pg_try_advisory_lock` を定期試行（15s + jitter）
2. 取得したら child context でロール本体を起動
3. リーダー中はロック専用コネクションに定期 heartbeat（`SELECT 1`、10s 間隔）。失敗 = リーダーシップ喪失とみなし、ロールを停止して取得ループに戻る
4. セッション断で PG 側ロックが自動解放されるため、待機プロセスが次の poll で取得しフェイルオーバー成立

k8s の Lease API に依存しないため monolithic mode でも同じコードが動く（[データ層](../data.md) 参照）。フェイルオーバー遅延は最大 poll 間隔（〜15s）だが、いずれも定期 reconcile 前提のロールなので許容範囲。短時間の split-brain はシングルトンロールの仕事がすべて冪等（レベルトリガー + 冪等原則）であるため安全。

### healthz と readyz: liveness は依存を見ない、readiness は DB を見る

`/healthz` は **liveness probe 専用**。依存サービス（DB・mirakc）の状態は一切チェックせず、プロセスが HTTP を返せる限り常に 200 を返す。

- 不変条件 1「api ロールは mirakc に問い合わせない」により、mirakc チェックは構造的に不可
- ハイブリッド構成（[overview.md](../overview.md)）ではクラウド側 api から mirakc に到達できないのが正常状態
- liveness に依存チェックを入れると「依存ダウン → 全プロセス再起動ループ」になる（liveness probe の定番アンチパターン）
- DB は起動時に fail-fast で検証済み。ランタイムの DB 断は各ロールがリトライ / クラッシュで対処する（crash-only 原則）

mirakc の健全性は watcher が `observed_at` として DB に記録し、UI / アラートで可視化する。

**readiness は `/readyz` に分けてあり、こちらは DB への ping を見る。** api を Service の後ろに置く以上、DB に繋がっていない Pod にトラフィックが振られる窓が実際に開くため。liveness と readiness で見るものが逆になるのは、問いが違うからである --- liveness は「このプロセスは再起動すべきか」、readiness は「今トラフィックを受けられるか」。

- 見るのは DB への ping だけ。**mirakc への到達性は見ない**（不変条件 1。ハイブリッド構成ではクラウド側 api から mirakc に到達できないのが正常状態）
- **プールが無ければ 503**（fail-closed）。実バイナリではプールは常にある（起動時に fail-fast する）ので、これはルータの組み立てを誤ったときに 200 で隠さないための保険
- `pgxpool.Pool.Ping` はプールからコネクションを 1 本取る。したがってプールが飽和している間もタイムアウトして 503 になりうる（未測定。`Acquire` が待つことからの帰結）。**この応答から DB 断と輻輳は区別できない**
- 輻輳で 503 が続くと**全レプリカが同時に Endpoints から抜ける**（同じ DB を見ているのでレプリカを増やしても同時に落ちる）。probe の `failureThreshold` を上げて猶予を作ってはいるが、これは遅らせるだけである。この形の全断が起きるなら閾値ではなく DB 側（プール上限・`statement_timeout`。[§3](database.md)）を見る
- ハンドラ側にも応答の上限を持つ（probe の `timeoutSeconds` はそれより長くする。同値以下だと kubelet が先に諦めるので、ハンドラが 503 を返す経路が一度も通らない）
- Host allowlist の免除対象に入れる。免除し忘れると readiness が 400 で落ち、Pod は永久に Service の後ろに入らない

### DB 接続失敗: fail-fast + 明示ログ

DB 接続失敗はエラーを握り潰さず fail-fast + 明示ログとする（EPGStation#628 の教訓、crash-only 方針と整合）。

### ネットワーク FS ハング: ジョブストール検知 + 外部 liveness

ネットワーク FS のハング（EPGStation#721）は worker ジョブがストールしうる。対策:

- **ジョブのストール検知**: ingest のタイムアウトは総時間ではなく**ストール検知**（N 秒間無進捗で切断扱い）。総時間タイムアウトは遅い回線の正常な転送を殺す
- **外部 liveness**: k8s の liveness probe / systemd watchdog を推奨構成に含める

### 経緯と将来の構想

#### `internal/role`（`RunSingleton`）は watcher 専用になったが畳まない（issue #24 M2-20）

M2-17 / M2-18 で ruler / reconciler / record_sweep がジョブになり、利用箇所は
`cmd/rokuban/server.go` の 1 箇所だけになった。それでも独立したパッケージとして残す。
**「ソケットを connect し続ける」という形のロールが存在する限り必要な機構**だからである
（[overview.md](../overview.md) §ロール分類の基準）。リーダー選出の失敗モード
（heartbeat 喪失・split-brain・フェイルオーバー遅延）はテストで固定しておく価値が単独である。
呼び出し元が 1 つであることは、機構の複雑さが 1 つに減ったという成果であって、削除の根拠ではない。

#### 将来オプション: チャンク並列エンコード（未実装）

preemption 対策は上記の ScaledJob で十分であり、チャンク化の価値はクラウドバーストによる高速化（数時間のエンコードを worker N 台で数分の壁時計時間に短縮）に限られる。自宅の HW エンコード（QSV/VAAPI）は実時間の数倍で足りるため価値が薄く、クラウド構成専用のオプション。初期実装には含めない。

実装方針の見立て: 実ファイルは分割せず、各ジョブに (開始時刻, 長さ) を渡して 2 段 seek（`-ss` を `-i` の前後で併用）でフレーム精度の境界を出す。映像はチャンクごとに独立ジョブ（境界は強制 IDR）。音声は音声フレーム境界のズレによる接合ノイズを避けるため分割せず、全体を 1 パスで別エンコードして最後に mux（ISDB の番組途中の音声レイアウト切替の正規化もここに集約）。全チャンク完了後に concat demuxer でロスレス結合 + 検証の fan-in ジョブ。構造化エンコードプロファイル（[docs/storage.md](../storage.md)）とは独立な executor の戦略なので、プロファイル定義に手を入れず後付けできる。

#### 番号の対応

- ロールとキュー購読の構造的保証は issue #113（`--roles watcher` 構成で起動時検査が実態より広い安心を与える経路があった）。
- キュー名の site 修飾（`<論理名>_<site>`）と watcher の advisory lock キーの site 修飾（`watcher:<site>`）は issue #185 M4-13。`delete_reconcile` / `catalog_export` の `cleanup` キューへの配置も同じ issue。
- `--sites` フラグと `mirakcs:` レジストリは issue #183 M4-11。
- streamer のスケール設計（sticky を使わない / consistent hash / 既定 replicas=1 の可逆性）は issue #56。ライブの資源同定 `/api/sites/{site}/services/{serviceId}/...` は M3-1。id 空間を一覧 API に揃えるため `networks/{networkId}/services/{serviceId}` に変えたのは issue #217。ingress-nginx の `upstream-hash-by` で consistent hash の同じキャプチャがどう書けるかは M4-6 で実機確認するとされた（本文では未検証と記載）。
- 録画配信の URL に site を持たない決定（`recordings.id` は surrogate）は issue #31。
- watcher の `processRecord` 冪等化（singleton 性が「正しさ」の要件でなくなった）は M2-16。
