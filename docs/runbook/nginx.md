> [runbook.md](../runbook.md) の一部。索引から辿る。

# nginx 前段の実機確認

`deploy/nginx/nginx.conf` は、Rokuban の前段に nginx を置く構成例である。
nginx は TLS と Basic 認証を担当する。
API と SSE は Rokuban へプロキシする。
録画ファイルのバイト転送だけは X-Accel-Redirect で nginx に委ねる。

## 構成の前提

以降の手順で共通して使う変数をまとめて定義し、確認用ディレクトリを作る。
同じシェルセッションで以下の手順を順に実行する前提とする。

```sh
DOMAIN=rokuban.example.com
TLS_ROOT=/tmp/rokuban-nginx-tls
TMP=/tmp/rokuban-nginx-check
MEDIA_DIR=/tmp/rokuban-nginx-media
mkdir -p "$TLS_ROOT/live/$DOMAIN" "$TLS_ROOT/archive/$DOMAIN" \
  "$TMP/certbot/.well-known/acme-challenge" "$MEDIA_DIR"
```

`MEDIA_DIR` は Rokuban が使う media ディレクトリと同じ場所にする。
日本語・空白・括弧に加えて `%` を含む fixture をここに置き、以下の確認で使う。

nginx と Rokuban が同じホストにいる場合、設定の upstream は
`127.0.0.1:40773` のままにする。
nginx をコンテナで動かす場合は、同じネットワークのサービス名か
`host.docker.internal:40773` に変更する。

Rokuban の設定は、公開ホスト名と X-Accel-Redirect の場所を一致させる。

```yaml
server:
  listen: "127.0.0.1:40773"
  allowed_hosts: [rokuban.example.com]
  trust_forwarded_host: false

storage:
  media_dir: /mnt/media
  accel_location: /_media/
```

nginx から次の 2 つが読めるようにする。

- `/mnt/media`: Rokuban の `storage.media_dir` と同じ録画ディレクトリ
- `/etc/letsencrypt` と `/etc/nginx/htpasswd/rokuban`: 証明書と認証ファイル

SPA は Go バイナリのビルド時に go:embed で埋め込まれるため、nginx へは渡さない。

`/_media/` は外部から直接読めない。
アプリの認可を通った X-Accel-Redirect だけが内部リダイレクトで到達する。

## TLS と認証の準備

本番では HTTP-01 または DNS-01 で Let's Encrypt の証明書を取得する。
HTTP-01 の webroot は `/var/www/certbot` を使う。

ローカル確認では、同じパスに自己署名証明書を置けばよい。
自己署名証明書は通信経路の確認用で、Let's Encrypt の代わりにはならない。

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout "$TLS_ROOT/live/$DOMAIN/privkey.pem" \
  -out "$TLS_ROOT/live/$DOMAIN/fullchain.pem" \
  -subj "/CN=$DOMAIN" \
  -addext "subjectAltName=DNS:$DOMAIN"
htpasswd -Bbn rokuban 'change-this-password' > /tmp/rokuban-nginx.htpasswd
```

実運用では、認証ファイルをパスワード管理下で作成する。
上のパスワードはローカル確認専用で、運用に流用しない。

Let's Encrypt の初回取得は、証明書がまだ無い状態で nginx を起動できない。
先に一時証明書で起動し、HTTP-01 の challenge が通ることを確認してから
`certbot certonly --webroot` を実行し、取得後に nginx を reload する。

```sh
certbot certonly --webroot -w /var/www/certbot -d rokuban.example.com
nginx -t
nginx -s reload
```

## 実機の起動

Rokuban を `127.0.0.1:40773` で起動する。
DB と Rokuban の起動は [setup.md](setup.md) に従う。
検証用録画の `recording_id` は、既存の録画から 1 件選ぶか、
日本語・空白・括弧に加えて `%` を含む相対パスの fixture を DB と `MEDIA_DIR` に用意する。
`%` は `internal/contentpath` のサニタイザを通り抜ける数少ない文字で、
エスケープの有無を分ける鍵になる。
以下の確認では、実際に使う録画 ID と相対パスをそれぞれ `$ID` と `$REL_PATH` に入れる。

nginx をホストで起動する場合、証明書・認証ファイル・media・certbot webroot を
`nginx.conf` が読む実パスへ配置してから `nginx -t` を通す。
**この経路は未検証。実測は次項の Docker 経路のみ。**

```sh
cd /path/to/rokuban
sudo install -d "/etc/letsencrypt/live/$DOMAIN" /etc/nginx/htpasswd /var/www/certbot /mnt/media
sudo cp "$TLS_ROOT/live/$DOMAIN/fullchain.pem" "$TLS_ROOT/live/$DOMAIN/privkey.pem" \
  "/etc/letsencrypt/live/$DOMAIN/"
sudo cp /tmp/rokuban-nginx.htpasswd /etc/nginx/htpasswd/rokuban
sudo cp -R "$MEDIA_DIR/." /mnt/media/
sudo nginx -t -c "$PWD/deploy/nginx/nginx.conf"
sudo nginx -c "$PWD/deploy/nginx/nginx.conf"
```

録画配信まで確認するには、Rokuban 自体の `storage.media_dir` も同じ
`/mnt/media` に向ける。

Docker で nginx だけを起動する場合は、設定を一時コピーして upstream を
`host.docker.internal` に置き換える。
アプリと nginx を同じ Compose に置く場合は、サービス名に置き換える。

```sh
sed 's/server 127.0.0.1:40773;/server host.docker.internal:40773;/' \
  deploy/nginx/nginx.conf > "$TMP/nginx.conf"
docker run --rm --name rokuban-nginx-check \
  --add-host=host.docker.internal:host-gateway \
  -p 8080:80 -p 8443:443 \
  -v "$TMP/nginx.conf:/etc/nginx/nginx.conf:ro" \
  -v "$MEDIA_DIR:/mnt/media:ro" \
  -v "$TLS_ROOT:/etc/letsencrypt:ro" \
  -v /tmp/rokuban-nginx.htpasswd:/etc/nginx/htpasswd/rokuban:ro \
  -v "$TMP/certbot:/var/www/certbot:ro" \
  nginx:1.29-alpine
```

コンテナの `nginx -t` は起動時に実行される。

ローカルの名前解決は `/etc/hosts` に追加する。

```text
127.0.0.1 rokuban.example.com
```

## 受け入れ確認

以下の `$URL` は `https://rokuban.example.com:8443`、
`$AUTH` は `rokuban:change-this-password` とする。
自己署名証明書のため curl には `-k` を付ける。

### TLS・Basic 認証・SPA

認証なしの API・録画・SSE・SPA はすべて `401` になることを確認する。

```sh
curl -sk -o /dev/null -w '%{http_code}\n' "$URL/api/version"
curl -sk -o /dev/null -w '%{http_code}\n' "$URL/api/recordings/$ID/file"
curl -sk -o /dev/null -w '%{http_code}\n' "$URL/api/events"
curl -sk -o /dev/null -w '%{http_code}\n' "$URL/recordings/123"
```

認証付きの API と SPA は `200` になる。
`/recordings/123` の本文に `<div id="root">` があれば、
アプリの SPA フォールバックが nginx 経由で通っている。

```sh
curl -sk -u "$AUTH" "$URL/api/version"
curl -sk -u "$AUTH" "$URL/recordings/123" | grep '<div id="root">'
```

HTTP の通常パスは HTTPS へ `308` で移り、challenge だけはリダイレクトされない。

```sh
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' \
  -H 'Host: rokuban.example.com' http://127.0.0.1:8080/recordings/123
printf challenge > "$TMP/certbot/.well-known/acme-challenge/test-token"
curl -sS -H 'Host: rokuban.example.com' \
  http://127.0.0.1:8080/.well-known/acme-challenge/test-token
```

### 録画の Range と X-Accel-Redirect

日本語・空白・括弧を含む fixture を使い、認証付き HEAD が `200`、
`Accept-Ranges: bytes` と正しい `Content-Length` を返すことを確認する。

```sh
curl -skI -u "$AUTH" "$URL/api/recordings/$ID/file"
```

次に先頭 188 バイトだけを取得する。
応答は `206`、`Content-Range: bytes 0-187/...`、本文 188 バイトになる。

```sh
curl -sk -u "$AUTH" -D "$TMP/range.headers" \
  -H 'Range: bytes=0-187' -o "$TMP/range.body" \
  "$URL/api/recordings/$ID/file"
wc -c "$TMP/range.body"
grep -E 'HTTP/|Accept-Ranges:|Content-Range:' "$TMP/range.headers"
```

アプリを直接叩くと、`storage.accel_location` が有効な場合は本文が空で、
`X-Accel-Redirect` に URL エスケープ済みの相対パスが出る。
nginx 経由ではそのヘッダーが内部リダイレクトに消費され、本文が返る。
この 2 つを同じ fixture で確認する。

```sh
curl -sS -H 'Host: rokuban.example.com' \
  -D "$TMP/app.headers" -o "$TMP/app.body" \
  "http://127.0.0.1:40773/api/recordings/$ID/file"
grep -E 'HTTP/|Content-Type:|X-Accel-Redirect:' "$TMP/app.headers"
wc -c "$TMP/app.body"
```

直接 `/_media/` を叩いた場合は、認証の有無に関わらず `404` になる。
これは `internal` の防壁が外れていないことの確認である。

```sh
curl -sk -u "$AUTH" -o /dev/null -w '%{http_code}\n' \
  "$URL/_media/$REL_PATH"
```

### `accel_location` 無効時の経路

Rokuban の `storage.accel_location` を空にして再起動する。
アプリ直接の応答に `X-Accel-Redirect` が無く、本文が返ることを確認する。
同じ Range 要求を nginx 経由でも行い、`206` と正しい範囲が返ることを確認する。
確認後は `/_media/` の設定を戻して再起動する。

### SSE の長時間接続

認証付きの SSE はすぐに `retry: 3000` を返し、25 秒間隔で `: ping` を繰り返す。
Rokuban 自身も `X-Accel-Buffering: no` を返すが、nginx 側の
`proxy_buffering off` も明示して、upstream の実装や経路を変えても SSE を溜めない
ことを構成として固定する。
`nginx.conf` の `proxy_read_timeout` は 90 秒なので、これを跨がない確認では
ping の有無で結果が変わらない。
`--max-time 120` を使い、終了コードが `28` になることと、25 秒間隔の ping が
複数回届いていることを両方確認する。
認証なしの SSE は `401` で、ストリームを作らない。

```sh
curl -skN --max-time 120 -u "$AUTH" \
  "$URL/api/events" > "$TMP/events.txt"; test $? -eq 28
grep -F 'retry: 3000' "$TMP/events.txt"
test "$(grep -c ': ping' "$TMP/events.txt")" -ge 4
```

### 壊して確認する項目

正常系だけでは nginx の設定漏れを見逃す。
一度に 1 つだけ変更し、確認後に元へ戻す。

- `auth_basic` を一時的に削除し、認証なしの API が `200` になることを確認する。
  その後に戻して `401` を確認する。
- `proxy_buffering off` を一時的に削除し、SSE の挙動を確認する。Rokuban の
  `X-Accel-Buffering: no` が残っている限り、このアプリでは削除だけで遅延が
  再現しないため、nginx 固有の差を確認するときは検証用 upstream でそのヘッダーを
  返さないようにする（または一時的に `proxy_ignore_headers X-Accel-Buffering;`
  を加える）。その後に元へ戻して、25 秒以内に `: ping` が届くことを確認する。
- `alias /mnt/media/` を存在しないディレクトリに変え、Range が `404` になることを確認する。
  その後に戻して日本語ファイル名が `206` になることを確認する。
- `internal` を一時的に削除し、直接 `/_media/` が読める状態になることを確認する。
  その後に戻して直接アクセスが `404` になることを確認する。
- `proxy_read_timeout` を ping 間隔より短い `10s` に一時変更する。ping と read
  timeout の関係が壊れる唯一の経路で、`--max-time 120` の前に接続が切れることを
  確認する。その後に戻して `--max-time 120` まで接続が保たれることを確認する。
- `internal/streamer/streamer.go` の `accelURI` から `url.PathEscape` を一時的に
  外す（`segments[i] = seg` に変える）。`%` を含む `$REL_PATH` の配信が失敗する
  （`404` か `500` のいずれか）ことを確認する。日本語・空白・括弧はエスケープを
  外しても通ってしまう可能性があり、そちらは実測で確定させる項目とする。
  Go 側の unit test はヘッダーがエスケープ済みであることまでしか主張できない。
  nginx がそれを必要とするかは、実機の alias 解決でしか分からない。
  その後に `url.PathEscape` を戻し、同じ fixture が `206` で返ることを確認する。

設定を戻すたびに `nginx -t` を通し、reload 後に正常系を 1 回繰り返す。
Go 側を戻したときは `go build` し直してから確認する。
