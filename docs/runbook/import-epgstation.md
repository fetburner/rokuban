> [runbook.md](../runbook.md) の一部。索引から辿る。

## EPGStation からの移行（`rokuban import epgstation`）

`rokuban import epgstation --rules` と `--library-json` の 2 つを使う。

### ルール（`--rules`）

EPGStation の REST API を直接叩く。EPGStation を起動したまま実行してよい。

```sh
rokuban import epgstation --config config.yml --rules \
  --epgstation-url http://localhost:8888
```

再実行しても行は増えない（EPGStation 側のルール id を冪等キーにしている）。
ARE 非互換の正規表現・`%CHNAME%` 等の未対応テンプレート変数・時刻指定ルール・
条件が 1 つも残らなかったルールは、いずれも標準出力に警告として出る。
警告が出たルールは有効化する前に必ず内容を見る（`rules` テーブルの
`enabled` 列を直接確認してもよい）。

### ライブラリ（`--library-json`）

EPGStation の REST API（`GET /api/recorded`）は実ファイルの相対パスを返さない
（`filename` は `path.basename(filePath)` で、ディレクトリ構成が失われる）。
そのため EPGStation 自身の DB（SQLite または MySQL。PostgreSQL は使えない）
から直接 SELECT して JSON を書き出す。

#### 1. EPGStation の録画ディレクトリを rokuban の `media_dir` 配下にマウントする

ファイル本体はコピーしない。既存の録画ディレクトリを、rokuban の
`storage.media_dir`（`config.yml`）の下にそのまま見える位置へマウントする
（bind mount / symlink 等、手段は問わない）。

原本（`videoFiles[].type = "ts"`）は `sites/{site}/` 名前空間に属する必要がある
（worker が起動時に検査する。docs/storage/contract.md §「rel_path の名前空間」）。
`{site}` は `--library-json` 実行時の `config.yml` に登録した唯一の site 名である
（`import epgstation` は単一 site 限定）。`ImportLibrary` がこれを rel_path へ
自動で前置するので、JSON 自体にこの前置を含める必要はない。
**マウント先は `sites/{site}/` の配下にする。**

例: EPGStation の録画が `/opt/epgstation/recorded/` にあるとする。
rokuban の `media_dir` が `/mnt/media`、site 名が `tokyo` なら
`/mnt/media/sites/tokyo/epgstation/` に bind mount する。
このとき JSON の `relPath` は `epgstation/...`（マウント先からの相対パス）になる。
`sites/tokyo/` は含めない。

#### 2. JSON を書き出す

EPGStation の DB スキーマは `l3tnun/EPGStation` の migrations で確認した。
対象は `src/db/migrations/{sqlite,mysql}/*-Init.ts`。テーブル名は
snake_case（`recorded` / `video_file` / `thumbnail` / `channel`）。
**列名は camelCase のまま**（`channelId` / `filePath` / `recordedId` 等）。

**SQLite**（JSON1 拡張が要る。SQLite 3.38 以降は標準搭載。EPGStation が
バンドルする better-sqlite3 で問題になったことはない）:

```sh
sqlite3 /path/to/epgstation/data/database.db <<'SQL' > library.json
SELECT json_group_array(json_object(
  'programId', r.programId,
  'channelId', r.channelId,
  'channelType', c.channelType,
  'serviceName', c.name,
  'channel', c.channel,
  'startAt', r.startAt,
  'endAt', r.endAt,
  'name', r.name,
  'videoFiles', json(COALESCE((
    SELECT json_group_array(json_object('type', v.type, 'relPath', v.filePath))
    FROM video_file v WHERE v.recordedId = r.id
  ), '[]')),
  'thumbnails', json(COALESCE((
    SELECT json_group_array(json_object('relPath', t.filePath))
    FROM thumbnail t WHERE t.recordedId = r.id
  ), '[]'))
))
FROM recorded r
LEFT JOIN channel c ON c.id = r.channelId;
SQL
```

`programId` を積むのは、これが real な放送 identity（`internal/mirakc.
SplitProgramID` で分解できる）で、合成した擬似 id より衝突しないため。
rokuban 自身が同じ放送を録っていれば同一の録画として扱われる。

**MySQL**（`JSON_ARRAYAGG` は MySQL 5.7.22+ / 8.0。MariaDB は 10.5+ が要る。
古い MariaDB では動かないので事前に確認する — 未検証）:

```sh
mysql -N -B -r epgstation_db <<'SQL' > library.json
SELECT JSON_ARRAYAGG(JSON_OBJECT(
  'programId', r.programId,
  'channelId', r.channelId,
  'channelType', c.channelType,
  'serviceName', c.name,
  'channel', c.channel,
  'startAt', r.startAt,
  'endAt', r.endAt,
  'name', r.name,
  'videoFiles', COALESCE((
    SELECT JSON_ARRAYAGG(JSON_OBJECT('type', v.type, 'relPath', v.filePath))
    FROM video_file v WHERE v.recordedId = r.id
  ), JSON_ARRAY()),
  'thumbnails', COALESCE((
    SELECT JSON_ARRAYAGG(JSON_OBJECT('relPath', t.filePath))
    FROM thumbnail t WHERE t.recordedId = r.id
  ), JSON_ARRAY())
))
FROM recorded r
LEFT JOIN channel c ON c.id = r.channelId;
SQL
```

**`-r`/`--raw` は省略しないこと。** `mysql -B` の既定（batch）出力は
`\`・タブ・改行をエスケープする。`JSON_OBJECT` が出す `\"` がさらに `\\"`
になり、番組名にダブルクオートを含む行が 1 つでもあると JSON が壊れる。
`--raw` はこのエスケープを止める（SQLite の CLI にはこの問題がない）。

`v.filePath` / `t.filePath` は EPGStation が実際に書き込んだパス（設定と
バージョンによって絶対パスのことも、`parentDirectoryName` からの相対
パスのこともある）。**手順 1 でマウントした先から見た相対パスに必ず
書き換える**。絶対パスをそのまま貼るなら SQL 側で `REPLACE(v.filePath,
'/opt/epgstation/recorded/', 'epgstation/')` のように前段で潰しておくと楽。

書き出される JSON の例（`internal/epgimport.LibraryItem` の配列。1 件だけ
示す）:

```json
[
  {
    "programId": 327360102415397,
    "channelId": 3273601024,
    "channelType": "GR",
    "serviceName": "○○テレビ",
    "channel": "27",
    "startAt": 1785000000000,
    "endAt": 1785001800000,
    "name": "番組名",
    "videoFiles": [
      { "type": "ts", "relPath": "epgstation/20260101/show.ts" }
    ],
    "thumbnails": [
      { "relPath": "epgstation/thumbnails/show.jpg" }
    ]
  }
]
```

- `videoFiles[].type` は `"ts"` だけを取り込む。`"encoded"` は rokuban の
  encode profile 名前空間と対応が取れないため警告してスキップする
- `thumbnails` は 1 件目だけ取り込む（`media_assets` は録画 1 件につき
  thumbnail 1 行までしか持てない）。2 件目以降は警告してスキップする
- `programId` が無ければ `channelId` + `name` + `endAt` から合成識別子を
  作る（実 identity より衝突しやすい。上の SQL の通り積んでおくのが安全）
- `channelType` / `serviceName` / `channel` を省略すると、それぞれ
  `GR` / `"unknown"` / `"unknown"` にフォールバックして警告する。落とさず
  埋めておくのが安全（`serviceName`/`channel` はライブラリ UI にそのまま出る）

#### 3. 取り込む

```sh
rokuban import epgstation --config config.yml --library-json library.json
```

再実行しても行は増えない（`recordings` の放送 identity と
`media_assets.rel_path` の両方に一意制約がある）。

### 前置導入前に取り込んだライブラリの移行

`sites/{site}/` 前置を自動で付ける前のバージョンで `--library-json` を実行
済みの場合、そのとき登録した原本の `rel_path` に前置が無い。worker は起動時
にこれを検査して拒否する（docs/storage/contract.md §「rel_path の名前空間」）。
以下の手順は worker と streamer を止めてから行う（動かしたまま `mv` すると
`active` 行が一時的に実体無しと誤検出される）。

1. 対象行を出す（`site` は import 時に指定した site 名）:

   ```sql
   SELECT a.id, a.rel_path, r.site
     FROM media_assets a JOIN recordings r ON r.id = a.recording_id
    WHERE a.state <> 'deleted'
      AND a.kind = 'original'
      AND a.rel_path NOT LIKE 'sites/%';
   ```

2. 各行について `{media_dir}/{rel_path}` を `{media_dir}/sites/{site}/{rel_path}`
   へ `mkdir -p` + `mv` する（同一 FS 内なので rename）。
3. 全ファイルの `mv` が終わったら 1 トランザクションで
   `UPDATE media_assets SET rel_path = 'sites/' || r.site || '/' || a.rel_path`
   を対象行に適用する（`deleting` 状態の行も対象に含める。含めないと
   delete_reconcile が旧パスを unlink しに行き実体無しと誤検出する）。
4. 旧パスを指す `orphan_files` の行が残っていれば削除する（導出台帳なので
   次パスで作り直される）。
5. `rokuban enqueue catalog_export` で新しい catalog 世代を書き、
   `rokuban catalog verify` で完成を確認する。以後 rescue は移行前の
   `rel_path` を持つ旧世代を選ばせないよう、確認後に旧世代を消す。

### 履歴（RecordedHistory）は未対応

EPGStation の `RecordedHistory`（再放送重複排除の種）はこのコマンドに
含まない。rokuban の重複排除（`internal/ruler/dedupe.go`）は
`recordings.rule_id` が一致する行だけを比較対象にする。ライブラリ import
が使う in-place 登録（`internal/inplace.Register`）は rule_id を書く列を
持たない。`RecordedHistory` 自体にも EPGStation 側のルール id が無い。
取り込んでも重複排除には一切効かないため、`internal/ruler` 側の対応と
合わせて別途扱う（GitHub issue #72 のコメント参照）。
