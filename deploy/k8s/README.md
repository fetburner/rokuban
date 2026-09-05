# k8s マニフェスト（参照実装）

素の kustomize。Helm chart は持たない（判断の根拠は
[docs/operations/k8s.md](../../docs/operations/k8s.md) §マニフェストの配布形式）。

```
base/            中央（site 非依存）1 式:
                 config.yml（configMapGenerator の入力）/ migration Job /
                 api・notifier・streamer(録画配信) の Deployment + Service + PDB /
                 media の PVC / site 非依存キューの KEDA ScaledJob /
                 site 非依存ジョブの CronJob
site/            **サイト 1 組ぶん**: watcher Deployment /
                 site 束縛キュー（ingest・epg・reconciler・watcher）の ScaledJob /
                 site 束縛ジョブの CronJob
overlays/kind/   kind での動作確認用（base + site 1 組 + image の差し替え）
overlays/e2e/    受け入れ判定ハーネス用（base + site 2 組）
schemas/         kubeconform に渡す CRD スキーマ（KEDA の ScaledJob）
```

**base は複数サイトを知らない。** レジストリは常に 1 要素で、どの Pod も site に
束縛しない（`server` は `--sites=` の明示的な空、`enqueue` は `--site` を渡さない）。
サイトを増やす差分は「`mirakcs:` に
1 要素 + `site/` を 1 組生やす」だけになる（`overlays/e2e` が実例）。
判定は `workloads_test.go` の `TestBaseIsSiteIndependent`。

`site/` は site 名 `default` で書いてある。`base/config.yml` の `mirakcs:` も
1 要素目の site を `default` にしてあるので、**単一サイトの overlay は patch を
1 つも書かない**。

**まだ無いもの**:

- **入口（Ingress）**。当面は `kubectl port-forward svc/rokuban-api 40773` で触る
- **ライブ視聴の streamer。** streamer は 1 プロセスが N サイトを束縛でき、
  `live.enabled: true` に束縛サイト数の制約は無い（`cmd/rokuban/server.go`）。
  そのため「録画配信は中央（site 非依存、0 サイト束縛）、ライブはサイトごと」を
  同じ streamer ロールの別 Pod として書くこと自体はできる。実装していないのは
  overlay の切り方（`site/` の streamer にライブ用の設定・Service をどう足すか）
  がまだ無いだけで、issue に提起してある
- **Prometheus の Operator 連携**（ServiceMonitor / PodMonitor）。常駐の Pod には
  `prometheus.io/scrape` の annotation を付けてあるが、**ScaledJob が起こす Job の
  Pod は数秒で消えるので scrape が間に合わない**。ジョブ側の観測は
  `river_job` を読む滞留メトリクス（どのロールの `/metrics` にも出る）で行う
  （docs/operations.md §1）

## 前提

- **外部の PostgreSQL。** base は postgres を含まない。接続先は
  `base/config.yml` に素の値で書いてある（`postgres:5432` / user `rokuban` /
  database `rokuban`）。違う環境では overlay で patch する
- **Secret は自分で供給する。** base は Secret を出荷しない
  （下記「秘密の供給」）
- **namespace は overlay で決める。** base に `namespace:` は書いていないので、
  素で apply すると `kubectl` の現在の context に入る。`enableServiceLinks: false`
  の根拠が「同一 namespace の Service」なので、ここは暗黙にしない方がよい
- **`server.allowed_hosts` は `rokuban.local` で出荷**。Ingress で公開するなら
  実際のホスト名に patch する。空にすると Host 検証（DNS rebinding 対策）が
  無効になる

## 秘密の供給

base は Secret を出荷しない（理由は
[docs/operations/k8s.md](../../docs/operations/k8s.md) §マニフェストの配布形式）。
Secret が無い状態で apply すると、Pod は `CreateContainerConfigError`
（`secret "rokuban-secrets" not found`）で起動する前に止まる（実測）。

要求される形と供給の 2 通りは `base/secret.example.yaml`（apply されない参考）。

## 使い方

```sh
# 1. 秘密（初回だけ。または overlay の secretGenerator で供給する）
#    2 つ目のキーは KEDA の postgresql トリガが読む接続文字列（base/secret.example.yaml）。
kubectl create secret generic rokuban-secrets \
  --from-literal=POSTGRES_PASSWORD='...' \
  --from-literal=POSTGRES_CONNECTION_STRING='postgresql://rokuban:...@postgres.<ns>.svc.cluster.local:5432/rokuban?sslmode=disable'

# 2. マイグレーション → 中央 → サイトの順で通す
kubectl delete job rokuban-migrate --ignore-not-found
kustomize build base | kubectl apply -f -
kubectl wait --for=condition=complete job/rokuban-migrate --timeout=300s
kustomize build site | kubectl apply -f -   # 単一サイト（site 名 default）
kubectl rollout status deployment/rokuban-api
```

**`site/` は base と別に apply する。** 実際の運用では overlay
（`overlays/kind` が単一サイトの実例）が両方を `resources` に持つので、
apply は 1 回で済む。

**KEDA が要る。** `site/` と base の worker は KEDA の `ScaledJob` である。
CRD が無いクラスタに apply すると、その部分だけが
`no matches for kind "ScaledJob"` で失敗する（Deployment 側の apply は成功する
ので、**ジョブを誰も消化しない構成が黙って立つ**）。

**この順序に意味がある。**

- **`kubectl apply` は Job と Deployment を同時に起こす。** api の `/readyz` は
  DB への ping しか見ず、スキーマの有無は見ない。待ち合わせを挟まないと、
  migration の完了前に api がまだ無いテーブルを引く（未検証）
- **Job の `spec` は immutable。** Job が残っている状態（実行中、または完走後
  `ttlSecondsAfterFinished: 3600` の窓の内側）で apply すると、**Job だけが
  `field is immutable` で失敗し、Deployment は成功する。** 未マイグレートの DB に
  新しい api が乗る形になるので、先に `delete --ignore-not-found` する。
  TTL を 1 時間にしてあるのは、失敗したマイグレーションのログを朝まで残すため

## 覚えておくこと

- **config 本体は `base/config.yml`**（`configMapGenerator` の入力）。名前に内容
  ハッシュが付くので、編集すれば apply でそのまま rollout が起きる
  （実測: [docs/runbook/k8s.md](../../docs/runbook/k8s.md)）。
  `disableNameSuffixHash` を足さないこと
- **ハッシュ名の ConfigMap は古い世代が残る。** `kubectl apply` は消さないので、
  溜まったら `kubectl apply --prune`（ラベルセレクタ付き）か手で消す
- **env で渡すのは機微情報だけ**（`${POSTGRES_PASSWORD}` の 1 つ）。それ以外の
  環境差は overlay で patch を当てる
- **Pod ごとに違うのは argv だけ。** キューの絞り込みも argv（`--queues`）に
  寄せてある。config キー（`worker.queues`）でしか指定できないと、ScaledJob の
  数だけ ConfigMap が増えるため。**共有する config.yml に `worker.queues` を書くと、
  `--queues` を渡している worker が全部起動エラーになる**（両方指定は排他）
- **worker の Job は 1 件消化して自分で終了する**（`--once`）。ScaledJob は Job の
  自己終了を前提にした機構で、常駐する `--roles worker` を載せると
  「0 → 1 → 0」が成立しない。`--once` は成功・失敗を問わず exit 0 なので
  `backoffLimit: 0` / `restartPolicy: Never` と組ませる（リトライは River が持つ）
- **worker の `terminationGracePeriodSeconds` は drain を包む。**
  `preStop の sleep + 10s + --soft-stop-timeout + 10s` より長くする
  （[docs/operations.md](../../docs/operations.md) §5「Deployment 併用時」。
  判定は `workloads_test.go` の `TestWorkerGraceCoversTheSoftStop`）。**api の
  値をそのまま写さないこと** --- api は River クライアントを Start しないので、
  足し算の後ろ 2 項が落ちている
- **KEDA のトリガのクエリは物理キュー名で書く。** site 束縛キューは
  `<論理名>_<site>` に修飾される。論理名のまま書くと**誰も入れないキューを
  数え続けて永久にスケールしない**（判定は `TestScaledJobTriggersMatchTheirQueue`）

この 2 つ（generator にする / env を機微に限る）の判断の根拠は
[docs/operations/k8s.md](../../docs/operations/k8s.md) §マニフェストの配布形式。
- **パスワードに改行を含むものは使えない。前後の空白は落ちる**（`base/config.yml`
  の `password:` のコメントに実測付き）。記号（`'` `"` `\` `*` `{` `#` `: `）は通る
- **image はロールごとに差し替えられる。** overlay の `images:` で
  `ghcr.io/fetburner/rokuban` を置換する（`overlays/kind` が実例）。ffmpeg を要する
  役（worker / ライブ視聴を使う streamer）は別の image 名を書く（公式イメージは
  ffmpeg を含まない。`Dockerfile.full` でセルフビルドする）
- **api は 2 レプリカ + PDB（`minAvailable: 1`）で出荷**。PDB が無いと
  `kubectl drain` が両方同時に退去させるので、ノード 1 台の退避で全断する。
  レプリカを別ノードへ散らす指定は soft（`ScheduleAnyway`）にしてあるので、
  1 ノードのクラスタでも Pending にはならない
- **readiness は DB の輻輳でも落ちうる。** `failureThreshold: 6`（30s）にしてある
  のは、「DB が遅い」が「全レプリカ同時に Endpoints から抜ける = 全断」に化けるのを
  遅らせるため。レプリカを増やしても同じ DB を見ているので同時に落ちる。
  この形の全断が起きるなら、閾値ではなく DB 側（プール上限・statement_timeout。
  [docs/operations.md](../../docs/operations.md) §3）を見る

kind での動作確認手順は [docs/runbook/k8s.md](../../docs/runbook/k8s.md)。

## 検査

| 何を見るか | どこで回るか |
|---|---|
| YAML として組めるか | `kustomize build`（CI の manifests ジョブ） |
| 各オブジェクトが k8s のスキーマに合うか | `kubeconform`（同ジョブ） |
| オブジェクト間の参照・名前の解決・config.yml の中身 | `go test ./deploy/k8s/` |

3 つ目を別に持つ理由は、前 2 つが緑のまま通る事故があるため。存在しない ConfigMap を
`envFrom` に書く / ポート名の解決先を失う / config.yml のキーを typo する、の
いずれも実測で両方緑だった（一覧は `manifests_test.go` の冒頭コメント）。
壊れるのはクラスタに載せた後になる。

**CRD（KEDA の `ScaledJob`）は `schemas/` に置いたスキーマで検査する。**
`-ignore-missing-schemas` は未知の kind を黙って飛ばすので採らない。
出どころと「`-strict` を効かせるための加工」は [schemas/README.md](schemas/README.md)。
Go テスト側は CRD でも Pod テンプレートを見つけて検査する。`ScaledJob` の
`spec.jobTargetRef.template` は対応済みで、未知の kind は
`TestEveryWorkloadIsInspected` が落とす。

**argv とキュー名の噛み合わせは `workloads_test.go` が見る。** 見ているものは
次のとおり。

- `--queues` とトリガのクエリが同じ物理キュー名を指していること
- site 束縛キューの ScaledJob が `site/` に居ること
- ffmpeg を要る役だけが `Dockerfile.full` のイメージを指していること
- `rokuban enqueue` の全ジョブに CronJob があること
- overlay の JSON6902 patch が実際に site 名の位置を指していること

**キューやジョブを足した日に、マニフェスト側の書き忘れがここで落ちる。**
一覧は `internal/jobs.AllQueueNames()` と `cmd/rokuban/enqueue.go` を
権威にしてあり、テストに書き写していない。

## ここを使うハーネス

受け入れを機械判定するハーネスは [e2e/](e2e/README.md) にある
（`./deploy/k8s/e2e/run.sh`）。手順と「CI で回さない決定」は
[docs/runbook/k8s.md](../../docs/runbook/k8s.md) §受け入れ判定ハーネス。
ハーネスが使う 2 サイト構成の overlay は `overlays/e2e`。

この 1 式がハーネスに前提としていること:

- **postgres と Secret の供給はハーネス側の責務。** base はどちらも出荷しない
- **`kubectl delete job rokuban-migrate --ignore-not-found` → apply →
  `kubectl wait --for=condition=complete` の順序は必須**（上記「使い方」の理由）
- **パスワードの出どころは 1 つにする。** 突き合わせるものが無いので、ズレると
  症状は「api が起動しない」だけになり、どちらが正なのか分からない。
  `overlays/kind` は自分の secretGenerator に持っているが、ハーネスは
  postgres 側にも同じ値を渡す必要があるため `overlays/e2e` に Secret を
  置かず、`deploy/k8s/e2e/lib/env.sh` の 1 変数から run.sh が両方に配っている
