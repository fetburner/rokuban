# 設定

設定は **YAML 1 ファイル + パース前の `${VAR}` 展開**の一本とする。機微情報（DB 接続情報等）を公開可能な設定から分離するための機構で、EPGStation に対する [l3tnun/EPGStation#725](https://github.com/l3tnun/EPGStation/pull/725)（js-yaml の `!env` カスタムタグ）と同じモチベーション。

## 方針

設定の入口は「YAML 1 ファイル + `${VAR}` 展開」のみ。CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定し、設定値そのものはフラグで渡さない。

## 背景

設定ファイルには性質の異なる 2 種類の情報が同居する:

- **公開しても問題なく、頻繁に変更しうる情報** --- エンコードプロファイル、ルール評価の挙動、mirakc の URL など。Git 管理したい
- **機微情報** --- PostgreSQL の接続情報、webhook URL に含める認証トークンなど。平文で置きたくない

ファイル全体の暗号化は前者の運用性を殺すので、公開情報だけを含む config に機微情報を環境変数で補完する。

## 展開方式

Grafana Loki / Tempo の `-config.expand-env` と同じ、**YAML パース前の生テキスト展開**。実装は [drone/envsubst](https://github.com/drone/envsubst) で、`${VAR}` と `${VAR:-default}` のシェル風記法をサポートする。

```yaml
db:
  host: postgres
  port: 5432
  user: ${POSTGRES_USER}
  password: ${POSTGRES_PASSWORD}
  database: rokuban
mirakc:
  - url: http://mirakc.local:40772
encode_profiles:   # 公開して困らない設定はベタ書き
  - name: h264
    ...
```

js-yaml の `!env` に相当するカスタムタグ方式は Go では筋が悪い（yaml.v3 の `UnmarshalYAML` でフィールドごとに型を汚す）。テキスト展開なら設定 struct は純粋な型付き struct のまま保てる。

## 設計判断

1. **展開は常時オン**。Loki がオプトインなのは後方互換のためで、グリーンフィールドには不要。リテラルの `${` は `$$` でエスケープ（エンコードコマンド内等）
2. **`os.ExpandEnv` は使わない**。未定義変数を黙って空文字にするため、パスワード未設定のまま起動して謎の認証エラーになる。`LookupEnv` で未定義を検出し、**起動時に「未定義: POSTGRES_PASSWORD」と fail-fast** する（crash-only 方針と整合。[全体アーキテクチャ](overview.md) 参照）
3. **YAML ライブラリは goccy/go-yaml**。yaml.v3 は事実上メンテ停止（2026 時点）
4. **機微情報の供給はデプロイ側の慣習に委ねる**。アプリから見えるのは常に環境変数だけなので、機構 1 つで全環境をカバーできる:
   - 自宅 (Docker Compose): `.env` / `env_file:`
   - 自宅 (systemd): `EnvironmentFile=/etc/rokuban/secrets.env`
   - k8s: `Secret` → `envFrom` / `secretKeyRef`（config 本体は ConfigMap）

## やらないこと

- **env による config キーの自動オーバーライド**（viper/koanf の多層マージ） --- どの値がどこから来たか追いにくくなる割に、この規模では利得がない。設定の入口は「YAML 1 ファイル + `${VAR}` 展開」のみ
- **設定ファイルの分割・include 機構** --- 分離の必要があるのは機微情報だけで、それは env で足りる
- **CLI フラグで設定値を渡すこと** --- CLI フラグは `--config` のパスと `--all` / ロール選択などプロセスの起動形態に限定する
