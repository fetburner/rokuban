## 前提

- mirakc が動いていて `GET /api/version` が返る
- mirakc の `recording.basedir` と `recording.records-dir` が設定されている
  （どちらも未設定だと `/api/recording/*` が 404 になる）
- mirakc のイメージに **`cat` と `dd` が含まれている**

最後の項目は見落としやすい。`FROM scratch` + magicpak のように必要なバイナリだけを
含むイメージだと、`GET /api/recording/records/{id}/stream` が **500** を返して
ingest が完走しない。HEAD はファイルのメタデータだけで応答できるので**成功する**ため、
「HEAD は通るのに GET が 500」という切り分けにくい形で出る。

## 起動

```sh
cp .env.example .env
$EDITOR .env          # MIRAKC_URL と POSTGRES_PASSWORD は必須
docker compose up -d
docker compose logs -f rokuban
```

`http://localhost:40773` で UI が開く。

正常なら起動から 30 秒ほどで EPG の全量同期が 1 回走る。

```
INFO epg sync complete services_projected=19 programs_fetched=7139 programs_projected=2680 ...
```

`programs_projected` が 0 のままなら mirakc 側の EPG がまだ空か、
`update-schedules` ジョブが一度も走っていない（既定は 1 日 2 回）。

同じ頃にチューナーの射影（M2-10）も 1 回走る。容量超過の判定はこの射影だけを
読むので、これが出ていない間は**超過が一切報告されない**（後述の「沈黙は保証ではない」）。

```
INFO tuner sync complete site=default tuners_fetched=4 tuners_projected=4 stale=0 capacity_overages=0
```

## 定期ジョブの周期とヒント

M2 の待ち時間はほぼこの表で決まる。**定期パスが真実**で、ヒントは投入を早めるだけ
（落としても次の定期パスが拾う。不変条件 5）。

| ジョブ | 既定周期 | 設定キー | 前倒しするヒント |
|---|---|---|---|
| `epg_sync` | 10 分 | `epg.sync_interval` | なし |
| `tuner_sync` | 10 分 | **なし**（コード固定。運用者が触る理由がないため） | なし |
| `ruler_pass` | 10 分 | なし | ルールの作成 / 更新 / 削除（api が**同一トランザクションで**投入）・`epg_sync` の完了 |
| `reconcile_pass` | 30 秒 | なし | 予約の作成 / 取消（同一トランザクション）・`ruler_pass` の完了 |
| `record_sweep` | 5 分 | なし | なし（SSE 再接続を契機にする案は M2-18 で見送り） |

- 5 つとも `RunOnStart` なので**プロセス起動直後に 1 回走る**
- ヒントは `UniqueOpts{ByArgs, ByState}` で定期実行に合流する。ルールや予約を連続で
  編集してもパスは 1 回で足りる
- `worker.periodic_jobs: false`（k8s 構成）ではどのジョブも自動投入されない。
  CronJob から `rokuban enqueue` で投入する

手で即時実行できる。ジョブ名はハイフン区切り（`epg-sync` / `tuner-sync` /
`ruler-pass` / `reconcile-pass` / `record-sweep` / `catalog-export`）。
site 束縛ジョブは多サイトでは `--site` が必須。`catalog-export` だけ site 非依存
で `--site` を付けない（[operations.md](../operations.md) §ジョブ化されたループの監視）。

```sh
docker compose exec rokuban rokuban enqueue ruler-pass --config /config.yml
# inserted job "ruler-pass" (id=42) for site "default"
# 既に待機中なら投入されず、終了コードは 0 のまま:
#   job "ruler-pass" already pending for site "default", not inserted

# catalog-export は --site なし（全体で 1 本）
docker compose exec rokuban rokuban enqueue catalog-export --config /config.yml
# inserted job "catalog-export" (id=43)
```

**ルールを作ってから録画が始まるまでに待つものは 3 段ある。**

1. **番組が EPG プロジェクションに入る** — `epg_sync`（最大 10 分）。その手前に
   mirakc の `update-schedules`（既定 1 日 2 回）があるので、**放送直前の番組は
   そもそも射影に無い**ことがある。ruler は射影しか見ない（不変条件 1）
2. **ruler が `reservations.base` を導出する** — ルール書き込みと同一トランザクションで
   ヒントが入るので通常は即座。ヒントが落ちれば最大 10 分
3. **reconciler が mirakc に schedule を作る** — `ruler_pass` の完了がヒントなので
   続けて走る。落ちれば最大 30 秒

