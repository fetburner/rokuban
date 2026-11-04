> [runbook.md](../runbook.md) の一部。索引から辿る。

# CM ロゴの実機確認

実放送の録画を使い、ロゴ位置の解析・採用・再検出と、採用待ちで止まる録画を確認する。実施したサービスと確認範囲を記録する。

## 専用環境を用意する

テスト専用の PostgreSQL データベースと録画保存先を用意する。本番 DB や、他の Rokuban プロセスが使う DB は指定しない。別 deployment のクラスタで代用すると、api・worker・notifier を単一プロセスで起動したことも、そのプロセスが専用 DB に接続したことも確認できない。

`cm_detect.enabled` の値は録画の取り込み時に録画単位へ凍結される。無効の間に取り込んだ録画へ後から自動適用されないため、この手順では有効にした後に取り込んだ録画を確認する。JLSE の `logoframe`、`chapter_exe`、`join_logo_scp` が必要なので、`Dockerfile.full` から `rokuban:full` を作る。イメージ内では 3 ツールが `/usr/local/bin` にある。

```sh
docker build -f Dockerfile.full \
  --build-arg BASE=ghcr.io/fetburner/rokuban:latest \
  -t rokuban:full .
createdb -h <postgres-host> -p <postgres-port> -U <postgres-user> cm_detect_test
mkdir -p ./tmp/cm-detect-media
sudo chown 65534:65534 ./tmp/cm-detect-media
```

次の設定例を `cm-detect-config.yml` として保存し、`<...>` を実環境の値に置き換える。`db.database` は上で作った専用 DB、`mirakcs` は確認する実 mirakc、`storage.media_dir` は録画原本を置く専用ディレクトリにする。コンテナから PostgreSQL と mirakc の両方へ到達できるアドレスを使う。DB のパスワードをシェル履歴や確認記録へ出さない。

```yaml
server:
  listen: "0.0.0.0:40773"
  allowed_hosts: ["localhost", "127.0.0.1"]
db:
  host: "<postgres-host-reachable-from-container>"
  port: <postgres-port>
  user: "<postgres-user>"
  password: "<password>"
  database: cm_detect_test
  sslmode: disable
mirakcs:
  - site: <site-key>
    url: "http://<mirakc-host>:40772"
storage:
  stores:
    main: { kind: fs }
  originals:
    - store: main
  derived: main
  media_dir: /mnt/cm-detect-media
cm_detect:
  enabled: true
  binary_dir: /usr/local/bin
```

コンテナから PostgreSQL と mirakc に届く Docker network を選ぶ。イメージの entrypoint は `rokuban` なので、先にマイグレーションを適用し、続けて 3 ロールを同じプロセスで起動する。ホスト側の録画保存先はコンテナの実行ユーザー（uid/gid 65534）が書き込めるようにする。

```sh
docker run --rm --network <test-network> \
  -v "$PWD/cm-detect-config.yml:/config.yml:ro" \
  -v "$PWD/tmp/cm-detect-media:/mnt/cm-detect-media" \
  rokuban:full migrate up --config /config.yml

docker run --rm --network <test-network> \
  -v "$PWD/cm-detect-config.yml:/config.yml:ro" \
  -v "$PWD/tmp/cm-detect-media:/mnt/cm-detect-media" \
  rokuban:full storage adopt main --config /config.yml

docker run --rm --network <test-network> -p 40773:40773 \
  -v "$PWD/cm-detect-config.yml:/config.yml:ro" \
  -v "$PWD/tmp/cm-detect-media:/mnt/cm-detect-media" \
  rokuban:full server --roles api,worker,notifier --config /config.yml
```

このプロセスが動いている間、ブラウザーで `http://localhost:40773` を開く。

## 原本録画を用意する

確認したい局について、録画詳細で原本が有効な録画を少なくとも 1 件用意する。局画面のコマ選択は原本がある録画を使う。原本が無いと枠を編集できず、再検出の対象にもならない。

新たに録画する場合は、番組を予約し、原本の保持を「常に保持」にする。`until_encoded` の録画は、CM 検出など原本を使う処理が完了すると削除対象になり得る。テスト中は原本を保持し、削除 reconcile が原本を消す前に枠の操作と再検出を終える。

録画が完了したら、録画詳細に原本が表示されるまで待ってから次へ進む。番組を予約しただけでは、原本の取り込みが完了したことを確認できない。

## 候補を解析して採用する

ロゴの枠から候補を作り、確認してから再検出を依頼する。

現在のロゴを持つ局を選び、再検出に使える原本が残っていることを確かめる。`/cm-logos` の一覧から局を開き、コマを選び、CM ロゴが映る範囲を枠で囲んで「ロゴを解析」を押す。

候補解析の job が worker に渡るまでは表示が遅れることがある。画面は待機中と実行中を別表示にせず、「解析中」と表示する。候補が準備できると「候補あり」となり、現在のロゴと候補が並ぶ。解析に失敗したときは工程名に応じた表示を確認し、次の手順の採用へ進まない。

候補を確認し、「採用前に検出した録画 N 件（うち原本が残っている M 件）も検出し直す」のチェックをオンのまま「採用」を押す。採用後は局画面に検出待ち件数が表示される。数分から数十分かかることがあるため、`GET /api/cm-logos` の対象サービスの `pendingCount` が `0` になり、対象録画の詳細にある「記録」タブが「検出済み」になるまで待つ。待ち件数が残る間に確認を終えない。

## 採用待ちを確認する

有効なロゴが無く、コマ選択に使える原本が残る局を選ぶ。条件に合う局がなければ、有効なロゴが無い局で原本録画を作り、コマ選択に使える状態になるまで待つ。

局画面でコマを選び、枠を描いて「ロゴを解析」を押す。この操作で枠を保存して候補解析を依頼する。候補ができても「採用」は押さず、有効なロゴが無い状態のまま新しい番組を予約する。原本の保持は「常に保持」にする。録画が完了し、録画詳細に原本が表示されたら、対象録画の CM 検出状態が採用待ちになるまで待つ。

局画面の「影響する録画」で対象録画を確認し、録画詳細では「記録」タブを開く。どちらも「この局はロゴの採用待ちです。局の画面で候補を確かめて採用してください。」と表示され、「再検出」ボタンが出ないことを確かめる。未解決: 録画詳細の見出しメニューには、採用待ちでも「CM 検出をやり直す」が表示される。この表示は確認の合否に含めない。

## 後始末と結果の記録

確認した枠を消し、測定した範囲と結果を記録する。

局画面の高度な操作から「枠を消して自動に戻す」を実行する。採用待ちのまま枠を残すと、`until_encoded` の原本は削除されない。枠を消しても原本がすぐに削除されるとは限らないため、削除 reconcile 後に録画詳細で状態を確かめる。確認用 DB と録画保存先を他の環境へ接続しない。

実施した作業の記録には局名、site、network ID / service ID、受信方式、映像の coded 寸法と SAR、使った原本録画、試した段階、測定した結果を書く。成功した範囲と失敗・未実施の範囲を分け、1 局や 1 受信方式での結果を他の局へ一般化しない。

| 確認項目 | 記録する測定値 |
|---|---|
| 実行環境 | `rokuban:full`、実バイナリの起動ロール、専用 DB を使ったこと |
| 局と映像 | 局名、site、network ID / service ID、受信方式、coded 寸法、SAR |
| 候補採用と再検出 | 解析結果、採用操作、`pendingCount` の推移、完了した録画の状態 |
| 採用待ち | 有効ロゴが無いこと、原本があること、新しい録画の記録タブと「影響する録画」の表示、再検出ボタンの有無 |
| 範囲 | 通った段階、通らなかった段階、未実施の段階 |

<details><summary>根拠</summary>

- 設定キーと既定値: `config.example.yml` の `cm_detect`
- JLSE ツールのイメージ内配置: `Dockerfile.full`
- API・worker・notifier の起動フラグ: `cmd/rokuban/server.go`
- 採用待ち、再検出件数、原本保持の表示: `web/src/pages/cm-logos.tsx`、`web/src/lib/encode-settings.ts`
- 採用待ちの文言: `web/src/lib/cm-detect-stage.ts`
- 再検出の完了条件に使う `pendingCount`: `openapi.yaml` の `CMLogoState`

</details>
