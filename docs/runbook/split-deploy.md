> [runbook.md](../runbook.md) の一部。索引から辿る。

## デプロイの形を切り替える

単一ホストから k8s の参照実装へ移る判断と、戻す順序を示す。予約の正しさは [shadow.md](shadow.md)、視聴・寿命・移行は [cutover.md](cutover.md) で確認する。

### 移るかの判断

[保証する構成と機能の範囲](../overview.md#保証する構成と機能の範囲) を読み、ロールを分ける必要があるときに参照実装を選ぶ。[保証の境界](../overview.md#保証の境界) にある長期の分散運用は未検証である。下の出口基準に合格しても、長期の分散運用は保証の外のままである。

参照実装の構成は [配布物の索引](../../deploy/k8s/README.md) を引く。中央の `base/` とサイトの `site/` に加え、既存メディアを扱う置き場の `store/` も必要である。ハイブリッドの停止時の期待値は [自宅サーバーが落ちたとき](../overview.md#自宅サーバーが落ちたとき) を参照する。

### 引き継ぎの準備

同じリリースのバイナリで配置だけを変える。DB の移転やバージョンの巻き戻しを同時に行わず、現在の DB とメディアを復帰先からも使えるようにする。バックアップと復元の方針は [DB 運用](../operations/database.md) を参照する。

- DB は既存の接続先を引き継ぐ。compose の PostgreSQL を使い続けるなら、そのサービスとデータ volume を残し、k8s からの到達性を確認する
- メディアは空の PVC に置き換えない。既存ファイルと置き場の目印を維持し、同じ置き場名でマウントする。[置き場の目印](setup.md#置き場の目印) と [PVC マウントの所有権](../operations/k8s.md#pvc-マウントの所有権) を確認する
- config は既存の site 名・mirakc の接続先・置き場名・保存方針・エンコード設定を引き継ぐ。新しいネットワークでの DB・配信先・認証・Host 検証も確認する。キーは `config.example.yml`、共有設定は `deploy/k8s/base/config.yml`、秘密の渡し方は [秘密の供給](../../deploy/k8s/README.md#秘密の供給) を引く

停止前に復帰用 config とイメージを保存する。稼働中の録画・取り込み・変換を確認し、切替窓を決める。メディアのコピーが必要なら、メディアに書き込むプロセスを止めた後で最終同期する。

### 切替の順序

本番 DB に接続する前に、使い捨て環境で [kind の確認手順](k8s.md#k8s中央-1-式の確認手順) と [受け入れ判定ハーネス](k8s.md#受け入れ判定ハーネスkind--keda) を通す。本番データをハーネスへ渡さない。

1. 予約・ルールの変更など、api 経由の書き込みを止め、旧 `--all` を停止する。取り込み中の停止は [自前の compose の停止の猶予](setup.md#自前の-compose-の停止の猶予) に従う。DB とメディアは残す。旧プロセスが終了したことを確認する
2. 引き継ぐ DB とメディアのバックアップを確保し、新配置の設定・マウントを確認する。k8s 側は `worker.periodic_jobs: false` とする
3. [参照実装の使い方](../../deploy/k8s/README.md#使い方) に従い、マイグレーションの完了を待って中央・置き場・サイトを起動する。置き場の adopt Job の成功も確認する。apply だけで完了とは判断しない
4. [定期投入の監視](../operations/monitoring-scaled-jobs.md#cronjob-による定期投入) で CronJob の投入と worker の消化を確認する。下の出口基準を満たしてから Ingress や前段 nginx の転送先を新配置へ向け、api 経由の書き込みを再開する

切替そのものを既存 compose のデータで実施する手順は未検証である。kind の合格記録と運用先での確認記録を分けて残す。

### `--all` へ戻す

同じリリース・同じ DB・同じメディアで配置を戻す。DB のダウングレードはこの手順に含めない。

1. 予約・ルールの変更など、api 経由の書き込みを止める。自動再適用する仕組みも止める。下のコマンドで CronJob を停止し、api と watcher を停止する
2. 既に作られた投入 Job と worker Job の終了を待つ。下の SQL で滞留を確認する。notifier の常駐 River client は回収と再試行待ちの昇格に必要なので、この間は残す
3. available・running の行と実行中 Job がなくなったら、残りの常駐ロールを停止する。DB と Job を再確認してから ScaledJob を削除する。DB・PVC・メディアは削除しない
4. 復帰用 config を現在の DB・メディアへ向ける。`worker.periodic_jobs: true` を明示し、同じ site 名・置き場名と保存方針を引き継ぐ
5. [起動手順](setup.md#起動) で `--all` を起動する。定期ジョブの投入・完走と録画の再生を確認する。その後、前段 nginx などの転送先を復帰先へ戻し、api 経由の書き込みを再開する

#### 投入元の停止

停止対象は配布物のラベルで引く。対象クラスタと namespace を確認し、中央・サイト・置き場を別 namespace に置いた場合は、それぞれで実行する。

以下の `NS` には対象の namespace を設定する。

```sh
NS=<対象のnamespace>
kubectl -n "$NS" get cronjob -l app.kubernetes.io/name=rokuban
kubectl -n "$NS" get cronjob -l app.kubernetes.io/name=rokuban -o name |
  while IFS= read -r resource; do
    kubectl -n "$NS" patch "$resource" -p '{"spec":{"suspend":true}}'
  done
kubectl -n "$NS" get deployment -l 'app.kubernetes.io/name=rokuban,app.kubernetes.io/component in (api,watcher)'
kubectl -n "$NS" scale deployment \
  -l 'app.kubernetes.io/name=rokuban,app.kubernetes.io/component in (api,watcher)' --replicas=0
kubectl -n "$NS" get pods -l 'app.kubernetes.io/name=rokuban,app.kubernetes.io/component in (api,watcher)'
```

対象 Pod の終了を待つ。CronJob を停止しても、既に起動した投入 Job は終了まで待つ。

#### 滞留と実行中 Job の確認

投入元の停止後も、実行中の処理から後続ジョブが投入されることがある。DB の状態と Job の状態を繰り返し確認する。次の SQL は対象 DB に接続した SQL クライアントで実行する。

```sql
SELECT queue, kind, state, count(*)
FROM river_job
WHERE state IN ('available', 'running')
GROUP BY queue, kind, state
ORDER BY queue, kind, state;
```

```sh
kubectl -n "$NS" get jobs -l app.kubernetes.io/name=rokuban
kubectl -n "$NS" get pods -l app.kubernetes.io/name=rokuban
```

SQL が 0 行となり、Job の ACTIVE がすべて 0 になったことを確認する。失敗した Job があればログを調べる。完了済み Job の履歴は残っていてよい。

retryable・scheduled の行は残したまま進んでよい。同じ DB を引き継ぐ `--all` が、再試行待ちと snooze の待機中のジョブを処理する。

[スケーラの件数と回収](../operations/k8s.md#スケーラの件数と回収) は待機中と再試行待ちを数える。投入元を止めて available・running の行を消化してから削除する順序とし、KEDA の pause 機能や削除時の実行中 Job の扱いは前提にしない。

#### 残りの常駐ロールと ScaledJob の停止

実行中 Job がなくなってから常駐ロールを停止する。対象 Pod の終了を待ち、上の SQL と Job の確認を再実行してから ScaledJob を削除する。

待っている間に retryable が available へ昇格し、削除の直前に Job が起きる場合がある。削除後も下のコマンドで Job・Pod の残りを確認し、旧配置の消費者が終了するまで `--all` を起動しない。running のまま残った行の回収は、River の `RescueStuckJobsAfter` の公開契約に従う。設定の対応は [スケーラの件数と回収](../operations/k8s.md#スケーラの件数と回収) を参照する。

```sh
kubectl -n "$NS" get deployment -l app.kubernetes.io/name=rokuban
kubectl -n "$NS" scale deployment -l app.kubernetes.io/name=rokuban --replicas=0
kubectl -n "$NS" get jobs,pods -l app.kubernetes.io/name=rokuban
# SQL が 0 行、実行中 Job と Pod がないことを再確認してから削除する
kubectl -n "$NS" get scaledjob -l app.kubernetes.io/name=rokuban
kubectl -n "$NS" delete scaledjob -l app.kubernetes.io/name=rokuban
kubectl -n "$NS" get jobs,pods -l app.kubernetes.io/name=rokuban
```

`--all` を起動するだけでは、k8s 用の `periodic_jobs: false` が残った場合に定期投入が戻らない。イベントで取りこぼした処理を定期 reconcile が拾えなくなるため、復帰後も [停止を見分けるメトリクスと症状](../operations/monitoring-scaled-jobs.md#停止を見分けるメトリクスと症状) で確認する。実データでの往復切替は未検証である。

<details><summary>根拠</summary>

- 停止対象のラベル: `deploy/k8s/base/`・`site/`・`store/` の CronJob・Deployment・ScaledJob の `app.kubernetes.io/name: rokuban`。api と watcher は `app.kubernetes.io/component` で選ぶ
- running の回収契約: River `Config.RescueStuckJobsAfter` の公開 doc コメント。設定は `worker.rescue_stuck_jobs_after`（`config.example.yml`）
- スケーラの対象状態: 各 `worker-scaledjobs.yaml` の SQL（available / retryable）
- 定期投入の設定経路: `cmd/rokuban/server.go` の `SchedulePeriodicJobs`、`internal/config/config.go` の既定値は true
- k8s の明示値 false: `deploy/k8s/base/config.yml`。compose の全ロール起動: `docker-compose.yml` の `server --all`
- マイグレーションと置き場の初期化: `deploy/k8s/base/migrate-job.yaml`、`deploy/k8s/store/adopt-job.yaml`

</details>

### 出口基準チェックリスト

各行に実行日・対象リリース・結果を記録する。未確認の行があれば切替判断を保留する。

| # | 項目 | 合格の判定 |
|---|---|---|
| 1 | 参照実装の受け入れ | [ハーネス](k8s.md#受け入れ判定ハーネスkind--keda) の通常実行が終了コード 0。部分実行を代用しない |
| 2 | データの引き継ぎ | [切替の順序](#切替の順序) を終え、既存録画の再生と新しい録画の ingest・encode・再生を [cutover の確認](cutover.md#派生物の再生と-until_encoded項目-12) で確認できる |
| 3 | worker が 0 でも投入 | [監視の見方](../operations/monitoring-scaled-jobs.md#停止を見分けるメトリクスと症状) で、消化を止めた間も CronJob の周期に沿った投入を確認する。再開後は滞留が消化され、DB 由来の成功時刻が進む |
| 4 | ライブの配置 | [既定を 1 にする判断](../operations/k8s-streamer.md#既定を-1-にする根拠と増やす判定基準) に合わせて配置する。[ライブの確認](live.md) が通り、増設時も URL を変えずサービス同定子で振り分けられる |
| 5 | 監視・アラート | [監視の集約と初回パス](../operations/monitoring-scaled-jobs.md#アラート式の集約と初回パス) と [アラート設計](../operations/alerts.md) に照らして系列・集約式を確認する。プロセス内の最終パスゲージを ScaledJob の成功判定に使わない |
| 6 | 前段 | [nginx の受け入れ確認](nginx.md#受け入れ確認) が通る |
| 7 | 復帰の準備 | [復帰手順](#--all-へ戻す) に使う config の `worker.periodic_jobs` が true。停止対象の CronJob・Deployment・ScaledJob を同手順のラベルで取得でき、配布物の対象と一致する |
