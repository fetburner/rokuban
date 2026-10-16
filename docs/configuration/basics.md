> [configuration.md](../configuration.md) の方針・展開方式・規約・config と DB の境界・運用補助・やらないこと。索引から辿る。

## 方針

設定の入口は YAML 1 ファイルと環境変数の展開だけにする。CLI フラグは起動形態を表すものに限り、設定値そのものはフラグで渡さない。

## 展開方式

展開は YAML をパースする前の生テキストに対して常に行う。未定義の変数があれば、起動時に失敗させる。

Grafana Loki / Tempo の `-config.expand-env` と同じ生テキスト展開で、実装は [drone/envsubst](https://github.com/drone/envsubst) である。`${VAR}` と `${VAR:-default}` のシェル風記法を使える。

展開は常時オンにする。Loki がオプトインなのは後方互換のためで、グリーンフィールドには不要だからである。リテラルの `${` は `$$` でエスケープする（エンコードコマンド内など）。

標準ライブラリの `os.ExpandEnv` は使わない。未定義変数を気づかないうちに空文字にするため、パスワード未設定のまま起動して原因の分からない認証エラーになる。代わりに `LookupEnv` で未定義を検出し、**起動時に「未定義: POSTGRES_PASSWORD」と fail-fast する**（crash-only 方針と整合。[overview.md](../overview.md) 参照）。

YAML ライブラリは goccy/go-yaml を使う。yaml.v3 は事実上メンテナンスが止まっている（2026 時点）。

### 機微情報の供給

アプリから見える機微情報は常に環境変数だけなので、機構 1 つで全環境をカバーできる。環境ごとの供給元は次の通り。

- 自宅 (Docker Compose): `.env` / `env_file:`
- 自宅 (systemd): `EnvironmentFile=/etc/rokuban/secrets.env`
- k8s: `Secret` → `envFrom` / `secretKeyRef`（config 本体は ConfigMap）

## 規約

キーは snake_case で、期間は duration 文字列で書く。未知のキーは起動失敗にし、エラーは検出できるものを全件列挙する。

1. snake_case: キーは snake_case にする（Prometheus/Loki/mirakc に揃える）。JSON API 側の camelCase とは世界が違うので混在してよい
2. Go duration 文字列: 期間は `30s` / `5m` の `time.Duration` 文字列で書く（goccy/go-yaml のカスタム型 1 つで済む）
3. strict パース: 未知キーは起動失敗にする（goccy の strict モード）。`encode_profles` のような typo が気づかないうちにデフォルト動作になるのは、未定義環境変数の全件列挙 fail-fast と同じ思想で塞ぐべき穴である
4. エラーは全件列挙: 未定義環境変数・必須キー欠落ともに、検出できるものは全件列挙して起動失敗する（1 個ずつ直させない）

## config と DB の境界

**設定はホストやクラスタを建て直すと変わるデプロイ環境の性質、DB は運用中に UI から変えたい意思（ルール・予約・視聴履歴）を持つ。** エンコードプロファイルは設定側に置き、ルールからは名前で参照する。自由形式のコマンド文字列とフィルタグラフの直接指定は許さない。

エンコードプロファイルは config 側に落ちる。プロファイルの実体は「その環境の ffmpeg ビルドと HW (VAAPI/QSV/NVENC) で何ができるか」であり、`ffmpeg` パスと同じデプロイ属性である。DB のルールからは名前参照とし、ルール保存時に存在検証する（なければ 400）。自由形式の cmd 文字列は採らない（構造化フィールドから worker が引数を組み立てる）。HW エンコードも同じ構造化フィールドで表す（[live.md](live.md)「encode/live の HW エンコード」）。`-vf`（filtergraph）を直接書けるキーも作らない。filtergraph は第 2 のコマンド言語であり、`scale_vaapi=...,drawtext=...` のような式が書けた時点で cmd を別名で解禁したのと同じになるためである。

`encode.profiles[].rate` は録画実尺に対する締切の倍率で、既定は 4.0 である。encode ジョブ投入時に録画の終了時刻と開始時刻の差と rate から締切を解決して args に保存する。これにより、JobRescuer を動かす leader と投入元の設定が異なっても、実行ジョブに対して同じ締切を使える。

締切は `max(1 時間, 実尺 × rate)` である。実尺は終了時刻と開始時刻の差を使い、取れなければ番組長で代用する。それも取れない場合と締切のない旧 args は 12 時間となる。River はこの値をそのまま ffmpeg の締切に使うので、短く見積もると長尺が毎回締切超過で失敗する。誤った締切超過より、死亡検知が遅れる方を選ぶ。

<details><summary>根拠</summary>

- 実尺の取得元: `recordings.ended_at - started_at`、`ended_at - started_at`
- 実尺が取れないときの代用: `program_duration_ms`

</details>

## 運用補助

運用者が使う補助は、設定を検証するコマンドと、キーの網羅の基準になるサンプル設定ファイルの 2 つである。

- `rokuban config validate`: env 展開 + strict パース + 必須検証だけやって終了する。ConfigMap 更新前チェックや CI に置ける
- `config.example.yml`: コメント付きでリポジトリに置き、キーの網羅の基準とする（ドキュメント生成までは YAGNI）

## やらないこと

設定まわりで採らない仕組みは、env による自動上書き、設定ファイルの分割、ホットリロードである。CLI フラグでの設定値渡しも採らず、フラグは起動形態を表すものに限る。

- env による config キーの自動オーバーライド（viper/koanf の多層マージ）は採らない。どの値がどこから来たか追いにくくなる割に、この規模では利得がない
- 設定ファイルの分割・include 機構は採らない。分離の必要があるのは機微情報だけで、それは env で足りる
- CLI フラグで設定値を渡すことは採らない。CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定する。
  - 例外は「このプロセスが何を担当し、どう畳むか」を表すフラグに限る（`--sites` / `--queues` / `--once` / `--once-idle-timeout` / `--soft-stop-timeout`）。
  - `--queues` は config キー `worker.queues` と同じ値を取るが、その値自体がデプロイ時のパラメータであって環境の設定ではない。**両方指定は起動エラー**にして、値の出所が 2 つに分かれないようにしている。
  - `--once` とその `--once-idle-timeout` も argv 側に置く。前者は起動形態そのもので、後者はその形態にしか意味を持たない値なので、config キーにすると常駐プロセスにも見える場所に置くことになる。
  - `--soft-stop-timeout`（SIGTERM 後に実行中のジョブを打ち切るまでの猶予）も同じ扱いである。対になるのは k8s の `terminationGracePeriodSeconds` で、config には置かない。プラットフォーム側の SIGKILL が真の上限になる。その値はワークロードごとに桁で違う（数時間の encode と数秒の ruler）。共有 ConfigMap 1 個ではその違いを表現できず、同じ Pod spec に 2 つの数値を並べて書けることが対応関係を保つ唯一の手段になる。
  - k8s のロール分割では ConfigMap 1 個を全 Pod で共有し、Pod ごとの差分を argv に寄せる（[operations.md](../operations.md) §5）。これらが argv 側に無いと、ConfigMap が Pod セットの数だけ増える
- SIGHUP 等のホットリロードは採らない。設定変更は再起動で反映する。crash-only + level-triggered 設計なら再起動は無害で、リロードは「どの値がいつから有効か」という追跡困難性を別の形で持ち込むだけである
