# MySQL 構成の導入（OCI 向け）

MySQL 構成は OCI 向けの選択肢である。PostgreSQL が既定で、オンプレの導入は変えない。MySQL 構成のジョブエンジンは asynq（Redis）に限る。River は PostgreSQL 専用だからである。PostgreSQL 構成の導入は [setup.md](setup.md) を読む。

## 前提

MySQL 9.7（CI が回すバージョン）と Redis が要る。9.7 より新しいバージョンで動くかは未検証である。接続の分離レベル・sql_mode・照合順序・時刻帯は、起動時に接続ごとに強制し、違えば起動を止める。データベースは利用者が作る。migration は表だけを作る。

## 設定

設定は PostgreSQL と同じ db 節に書き、方言だけを切り替える。

```yaml
db:
  dialect: mysql
  host: mysql.example.internal
  port: 3306
  user: rokuban
  password: ${MYSQL_PASSWORD}
  database: rokuban
worker:
  engine: asynq
  redis_url: "${REDIS_URL}"
```

port は、書かなければ 3306 になる。engine を river のままにすると起動時に拒む。sslmode に disable 以外を書いた設定と、api_statement_timeout を書いた設定も、起動時に拒む。

## 手順

最初に migration を流す。migrate up は MySQL の埋め込み migration だけを適用し、River の表は作らない。次に storage adopt で置き場の目印を書く。最後に、置き場を指定して全ロールで起動する。

```sh
rokuban migrate up --config config.yml
rokuban storage adopt <置き場> --config config.yml
rokuban server --all --stores <置き場> --config config.yml
```

起動したら `/readyz` が 200 を返すことを確かめる。録画ルールを作り、`/api/events` に event rules が届き、予約が現れることを確かめる。

## DB ごとの既知の差

PostgreSQL 構成との差を、利用者が見る挙動・運用・接続の設定に分けて挙げる。判断の根拠がある差には、store-contracts.md の節を添える。

利用者が見る差は、次のとおり。正規表現は、PostgreSQL が POSIX ARE、MySQL が ICU を使う（[store-contracts.md](../data/store-contracts.md)「正規表現の構文」）。部分一致検索には、MySQL に pg_trgm 相当の索引が無い。性能は未計測である。チャプターの区間の重なりは、排他制約ではなく書き手のコードで確かめる（[store-contracts.md](../data/store-contracts.md)「CM 検出（試行と結果）とチャプター編集」）。

運用の差は、次のとおり。通知は通知表への書き込みで届け、notifier が約 1 秒ごとに読み直す。掃除は定期ジョブが 1 分ごとに行い、5 分より古い行を消す。転送帯域のリースとリーダー選出は GET_LOCK を使い、ロック名に DB 名を含める（[store-contracts.md](../data/store-contracts.md)「直列化」）。

接続の設定は、次のとおり。api ロール向けの statement timeout は未対応である。TLS も未対応で、接続は平文の TCP になる。`db.max_conns` の意味は PostgreSQL と同じで、未指定なら roles から同じ表で算出する。MySQL.Free の接続数の上限は未確認である（未検証）。

## 撤退の条件

OCI 構成を止めたら、MySQL 構成のサポートを落とす。MySQL を足した動機が、OCI の Always Free だけだからである。PostgreSQL と MySQL の間の移行はサポートしない。撤退するときに限り、MySQL から PostgreSQL への一方向の移行を作る（未実装）。
