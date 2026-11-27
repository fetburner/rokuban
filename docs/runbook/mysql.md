# MySQL 構成の導入（OCI 向け）

MySQL 構成は OCI 向けの選択肢である。PostgreSQL が既定で、オンプレの導入は変えない。MySQL 構成のジョブエンジンは asynq（Redis）に限る。River は PostgreSQL 専用だからである。PostgreSQL 構成の導入は [setup.md](setup.md) を読む。

## 前提

MySQL 9.x と Redis が要る。接続の分離レベル・sql_mode・照合順序・時刻帯は、起動時に接続ごとに強制し、違えば起動を止める。データベースは利用者が作る。migration は表だけを作る。

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

port は、書かなければ 3306 になる。engine を river のままにすると起動時に拒む。sslmode は disable 以外を、api_statement_timeout は書くことを、起動時に拒む。

## 手順

最初に migration を流す。migrate up は MySQL の埋め込み migration だけを適用し、River の表は作らない。次に storage adopt で置き場の目印を書く。最後に、置き場を指定して全ロールで起動する。

起動したら `/readyz` が 200 を返すことを確かめる。録画ルールを作り、`/api/events` に event rules が届き、予約が現れることを確かめる。

## DB ごとの既知の差

MySQL には LISTEN と NOTIFY が無い。通知は通知表に書かれ、notifier が約 1 秒ごとに読み直す。掃除は定期ジョブが 1 分ごとに行い、5 分より古い行を消す。

ジョブは asynq が実行する。River の回収に当たる仕組みは、MySQL 構成には無い。api ロール向けの statement timeout は未対応である。TLS も未対応で、接続は平文の TCP になる。

リーダー選出は GET_LOCK を使い、名前は固定長のハッシュにする。`db.max_conns` の意味は PostgreSQL と同じで、未指定なら roles から同じ表で算出する。MySQL.Free の接続数の上限は未確認である（未検証）。

## 撤退の条件

OCI 構成を止めたら、MySQL 構成のサポートを落とす。DB 間の移行は作らない。撤退するときに、MySQL から PostgreSQL への一方向の移行を作る（未実装）。
