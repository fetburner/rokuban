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
| 1 | 予約差分 | `rokuban shadow-diff` が終了コード 0。または `EPGStationOnly` が手動予約と、無効で取り込まれたルールの予約だけ（[shadow.md](shadow.md)） |
| 2 | 派生物の再生 | Rokuban で録った番組の encoded 版をブラウザで最後まで再生できる |
| 3 | `until_encoded` | 派生物が揃うまでは原本が `active` のまま。揃った後に原本だけが `deleted` になり、encoded は再生できる |
| 4 | ごみ箱の復元 | 削除 → 復元の前後でファイルの inode と mtime が変わらない |
| 5 | ブレーカー | `GET /api/breakers` が空。`rokuban_circuit_breaker_tripped` が全系列 0 |
| 6 | catalog | `rokuban catalog verify` が終了コード 0。練習 DB への rescue が通る |
| 7 | webhook（使う場合） | 実際の受け口が `recording.finished` を 1 件受け取る |

切替後に次の 2 項目を確かめる。どちらかが落ちたらロールバックを検討する。

| # | 項目 | 合格の判定 |
|---|---|---|
| 8 | ライブラリの欠け | EPGStation の録画件数と取り込んだ件数の差を全件説明できる。マウント配下の未登録ファイルが、消えてよいものだけ |
| 9 | 実体無し | 取り込みから 24 時間以上後に `rokuban_media_assets_missing` の系列が 1 つも無い。`rokuban_missing_asset_scan_suspected_storage_failure_total` も増えていない |

**項目 1 は、並走中に Rokuban 側でも同じルールが有効になっていることを前提にする**。
その間は両方が録る（[shadow.md](shadow.md) の「二重録画に注意」）。
二重録画を避けるために Rokuban のルールを無効にしていたなら、項目 1 の前に有効にする。
二重録画は資源を余分に使うだけで、録り逃しより安い。

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
3. 派生物が揃ったら、削除 reconcile の次のパス（既定 15 分間隔）を待つ。
   原本はエンコード完了だけでは消えない。サムネイルや TS 計測なども待つ。
   条件の権威は view `until_encoded_deletable_originals` で、判断は [storage/retention.md](../storage/retention.md) §6 §7 にある。
   待たずに確かめるなら `docker compose exec rokuban rokuban enqueue delete-reconcile --config /config.yml`
4. 同じ SQL で `original` が `deleted`、`encoded` が `active` になっていることを見る
5. 録画詳細の画面で encoded 版を末尾までシークして再生する（項目 2）

原本が消えた後は再エンコードできない。

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
各ブレーカーの意味と再開の可否の判断は [operations/alerts.md](../operations/alerts.md) の
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

   `enqueue` は投入だけして終わるので、直後の `verify` は 1 つ前の世代を見ることがある。
   rescue が使う世代の名前（`catalog-<UTC 時刻>`）が投入より後の時刻になるまで `verify` を繰り返す。
   `catalog verify` は DB に触らない。完成世代が 1 つも無ければ非ゼロで終わる
2. 練習 DB を作り、マイグレーションと rescue を流す。`config.compose.yml` は
   `POSTGRES_DB` を DB 名に使うので、環境変数だけで向け先を変えられる:

   ```sh
   docker compose exec postgres sh -c 'createdb -U "$POSTGRES_USER" rokuban_drill'
   docker compose run --rm --no-deps -e POSTGRES_DB=rokuban_drill rokuban migrate up --config /config.yml
   docker compose run --rm --no-deps -e POSTGRES_DB=rokuban_drill rokuban rescue --config /config.yml
   ```

3. live DB と練習 DB で件数を比べる。差は世代時刻より後に起きた録画・削除・ごみ箱移動・状態の変化で説明できるはずである:

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
ヘッダと本文を目で見られる（`X-Rokuban-Webhook-Secret` は `webhook.secret` が空でないときだけ付く）。
`nc` は応答を返さないので、Rokuban 側には timeout のログが出る。
イベントの種類とペイロードは [configuration.md](../configuration.md) の「webhook のイベントとペイロード」にある。
webhook の失敗は録画や ingest を止めない。

### 切替手順

並走中は両方が録っているので、切替は EPGStation 側を止めるだけで済む。
Rokuban の schedule は並走中から mirakc にあるので、切替で録り逃す窓は無い。
**手順の順序を決めるのは、`--rules` の取り込みが EPGStation 側の `enabled` を写すことである**。

1. 並走中に EPGStation 側で足した・変えたルールを取り込み直す。
   EPGStation のルールが有効なうちに行う。警告が出たルールは中身を見る（[import-epgstation.md](import-epgstation.md)）:

   ```sh
   docker compose exec rokuban rokuban import epgstation --config /config.yml --rules \
     --epgstation-url http://<epgstation>:8888
   ```

2. 予約を導出させて突き合わせる。`enqueue` は投入だけして終わるので、差分が残ったら少し待って `shadow-diff` を再実行する:

   ```sh
   docker compose exec rokuban rokuban enqueue ruler-pass --config /config.yml
   docker compose exec rokuban rokuban shadow-diff --config /config.yml --epgstation-url http://<epgstation>:8888
   ```

   `EPGStationOnly` に残るのは 2 種類である。
   1 つはルール由来でない EPGStation の手動予約で、`--rules` の対象外なので Rokuban の番組表から予約し直す。
   もう 1 つは条件が 1 つも残らず無効で取り込まれたルールの予約で、条件を直して有効にする。
   手動予約の一覧は次で出る（`total` が 1000 を超えるなら `offset` を進める）:

   ```sh
   curl -s "http://<epgstation>:8888/api/reserves?type=all&isHalfWidth=false&limit=1000&offset=0" \
     | jq '.reserves[] | select(.ruleId == null)'
   ```

3. EPGStation の UI で全ルールを無効にし、手動予約を消す。上の `/api/reserves` の `total` が 0 になることを見る。
   ここから先は Rokuban だけが録る
4. 取り込んだルールにエンコードプロファイルと保持ポリシーを設定する（上の「エンコードプロファイル」）
5. EPGStation の最後の録画が終わったら、録画件数を控えてから EPGStation を止める:

   ```sh
   curl -s "http://<epgstation>:8888/api/recorded?isHalfWidth=false&limit=1&offset=0" | jq .total
   ```

   データ（DB と録画ディレクトリ）はロールバックの期間が終わるまで残す
6. ライブラリを取り込む（下の「ライブラリの取り込み」）
7. 取り込みの欠けを確かめる（下の「ライブラリの欠け」）

**手順 3 の後に `--rules` を再実行しない**。再実行は `enabled` を EPGStation 側の値で上書きするので、
手順 3 で無効にした状態が Rokuban の全ルールに写る。

### ライブラリの取り込み

**EPGStation の録画ディレクトリを `media_dir` の配下に置いた瞬間から、孤児回収の時計が動く**。
走査は `catalog/` 以外の全ファイルを見る。取り込まなかったファイルは、mtime の猶予（既定 7 日）を過ぎていれば
次のパスで孤児候補になり、エイジング（既定 14 日）の後に unlink される。
止めるのは 1 パスの削除数のブレーカーだけである。
そのため、マウントは取り込みの直前に行い、14 日以内に取り込みと確認を終える。

1. **取り込めないファイルをマウントの外へ出す**。取り込むのは `type=ts` の原本とサムネイル 1 件だけである。
   EPGStation のエンコード済みファイル（`type=encoded`）は警告付きで飛ばされ、マウント配下に残れば孤児として消える。
   エンコード済みファイルの出力先が録画ディレクトリの中にあるなら、先に別の場所へ移す
2. 録画ディレクトリを `sites/<site>/` の配下にマウントする。compose なら `rokuban` サービスの `volumes` に足す（例）:

   ```yaml
   - /opt/epgstation/recorded:/mnt/media/sites/<site>/epgstation
   ```

3. JSON を書き出してコンテナへ渡し、取り込む。JSON の書き出し方は [import-epgstation.md](import-epgstation.md) の「ライブラリ」:

   ```sh
   docker compose cp library.json rokuban:/tmp/library.json
   docker compose exec rokuban rokuban import epgstation --config /config.yml --library-json /tmp/library.json
   ```

未解決: エンコード済みファイルしか持たない EPGStation の録画（原本をエンコード後に消したもの）は再生できる形で取り込めない。
サムネイルがあればサムネイルだけの録画（再生不可）として登録され、無ければ飛ばされる。

### ライブラリの欠け（項目 8）

1. 件数を比べる。EPGStation 側は切替手順 5 で控えた値を使う。Rokuban 側はマウント先の前置で数える:

   ```sql
   SELECT count(DISTINCT recording_id) FROM media_assets
    WHERE rel_path LIKE 'sites/<site>/epgstation/%' AND state <> 'deleted';
   ```

   差は、取り込みの警告（encoded しか無い録画など）で説明できるはずである
2. マウント配下のファイルと登録済みの `rel_path` を突き合わせ、未登録のファイルを出す。
   `orphan_files` は使わない。mtime が 7 日以内のファイルはそこに載らず、後から孤児候補になって消えるためである:

   ```sh
   docker compose exec -T rokuban sh -c 'cd /mnt/media && find sites/<site>/epgstation -type f' | sort > files.txt
   docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At' <<'SQL' | sort > assets.txt
   SELECT rel_path FROM media_assets
    WHERE rel_path LIKE 'sites/<site>/epgstation/%' AND state <> 'deleted';
   SQL
   comm -23 files.txt assets.txt
   ```

3. `comm` の出力は、取り込まない限りいずれ消えるファイルの一覧である。
   残す原本は JSON に足して取り込み直す（取り込みは冪等）。取り込めない種類はマウントの外へ移す。
   出力が空になるか、消えてよいものだけになれば合格とする

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

手順 2 から 4 までの間はどちらも録らない。録画中が無く、次の予約の開始まで余裕がある時間帯に行う。
録り逃しより二重録画を選ぶなら、手順 4 を先に済ませてから 1 を始める。

1. Rokuban を止める: `docker compose stop rokuban`
2. Rokuban が作った schedule（tag が `program:` で始まる）を mirakc から消す。
   `MIRAKC_URL` は `.env` にしか無いので、先にシェルへ読み込む:

   ```sh
   set -a; . ./.env; set +a
   curl -s "$MIRAKC_URL/api/recording/schedules" \
     | jq -r '.[] | select(any(.tags[]?; startswith("program:"))) | .program.id' \
     | while read -r id; do curl -s -X DELETE "$MIRAKC_URL/api/recording/schedules/$id"; done
   ```

   録画中の schedule を消したときの mirakc の挙動は未検証である。
   録画中（`state` が `recording`）のものは終わるのを待ってから消すのが安全
3. 同じ `GET` の結果に `program:` の tag が 0 件であることを見る
4. EPGStation を起動し、ルールを有効に戻す。切替手順 3 で消した手動予約は戻らないので、入れ直す
5. ライブラリを取り込んだ後なら、取り込みで加えた変更を戻す:
   - 「ライブラリの取り込み」の手順 1 でマウントの外へ移したエンコード済みファイルを元の場所へ戻す。
     EPGStation の DB は元のパスを指している
   - `rokuban` サービスの `volumes` から録画ディレクトリのマウントを外す。
     **外さずに Rokuban を起動すると、EPGStation の新しい録画が取り込まれないまま孤児回収で消える**

Rokuban の DB とメディアはそのまま残る。マウントを外した後は、取り込んだ録画の行が実体無しとして報告される（削除はされない）。
Rokuban を戻すときは、`volumes` の変更を反映するために `docker compose up -d rokuban` で作り直す。
ルールが有効なままなら、次の reconcile パスで schedule が作り直される。
切替を再試行するときは、並走の状態（両方が録る）に戻ってから切替手順 1 からやり直す。
ライブラリは「ライブラリの取り込み」の手順どおり、マウントから取り込み直す。
