# 設定

設定は **YAML 1 ファイル + パース前の `${VAR}` 展開**の一本とする。

## 方針

設定の入口は「YAML 1 ファイル + `${VAR}` 展開」のみ。CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定し、設定値そのものはフラグで渡さない。

## 展開方式

Grafana Loki / Tempo の `-config.expand-env` と同じ、**YAML パース前の生テキスト展開**。実装は [drone/envsubst](https://github.com/drone/envsubst) で、`${VAR}` と `${VAR:-default}` のシェル風記法をサポートする。

- **展開は常時オン**。Loki がオプトインなのは後方互換のためで、グリーンフィールドには不要。リテラルの `${` は `$$` でエスケープ（エンコードコマンド内等）
- **`os.ExpandEnv` は使わない**。未定義変数を黙って空文字にするため、パスワード未設定のまま起動して謎の認証エラーになる。`LookupEnv` で未定義を検出し、**起動時に「未定義: POSTGRES_PASSWORD」と fail-fast** する（crash-only 方針と整合。[overview.md](overview.md) 参照）
- **YAML ライブラリは goccy/go-yaml**。yaml.v3 は事実上メンテ停止（2026 時点）

### 機微情報の供給

アプリから見えるのは常に環境変数だけなので、機構 1 つで全環境をカバーできる:

- 自宅 (Docker Compose): `.env` / `env_file:`
- 自宅 (systemd): `EnvironmentFile=/etc/rokuban/secrets.env`
- k8s: `Secret` → `envFrom` / `secretKeyRef`（config 本体は ConfigMap）

## スキーマ構造

### 関心事別にセクションを切る

ロール別ではなく関心事別にトップレベルを構成する。ロール別 Deployment でも config は同一ファイルを共有するため、ロールで切ると streamer と worker が同じ ffmpeg 設定を二重に持つことになる。各ロールは自分に関係するセクションだけを読む。

### リファレンス

```yaml
server:                          # HTTP を持つロール (api, streamer, notifier) 共通
  listen: ":40773"               # mirakc の 40772 の隣
  allowed_hosts: [rokuban.local] # Host ヘッダー allowlist。localhost 系は常に許可

db:                              # 必須
  host: postgres
  port: 5432
  user: ${POSTGRES_USER}
  password: ${POSTGRES_PASSWORD}
  database: rokuban
  sslmode: disable

mirakc:                          # 必須
  url: http://mirakc.local:40772

storage:
  media_dir: /mnt/media          # 必須。アーカイブ層 (S3-via-CSI 可)
  scratch_dir: /var/tmp/rokuban  # ローカルスクラッチ
  accel_location: ""             # X-Accel-Redirect の internal location。空なら Go が直接配る

ingest:
  concurrency: 2                 # mirakc サイトあたり 1-2

epg:
  sync_interval: 10m             # mirakc から EPG を全量取得する間隔
  retention_grace: 24h           # 放送終了からこの時間で番組を刈り取る。
                                 # ruler の GC（予約と番組単位の意図）も同じ猶予を使う

worker:
  periodic_jobs: true            # プロセス内で定期ジョブ（epg_sync / ruler_pass / reconcile_pass / record_sweep）を投入するか。
                                 # k8s では false にし、CronJob から rokuban enqueue で投入する
                                 # （River の PeriodicJobs はリーダーだけが投入するため、
                                 #  KEDA で 0 にスケールすると誰も投入しなくなる。data.md §2）
  queues: []                     # 引くキューを絞る。空なら全部。
                                 # ロールを増やさずに「ruler / reconciler だけ別 Pod」を実現するための knob

encode:
  ffmpeg: ffmpeg                 # 既定は PATH 検索
  ffprobe: ffprobe
  profiles:
    - name: h264
      # 構造化プロファイル。詳細は M3 で確定

log:
  level: info
  format: json                   # json | text
```

### 必須キー

最小 3 つ: `db`（資格情報）・`mirakc.url`・`storage.media_dir`。残りは全部デフォルトを持ち、最小構成は 10 行程度に収まる。

### mirakc は単一オブジェクト

`mirakc` はリストではなく単一オブジェクトとする。複数 mirakc を許すと programId のスコープが mirakc 単位になり、予約・EPG 射影・ingest の全スキーマに「どの mirakc か」が波及する。チューナー集約は mirakc 自身のリモートチューナー機能で賄えるため、Rokuban 側は 1 エンドポイントで足りる。

多拠点が現実化した場合は Rokuban 側で `mirakcs:` リストを追加し、`mirakc:` 単一形式を要素数 1 の糖衣にすることで互換に拡張できる。ハブ mirakc への集約は採らない（WAN リアルタイム依存 = 録画中の回線瞬断が録画欠損に直結するため）。

### db は構造化フィールド

`url:` 1 本の DSN ではなく構造化フィールドを採る。パスワードの URL エスケープ事故を避けられる。

## 規約

1. **snake_case**: キーは snake_case（Prometheus/Loki/mirakc に揃える）。JSON API 側の camelCase とは世界が違うので混在してよい
2. **Go duration 文字列**: 期間は `30s` / `5m` の `time.Duration` 文字列（goccy/go-yaml のカスタム型 1 つで済む）
3. **strict パース**: 未知キーは起動失敗（goccy の strict モード）。`encode_profles` のような typo が黙ってデフォルト動作になるのは、未定義環境変数の全件列挙 fail-fast と同じ思想で塞ぐべき穴
4. **エラーは全件列挙**: 未定義環境変数・必須キー欠落ともに、検出できるものは全件列挙して起動失敗する（1 個ずつ直させない）

## config と DB の境界

**config = デプロイ環境の性質**（そのホスト/クラスタを再構築すると変わるもの）。**DB = 運用中に UI から変えたい意思**（ルール・予約・視聴履歴）。

この原則により**エンコードプロファイルは config 側**に落ちる。プロファイルの実体は「その環境の ffmpeg ビルドと HW (VAAPI/QSV/NVENC) で何ができるか」であり、`ffmpeg` パスと同じデプロイ属性。DB のルールからは名前参照とし、ルール保存時に存在検証（なければ警告）。

## 運用補助

- **`rokuban config validate`**: env 展開 + strict パース + 必須検証だけやって exit。ConfigMap 更新前チェックや CI に置ける
- **`config.example.yml`**: コメント付きでリポジトリに置き、スキーマのリファレンスとする（ドキュメント生成までは YAGNI）

## やらないこと

- **env による config キーの自動オーバーライド**（viper/koanf の多層マージ）: どの値がどこから来たか追いにくくなる割に、この規模では利得がない
- **設定ファイルの分割・include 機構**: 分離の必要があるのは機微情報だけで、それは env で足りる
- **CLI フラグで設定値を渡すこと**: CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定する
- **SIGHUP 等のホットリロード**: 設定変更は再起動。crash-only + level-triggered 設計なら再起動は無害で、リロードは「どの値がいつから有効か」という追跡困難性を別の形で持ち込むだけ
