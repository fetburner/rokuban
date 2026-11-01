> [runbook.md](../runbook.md) の一部。索引から辿る。

## EPGStation からの移行（`rokuban import epgstation`）

EPGStation から Rokuban へ、ルールとライブラリ（録画ファイル）を移す。ルールは EPGStation の REST API から、ライブラリは EPGStation の DB から書き出した JSON から取り込む。

実行するコマンドは `rokuban import epgstation --rules` と `--library-json` の 2 つである。

### ルール（`--rules`）

EPGStation の REST API を直接叩いて取り込む。EPGStation を起動したまま実行してよい。

```sh
rokuban import epgstation --config config.yml --rules \
  --epgstation-url http://localhost:8888
```

再実行しても行は増えない。EPGStation 側のルール id を冪等キーにしているからである。

次のルールは、標準出力に警告として出る。

- ARE 非互換の正規表現を含むルール
- `%CHNAME%` 等の未対応テンプレート変数を含むルール
- 時刻指定ルール
- 条件が 1 つも残らなかったルール

警告が出たルールは、有効化する前に必ず内容を見る。DB で有効 / 無効の列を直接確認してもよい。

<details><summary>根拠</summary>

- 有効 / 無効の列: `rules` テーブルの `enabled`

</details>

### ライブラリ（`--library-json`）

ライブラリは、EPGStation 自身の DB から JSON を書き出して取り込む。3 段で行う。録画ディレクトリをマウントし、JSON を書き出し、取り込む。

EPGStation の REST API（`GET /api/recorded`）は実ファイルの相対パスを返さない。ファイル名はディレクトリ構成を落としたものだからである。そのため EPGStation 自身の DB（SQLite または MySQL。PostgreSQL は使えない）から直接 SELECT して JSON を書き出す。

<details><summary>根拠</summary>

- 返らない相対パスの代わりに返る値: `filename`
- ディレクトリ構成を落とす処理: `path.basename(filePath)`

</details>

#### 1. EPGStation の録画ディレクトリを rokuban の `media_dir` 配下にマウントする

録画ファイルはコピーせず、既存の録画ディレクトリを、Rokuban のメディアの置き場の下にそのまま見える位置へマウントする。マウント先は site の名前空間の配下にする。JSON のパスにはその前置を含めない。

手段は問わない（bind mount / symlink 等）。置き場は rokuban の `storage.media_dir`（`config.yml`）である。

原本は site の名前空間に属する必要がある。worker が起動時に検査する（docs/storage/contract.md §「rel_path の名前空間」）。`{site}` は `--library-json` 実行時の `config.yml` に登録した唯一の site 名で、`import epgstation` は単一 site に限る。JSON 自体に前置を含める必要はない。取り込みが相対パスへ自動で前置するからである。**マウント先は `sites/{site}/` の配下にする。**

例として、EPGStation の録画が `/opt/epgstation/recorded/` にあるとする。rokuban の `media_dir` が `/mnt/media`、site 名が `tokyo` なら、`/mnt/media/sites/tokyo/epgstation/` に bind mount する。このとき JSON の `relPath` は `epgstation/...`（マウント先からの相対パス）になる。`sites/tokyo/` は含めない。

<details><summary>根拠</summary>

- 名前空間に属する必要がある原本: `videoFiles[].type = "ts"`
- 前置を自動で付ける関数: `ImportLibrary`

</details>

#### 2. JSON を書き出す

JSON は EPGStation の DB から書き出す。SQLite と MySQL で SQL が違う。MySQL では raw 指定を省略しない。どちらの DB でも、ファイルのパスはマウントした先から見た相対パスに書き換える。

EPGStation の DB スキーマは、EPGStation の migrations で確認した。テーブル名は snake_case で、列名は camelCase のままである。

<details><summary>根拠</summary>

- スキーマの確認元: `l3tnun/EPGStation`、`src/db/migrations/{sqlite,mysql}/*-Init.ts`
- テーブル名: `recorded` / `video_file` / `thumbnail` / `channel`
- camelCase のままの列名: `channelId` / `filePath` / `recordedId`

</details>

SQLite の SQL は次の通り。JSON1 拡張が要る。SQLite 3.38 以降は標準搭載で、EPGStation がバンドルする better-sqlite3 で問題になったことはない。

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

SQL が `programId` を積むのは、これが実際の放送の識別子で、合成した擬似 id より衝突しにくいからである。Rokuban 自身が同じ放送を録っていれば、同一の録画として扱われる。

<details><summary>根拠</summary>

- 放送の識別子を分解する関数: `internal/mirakc. SplitProgramID`

</details>

MySQL の SQL は次の通り。`JSON_ARRAYAGG` は MySQL 5.7.22+ / 8.0 で使え、MariaDB は 10.5+ が要る。古い MariaDB では動かない（未検証）ので事前に確認する。

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

**`-r`/`--raw` は省略しない。** `mysql -B` の既定（batch）出力は `\`・タブ・改行をエスケープする。すると `JSON_OBJECT` が出す `\"` がさらに `\\"` になり、番組名にダブルクオートを含む行が 1 つでもあると JSON が正しく読めなくなる。`--raw` はこのエスケープを止める。SQLite の CLI にはこの問題がない。

`v.filePath` / `t.filePath` は、EPGStation が実際に書き込んだパスである。設定とバージョンによって、絶対パスのことも、`parentDirectoryName` からの相対パスのこともある。手順 1 でマウントした先から見た相対パスに必ず書き換える。絶対パスをそのまま貼るなら、SQL 側で `REPLACE(v.filePath, '/opt/epgstation/recorded/', 'epgstation/')` のように前段で置換しておくと楽である。

書き出される JSON の例は次の通り（1 件だけ示す）。

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

<details><summary>根拠</summary>

- JSON の型: `internal/epgimport.LibraryItem` の配列

</details>

- `videoFiles[].type` は `"ts"` だけを取り込む。`"encoded"` は rokuban の encode profile 名前空間と対応が取れないため警告してスキップする
- `thumbnails` は 1 件目だけ取り込む。2 件目以降は警告してスキップする。録画 1 件につき thumbnail 1 行までしか持てないからである
- `programId` が無ければ `channelId` + `name` + `endAt` から合成識別子を作る。実際の識別子より衝突しやすいので、上の SQL の通り積んでおくのが安全である
- `channelType` / `serviceName` / `channel` を省略すると、それぞれ `GR` / `"unknown"` / `"unknown"` にフォールバックして警告する。落とさず埋めておくのが安全である（`serviceName` / `channel` はライブラリ UI にそのまま出る）

<details><summary>根拠</summary>

- thumbnail 1 行までの制約: `media_assets`

</details>

#### 3. 取り込む

書き出した JSON を指定して取り込む。再実行しても行は増えない。

```sh
rokuban import epgstation --config config.yml --library-json library.json
```

増えないのは、放送の識別子とメディアの相対パスの両方に一意制約があるからである。

既存の資産と両立しない Recorded は `ErrConflict` で拒否される。条件は、ごみ箱の録画に属する、生きたコピーが既にある、同じ置き場の同じ path に削除済み tombstone がある、のいずれかである。その Recorded だけを飛ばし、Warn ログと出力の `conflicts:` 件数で報告して、残りの取り込みを続ける。tombstone の件数も `tombstones:` に出る。ファイルを移動または改名して再実行すると登録できる。条件は rescue と同じである（[storage/rescue.md](../storage/rescue.md) の「既存の資産と両立しないファイル」）。

<details><summary>根拠</summary>

- 放送の識別子の一意制約: `recordings`
- 相対パスの一意制約: `media_asset_copies (store, rel_path)`

</details>

### 前置導入前に取り込んだライブラリの移行

site の前置を自動で付ける前のバージョンでライブラリを取り込み済みなら、登録した原本のパスに前置が無く、worker が起動時に拒否する。worker と streamer を止めてから、ファイルを移動してパスを更新する。

検査の契約は docs/storage/contract.md §「rel_path の名前空間」にある。止めてから行うのは、動かしたまま `mv` すると、有効な行が一時的に実体無しと誤検出されるからである。

1. 対象行を出す。`site` は import 時に指定した site 名である。SQL の `<store>` は、`storage.stores` の FS 置き場の名前に置き換える。

   ```sql
   SELECT a.id, c.rel_path, r.site
     FROM media_assets a
     JOIN media_asset_copies c ON c.media_asset_id = a.id
     JOIN recordings r ON r.id = a.recording_id
    WHERE c.store = '<store>'
      AND c.state <> 'deleted'
      AND a.kind = 'original'
      AND c.rel_path NOT LIKE 'sites/%';
   ```

2. 各行について `{media_dir}/{rel_path}` を `{media_dir}/sites/{site}/{rel_path}` へ `mkdir -p` + `mv` する（同一 FS 内なので rename）。
3. 全ファイルの `mv` が終わったら、1 トランザクションでコピーのパスを更新する。`deleting` 状態のコピーも対象に含める。含めないと delete_reconcile が旧パスを unlink しに行き、実体無しと誤検出する。

   ```sql
   UPDATE media_asset_copies c
      SET rel_path = 'sites/' || r.site || '/' || c.rel_path
     FROM media_assets a
     JOIN recordings r ON r.id = a.recording_id
    WHERE c.media_asset_id = a.id
      AND c.store = '<store>'
      AND c.state <> 'deleted'
      AND a.kind = 'original'
      AND c.rel_path NOT LIKE 'sites/%';
   ```
4. 旧パスを指す孤児候補の行が残っていれば削除する（導出された一覧なので次パスで作り直される）。
5. `rokuban enqueue catalog-export` で新しい catalog 世代を書き、`rokuban catalog verify` で完成を確認する。以後 rescue が移行前の `rel_path` を持つ旧世代を選ばないよう、確認後に旧世代を消す。

<details><summary>根拠</summary>

- 一時的に実体無しと誤検出される有効な行: `active`
- 孤児候補の行: `orphan_files`

</details>

### 履歴（RecordedHistory）は未対応

EPGStation の履歴（再放送の重複排除の種）は、このコマンドに含まない。取り込んでも重複排除には一切役に立たないので、Rokuban 側の対応と合わせて別途扱う。

Rokuban の重複排除は、ルール id が一致する行だけを比較対象にする。ライブラリ import が使う登録は、ルール id を書く列を持たない。履歴自体にも EPGStation 側のルール id が無い。

<details><summary>根拠</summary>

- 重複排除の実装: `internal/store/pgstore/rulerpass.go`
- 比較対象を決める列: `recordings.rule_id`
- ライブラリ import が使う登録: `internal/inplace.Register`
- EPGStation の履歴: `RecordedHistory`
- 合わせて直す側: `internal/ruler`

</details>
