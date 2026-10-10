> [runbook.md](../runbook.md) の一部。索引から辿る。

## EPGStation から Rokuban へ主運用を切り替える

EPGStation を止めて Rokuban を主運用にする手順と、その判断に使う出口基準。
並走（[shadow.md](shadow.md)）が確かめるのは**予約の正しさ**である。
この文書が確かめるのは**視聴・寿命・移行**で、並走の出口基準を満たした後に使う。

コマンドはリポジトリの `docker-compose.yml` で動かしている前提で書く。
`docker compose exec rokuban rokuban ...` はコンテナ内の CLI を呼ぶ。
SQL は `docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"'` で流す。

### 出口基準チェックリスト

切替は全項目を満たしてから行う。各項目の確かめ方は下の節にある。

| # | 項目 | 合格の判定 |
|---|---|---|
| 1 | 予約差分 | `rokuban shadow-diff` が終了コード 0（[shadow.md](shadow.md)） |
| 2 | 派生物の再生 | Rokuban で録った番組の encoded 版をブラウザで最後まで再生できる |
| 3 | `until_encoded` | エンコード中は原本が `active` のまま。完成後に原本だけが `deleted` になり、encoded は再生できる |
| 4 | ごみ箱の復元 | 削除 → 復元の前後でファイルの inode と mtime が変わらない |
| 5 | ブレーカー | `GET /api/breakers` が空。`rokuban_circuit_breaker_tripped` が全系列 0 |
| 6 | catalog | `rokuban catalog verify` が終了コード 0。練習 DB への rescue が通る |
| 7 | webhook（使う場合） | 実際の受け口が `recording.finished` を 1 件受け取る |

切替後に次の 2 項目を確かめる。どちらかが落ちたらロールバックを検討する。

| # | 項目 | 合格の判定 |
|---|---|---|
| 8 | ライブラリの欠け | EPGStation の録画件数と取り込んだ件数の差を全件説明できる。マウント配下の `orphan_files` が 0 件 |
| 9 | 実体無し | `rokuban_media_assets_missing` が全系列 0 |

### エンコードプロファイル

プロファイルの形は [config.example.yml](../../config.example.yml) の `encode.profiles` が権威である。
software（`libx264`）・VAAPI（`h264_vaapi`）・カット版の例がそこにある。
キーの判断は [configuration.md](../configuration.md) の「encode/live の HW エンコード」にある。

HW エンコードで踏みやすいもの:

- **device の存在は起動時に検査しない**。compose なら `rokuban` サービスに
  `devices: ["/dev/dri:/dev/dri"]` を足す。足し忘れは起動エラーではなく encode ジョブの失敗として出る
- **`rokuban:full` の ffmpeg は apt 版**で、`h264_vaapi` / `hevc_vaapi` を持つ。
  ARIB 字幕（`subtitles: webvtt`）を使うなら libaribcaption 入りの ffmpeg に差し替える。
  差し替えると apt 版のエンコーダを失う（`Dockerfile.full` の冒頭コメント）
- エンコーダの有無は `docker compose exec rokuban ffmpeg -hide_banner -encoders | grep vaapi` で見る
- 設定から消したプロファイルを参照するルールは `unknown encode profile` で失敗する（[troubleshooting.md](troubleshooting.md)）

**取り込んだルールはエンコードしない**。`import epgstation --rules` は
`keepOriginal: always`・プロファイル空でルールを作る（EPGStation のエンコード設定は写さない）。
UI のルール編集でプロファイルを選んでから、必要なら `until_encoded` にする。
プロファイルが空のままでは `until_encoded` を選べない。
`--rules` を再実行しても、Rokuban 側で設定したプロファイルと保持ポリシーは上書きされない。

### 派生物の再生と `until_encoded`（項目 2・3）

1. 試験用のルールを 1 本作り、プロファイルを 1 つと `keepOriginal: until_encoded` を設定する。
   短い番組に当たる条件にする
2. 録画が終わったら録画 ID を控え、アセットの状態を見る:

   ```sql
   SELECT kind, profile, state, rel_path FROM media_assets
    WHERE recording_id = <ID> ORDER BY id;
   ```

   エンコードが終わるまでは `original` が `active` で残る。これが項目 3 の前半である
3. エンコード完了後、削除 reconcile の次のパス（既定 15 分間隔）を待つ。
   待たずに確かめるなら `docker compose exec rokuban rokuban enqueue delete-reconcile --config /config.yml`
4. 同じ SQL で `original` が `deleted`、`encoded` が `active` になっていることを見る
5. 録画詳細の画面で encoded 版を末尾までシークして再生する（項目 2）

原本が消えた後は再エンコードできない。保持ポリシーの判断は [storage/retention.md](../storage/retention.md) §6 にある。

### ごみ箱の復元（項目 4）

復元は `recordings.deleted_at` を消すだけで、ファイルに触れない。それを実物で確かめる。

1. 適当な録画の `rel_path` を上の SQL で引き、inode と mtime を控える:

   ```sh
   docker compose exec rokuban stat -c '%i %Y' /mnt/media/<rel_path>
   ```

2. UI で削除する（`DELETE /api/recordings/{id}`）。ごみ箱に出ることを見る
3. ごみ箱から復元する（`POST /api/recordings/{id}/restore`）
4. 1 と同じ `stat` の出力が変わっていないこと、`media_assets.state` が `active` のままであることを見る

ごみ箱の猶予は `cleanup.trash_retention`（既定 30 日）。猶予を過ぎた録画と
「今すぐ完全削除」した録画だけを削除 reconcile が unlink する。

### ブレーカーが発動したとき（項目 5）

ブレーカーは**削除だけ**を止めるラッチで、手で再開するまで止まり続ける。
発動中も予約の作成と録画は続くので、慌てて再開しない。
3 種類の意味と再開の可否の判断は [operations/alerts.md](../operations/alerts.md) の
「大量削除サーキットブレーカー発動」にある。手順だけを書く:

1. `curl -s http://localhost:40773/api/breakers | jq` で発動中のブレーカーと `pending` / `threshold` を見る
2. `ruler_deletes` は `detail` に消されようとしていた番組が載る。EPG の欠損で番組が消えていないかを確かめる
3. `delete_reconcile` は `detail` に対象が載らない。内訳は DB で見る。
   孤児回収の候補は `SELECT rel_path, first_seen FROM orphan_files ORDER BY first_seen;`
4. 正当なら再開する。site を持つブレーカーと持たないブレーカーで URL が違う:

   ```sh
   # ruler_deletes / reconcile_total_loss（site は GET /api/breakers の値）
   curl -X POST http://localhost:40773/api/sites/<site>/breakers/ruler_deletes/resume
   # delete_reconcile（site を持たない）
   curl -X POST http://localhost:40773/api/breakers/delete_reconcile/resume
   ```

`reconcile_total_loss` は件数ではなく「desired が空なのに自分の schedule がある」という形で発動する。
発動したら DB 接続と `reservations` の中身を先に疑う。

### catalog と rescue の練習（項目 6）

rescue は DB を失った後にだけ使う。**live DB に向けて練習しない**（catalog の内容で上書きする）。
練習は別名の DB に向けて行う。

1. catalog の世代を新しくしてから完成を確かめる:

   ```sh
   docker compose exec rokuban rokuban enqueue catalog-export --config /config.yml
   docker compose exec rokuban rokuban catalog verify --config /config.yml
   ```

   `catalog verify` は DB に触らない。完成世代が 1 つも無ければ非ゼロで終わる
2. 練習 DB を作り、マイグレーションと rescue を流す。`config.compose.yml` は
   `POSTGRES_DB` を DB 名に使うので、環境変数だけで向け先を変えられる:

   ```sh
   docker compose exec postgres sh -c 'createdb -U "$POSTGRES_USER" rokuban_drill'
   docker compose run --rm --no-deps -e POSTGRES_DB=rokuban_drill rokuban migrate up --config /config.yml
   docker compose run --rm --no-deps -e POSTGRES_DB=rokuban_drill rokuban rescue --config /config.yml
   ```

3. live DB と練習 DB で件数を比べる。差は catalog の世代時刻より後に増えた分だけのはずである:

   ```sql
   SELECT count(*) FROM recordings WHERE deleted_at IS NULL;
   SELECT kind, count(*) FROM media_assets WHERE state = 'active' GROUP BY kind;
   ```

4. 練習 DB を消す: `docker compose exec postgres sh -c 'dropdb -U "$POSTGRES_USER" rokuban_drill'`

**練習 DB に向けて `server` を起動しない**。削除 reconcile が練習 DB の内容で
共有の `media_dir` を unlink しに行く。rescue 自身はファイルを変更しない。
ただしファイル操作の lock を live の worker と共有するので、走っている間は公開と削除が待たされる。
世代の選び方と復元の契約は [storage/rescue.md](../storage/rescue.md) にある。

### webhook の疎通（項目 7）

`webhook.url` に実際の受け口を書き、短い番組を 1 本録って `recording.finished` が届くことを見る。
受け口がまだ無ければ、別ホストで `nc -l 8080` を開いて URL に向けると、
ヘッダ（`X-Rokuban-Webhook-Secret`）と本文を目で見られる。
`nc` は応答を返さないので、Rokuban 側には timeout のログが出る。
イベントの種類とペイロードは [configuration.md](../configuration.md) の「webhook のイベントとペイロード」にある。
webhook の失敗は録画や ingest を止めない。

### 切替手順

**同じ番組を 2 回録らないことが、この手順の順序を決めている**。
取り込んだルールは EPGStation 側の有効・無効をそのまま写すので、
`--rules` を実行した瞬間から Rokuban も予約を入れる。
取り込みから EPGStation のルール無効化までを、録画が始まらない時間帯に収める。

1. 切替の時間帯を決める。EPGStation の録画中が無く、次の予約の開始まで 30 分以上ある時間帯にする
2. ルールを取り込む。警告が出たルールは中身を見る（[import-epgstation.md](import-epgstation.md)）:

   ```sh
   docker compose exec rokuban rokuban import epgstation --config /config.yml --rules \
     --epgstation-url http://<epgstation>:8888
   ```

3. 予約を導出させて突き合わせる:

   ```sh
   docker compose exec rokuban rokuban enqueue ruler-pass --config /config.yml
   docker compose exec rokuban rokuban shadow-diff --config /config.yml --epgstation-url http://<epgstation>:8888
   ```

   `EPGStationOnly` に残るのは、ルール由来でない EPGStation の手動予約である。
   `--rules` は手動予約を取り込まないので、Rokuban の番組表から予約し直す。
   手動予約の一覧は次で出る:

   ```sh
   curl -s "http://<epgstation>:8888/api/reserves?type=all&isHalfWidth=false" \
     | jq '.reserves[] | select(.ruleId == null)'
   ```

4. EPGStation の UI で全ルールを無効にし、手動予約を消す。
   上の `/api/reserves` の `total` が 0 になることを見る
5. 取り込んだルールにエンコードプロファイルと保持ポリシーを設定する（上の「エンコードプロファイル」）
6. EPGStation の最後の録画が終わったら EPGStation を止める。
   データ（DB と録画ディレクトリ）はロールバックの期間が終わるまで残す
7. ライブラリを取り込む（[import-epgstation.md](import-epgstation.md) の「ライブラリ」）。
   EPGStation を止めてから書き出すので、取り込み後に増える録画は無い
8. 取り込みの欠けを確かめる（下の「ライブラリの欠け」）

**切替後に `--rules` を再実行しない**。再実行は `enabled` を EPGStation 側の値で上書きするので、
手順 4 で無効にした状態が Rokuban の全ルールに写る。

### ライブラリの欠け（項目 8）

EPGStation の録画ディレクトリは `media_dir` の配下にマウントしてある。
**取り込まなかったファイルは孤児回収の対象になる**。
走査は `catalog/` 以外の全ファイルを見るので、EPGStation の古いファイルはすぐ孤児候補になる。
mtime の猶予（既定 7 日）を過ぎたファイルは、エイジング（既定 14 日）の後に unlink される。

1. 件数を比べる。EPGStation 側は止める前に控えておく
   （`curl -s "http://<epgstation>:8888/api/recorded?isHalfWidth=false&limit=1" | jq .total`）。
   Rokuban 側はマウント先の前置で数える:

   ```sql
   SELECT count(DISTINCT recording_id) FROM media_assets
    WHERE rel_path LIKE 'sites/<site>/epgstation/%' AND state <> 'deleted';
   ```

2. 削除 reconcile を 1 回流し、マウント配下の孤児候補を出す:

   ```sh
   docker compose exec rokuban rokuban enqueue delete-reconcile --config /config.yml
   ```

   ```sql
   SELECT rel_path, first_seen FROM orphan_files
    WHERE rel_path LIKE 'sites/<site>/epgstation/%' ORDER BY rel_path;
   ```

3. ここに出たファイルは、`first_seen` から 14 日で消える。残すなら JSON に足して取り込み直す
   （取り込みは冪等）。消してよいものだけが残っている状態を合格とする

`<site>` と `epgstation/` は [import-epgstation.md](import-epgstation.md) の例に合わせた値で、
自分のマウント先に読み替える。

未解決: EPGStation の録画履歴（再放送の重複排除の種）は取り込めない。
切替直後は、EPGStation で録った番組の再放送を Rokuban がもう一度録りうる。

### ロールバック

**Rokuban を止めるだけでは録画は止まらない**。録画は mirakc が実行するので、
Rokuban が作った schedule は mirakc に残って録り続ける。

全ルールを無効にして reconciler に消させる手は使えない。
desired が空になった時点で `reconcile_total_loss` が発動し、再開しても次のパスで同じ形を観測して再発動する。
mirakc の schedule を直接消す。

1. EPGStation を起動し、ルールを有効に戻す
2. Rokuban を止める: `docker compose stop rokuban`
3. Rokuban が作った schedule（tag が `program:` で始まる）を mirakc から消す:

   ```sh
   curl -s "$MIRAKC_URL/api/recording/schedules" \
     | jq -r '.[] | select(any(.tags[]?; startswith("program:"))) | .program.id' \
     | while read -r id; do curl -s -X DELETE "$MIRAKC_URL/api/recording/schedules/$id"; done
   ```

   録画中の schedule を消したときの mirakc の挙動は未検証である。
   録画中（`state` が `recording`）のものは終わるのを待ってから消すのが安全
4. 同じ `GET` の結果に `program:` の tag が 0 件であることを見る

Rokuban の DB とメディアはそのまま残る。`docker compose start rokuban` で戻すと、
ルールが有効なままなら次の reconcile パスで schedule が作り直される。
切替を再試行するときは、切替手順の 4（EPGStation のルール無効化）と同じ時間帯に戻す。
