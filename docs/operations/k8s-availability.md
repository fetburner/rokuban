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
