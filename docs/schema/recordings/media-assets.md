> [docs/schema.md](../../schema.md)（索引）の分割本文。節番号は分割前のまま（§6）。索引から辿る。

## 6. media_assets — メディアアセット（永続資産）

**メディアアセットの行の存在が「公開済み」の定義である**（ストレージ契約ルール 3）。録画に紐づくファイルの記録で、物理削除の後も行は消さずに残す。

```sql
CREATE TABLE media_assets (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recording_id bigint NOT NULL REFERENCES recordings (id),
    kind         text   NOT NULL CHECK (kind IN ('original', 'encoded', 'thumbnail', 'seek_tiles')),
    profile      text,                -- kind = 'encoded' のみ: エンコードプロファイル名
    CHECK ((kind = 'encoded') = (profile IS NOT NULL)),
    rel_path     text   NOT NULL,     -- ストレージルートからの相対パス（不変条件: 絶対パス禁止）
    size_bytes   bigint NOT NULL,
    sha256       bytea,                -- 内容の SHA-256。コピー数によらず 1 回だけ持つ
    derived_generation bigint,         -- 派生物のパスに最後に採った世代。original は NULL
    CHECK (derived_generation IS NULL OR (kind <> 'original' AND derived_generation >= 0)),
    CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    ingest_generation bigint,         -- kind = 'original' のみ: 原本にした temp の世代
    CHECK (ingest_generation IS NULL OR ingest_generation > 0),
    CHECK (kind = 'original' OR ingest_generation IS NULL),

    -- 物理削除プロトコル: active → deleting → deleted（冪等。どこで落ちても reconcile が拾い直す）
    state        text NOT NULL DEFAULT 'active'
                      CHECK (state IN ('active', 'deleting', 'deleted')),
    deleted_at   timestamptz,         -- 物理削除の完了時刻（tombstone）

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- 1 録画につき original / thumbnail / seek_tiles は 1 つ、encoded はプロファイルごとに 1 つ
    UNIQUE NULLS NOT DISTINCT (recording_id, kind, profile)
);

CREATE INDEX ON media_assets (recording_id);
CREATE INDEX ON media_assets (kind, state);
-- 生きているアセットのパス衝突を防ぐ（tombstone のパスは再利用可）
CREATE UNIQUE INDEX ON media_assets (rel_path) WHERE state <> 'deleted';
```

`media_assets` は論理資産の識別子、録画との関係、サイズ、SHA-256 を持つ。ローリングアップデート中の旧 binary が読むため、`rel_path` / `state` / `deleted_at` も残し、新しい書き手は両表を更新する。同じ理由で、view `removed_encoded_assets` と、関数と同名の旧 view `until_encoded_deletable_originals` が旧列を射影する。後者の旧 view は、置き場を引数に取る同名の関数を呼ぶ。現在の読み手は物理パスとライフサイクルを `media_asset_copies` から読む。コピーの数が増えても、サイズと SHA-256 は論理資産の 1 か所にしか置かない。

### media_asset_copies — 物理ファイルごとのコピー

論理資産の物理ファイルは、置き場と世代ごとに 1 行のコピーとして持つ。

```sql
CREATE TABLE media_asset_copies (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    media_asset_id bigint NOT NULL REFERENCES media_assets (id) ON DELETE CASCADE,
    store          text   NOT NULL CHECK (store ~ '^[a-z0-9-]+$'),
    rel_path       text   NOT NULL,
    generation     bigint NOT NULL, -- この物理ファイルの世代。INSERT 後は不変
    state          text   NOT NULL CHECK (state IN ('pending', 'active', 'deleting', 'deleted')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);

CREATE INDEX ON media_asset_copies (media_asset_id);
CREATE INDEX ON media_asset_copies (media_asset_id, store) WHERE state = 'active';
CREATE UNIQUE INDEX ON media_asset_copies (store, rel_path) WHERE state <> 'deleted';
CREATE UNIQUE INDEX ON media_asset_copies (media_asset_id, store, generation) WHERE state <> 'deleted';

CREATE TABLE missing_media_assets (
    media_asset_copy_id bigint PRIMARY KEY REFERENCES media_asset_copies (id) ON DELETE CASCADE,
    first_seen           timestamptz NOT NULL DEFAULT now()
);
```

`media_asset_copies` の 1 行は物理ファイル 1 つを表す。`generation` はファイルの内容世代で、INSERT 時に書いた後は更新しない。original は 0、派生物は `media_assets.derived_generation` と同じ世代のコピーを現行とする。

同じ論理資産・同じ置き場に世代違いの行が並ぶので、一意の鍵は 2 つある。`(store, rel_path) WHERE state <> 'deleted'` は、生きているパスを 1 行に絞る。`(media_asset_id, store, generation) WHERE state <> 'deleted'` は、同じ置き場・同じ世代の生きたコピーを 1 行に絞る。後者が無いと、配信が選ぶコピーが決まらない。後から足すと溜まった違反を掃除する移行が要るので、最初から置く。

配信・一覧・派生物の有無を調べる読み手は `active_media_asset_copies` または `preferred_active_media_asset_copy` を使う。両者は active かつ現行世代のコピーだけを返す。CM 検出と TS 解析は original だけを読むので、世代の述語は要らない。削除・棚卸し・rescue・パス名前空間の検査は古い世代も含む全物理行を読む。

`missing_media_assets` の行は、走査が観測できなかった物理コピーの ID を主キーに持つ。コピーのパス・置き場・論理資産はコピー表から引く。同じ論理資産に複数の世代や置き場があっても、観測できなかったコピーを個別に記録でき、コピーが削除されると観測も cascade で消える。

コピーの無い論理資産が、未割り当てを表す。マイグレーションはコピー行を作らず、`store` が空のコピー行は CHECK が拒む（不変条件 10）。worker は起動時に、コピーの無い論理資産が 1 件でもあるか、`store` が空の観測行・進捗行や、範囲が空の `delete_reconcile` のブレーカーの行が 1 件でもあれば、起動を拒む。`orphan_files` などの観測表と `recording_ingest_progress.temp_store` は、旧 worker が列を指定せずに INSERT するので、空の既定値を残す。

`storage adopt <store-name>` は、コピーが一つも無い論理資産に `media_assets` の互換列からコピー行を作り、パスからそのコピーの世代を記録する。既存コピーのパス・状態・世代は書き換えず、論理カウンターは全コピーの最大世代と比べて大きい方に引き上げる。観測表と進捗行の空の `store`、範囲が空の `delete_reconcile` のブレーカーも、指定した置き場へ移す。実行の順序は [runbook/setup.md](../../runbook/setup.md) の「置き場の目印」にある。

旧 worker の `ON CONFLICT` を保つため、移行中は単一列の一意キーを残す。対象は `orphan_files(rel_path)` と `storage_sync(root)` である。新しい複合キーと並存するので、同じ `rel_path` を 2 つの置き場の孤児として記録できず、同じ `root` は 2 つの置き場の観測にならない。旧 worker を退役した後に単一列キーを外す。

### 世代と書き手

派生物の世代は、論理行のカウンターとコピーごとの値の 2 つで持つ。公開と登録の書き手が守る規則を、ここにまとめる。

`derived_generation` は派生物の次回採番に使う論理カウンターで、現行のコピーを決める値でもある。削除済みの行を再生成するときも進め、減らさない。派生物の公開は、世代が現行以上のときだけ論理行を更新する。同じ世代の再試行は通り、低い世代は 0 行になって公開が失敗する。低い世代を通して `derived_generation` が下がると、上の世代のコピーは `generation < derived_generation` の旧世代に入らないので削除されず、現行でもなくなるので配信から見えなくなる。配置先に現行世代のコピーが無いときも、カウンターは論理行から読み、書き手の置き場のコピーを根拠にしない。

コピーの `generation` は、移行が既存のコピーのパスから読んで埋める（original と印の無いパスは 0）。論理カウンターは移行では触らない。旧版の in-place 登録は世代の印付きのパスでもカウンターを 0 にしたので、移行の直後はコピーの世代がカウンターを上回り、現行にも旧世代にも入らない資産がありうる。`storage adopt` がカウンターを引き上げて解消するので、移行の後は必ず adopt を通す。

uncut の再生成パスは `.u{n}` にする。単調な `derived_generation` を uncut にも適用すると、再生成したパスに世代の印が要る。`.g{n}` は[カット版かどうか](../../configuration/cut.md)を表す印なので、uncut に使うとカット版と見分けられない。世代が正かどうかでカット版かどうかを決める案は、uncut の tombstone を再生成した行をカット版と誤判定する。

行を INSERT するのは worker（ingest / encode / thumbnail ジョブ）と in-place 登録（rescue / EPGStation 取り込み）で、衝突時は冪等に書く。削除済みへ遷移した後も行は消さない（tombstone）。ドロップ統計と元サイズは、原本削除後も UI で見られる。

in-place 登録は、論理資産が既にあるとき、次の 3 つを満たす場合だけ通す。この置き場に生きたコピー（`active` / `deleting` / `pending`）が無い。パスの世代が `COALESCE(derived_generation, 0)` と等しい。録画がごみ箱にない。満たさなければ `ErrConflict` で拒否し、論理資産が無ければ新規に登録する。

同じパスの `active` な再登録は何も変えずに通し、同じパスの `deleting` / `pending` は unlink される予定のファイルなので拒否する。パスの世代が古いファイルを受け付けると、登録したファイル自体が旧世代として overlap の後に消える。新しいファイルを受け付けると、そのコピーは現行にも旧世代にもならず、配信も削除もされない。論理行の世代は in-place 登録では動かさない。生きたコピーがある置き場へ足すと、配信が選ぶ行が決まらない。規則は 1 文の SQL に置き、並行する 2 本の登録は前述の一意索引が止める。

ingest が公開する原本には、同じトランザクションで進捗行の `temp_generation` を写す。移行前からある原本では NULL のままで、読み手は 0 として扱う。原本の行が世代を持つのは、再生の URL とキャッシュの鍵に使い、commit の後も録画中と同じ値を保つためである。他の種別には持たせない。`sha256` は未計測なら NULL とし、コピー単位には複製しない。

物理削除に至るアセットの 4 ソースと公開前孤児の回収は、1 本の削除 reconcile ループに集約し、一括削除サーキットブレーカーをループ全体に 1 つかける。アセットのソースは、ごみ箱の猶予超過、原本の保持を派生物の完備までとした録画の原本、ユーザーが外したエンコード結果、overlap を過ぎた旧世代コピーである。

実体ファイルの不在は、メディアアセットを指す衛星表が持つ。行の存在が、直前の走査で active なのに実体ファイルを観測できなかったという主張で、「観測できた」を表す行は作らない（不変条件 10）。書き手は削除 reconcile で、記録を書く worker ではないので、本体の列にしない（不変条件 13）。パスと種別は複製せず、読み出しで JOIN する（不変条件 9）。この表を根拠にメディアアセットを自動で消す経路は無い。判定基準と 2 つの安全弁（エイジング / 全損シグネチャ）は、[storage/missing-assets.md](../../storage/missing-assets.md)が権威である。

<details><summary>根拠</summary>

- 削除済みの tombstone の列: `state`、`deleted_at`
- ドロップ統計: `drop_stats`
- 原本の保持を派生物の完備までとした設定: `until_encoded`
- 実体ファイルの不在を持つ衛星表: `missing_media_assets`（物理コピー ID を PK かつ FK に取り、`ON DELETE CASCADE`）
- 複製しない列: `rel_path` / `kind`
- 走査時の状態: `state = 'active'`
- 移行時と adopt 時の世代同期: `TestMediaAssetCopiesMigrationBackfillsCopiesAndGenerations` / `TestAdoptUnassignedStoreRows`

</details>

### media_asset_cuts — カット版が実際に適用した区間

**カット版が実際に適用した区間は、チャプターが後から直せるので現在のチャプターから再計算できず、二度と再取得できない事実である**（不変条件 9）。行が無いことは、カット版ではないことを表す。

チャプターはユーザーが後から直せるので、現在のタイムラインから再計算すると、そのファイルがどう作られたかを復元できない。この表があることで、現在のタイムラインと突き合わせて「編集前の内容です」を判定できる。

```sql
CREATE TABLE media_asset_cuts (
    media_asset_id bigint PRIMARY KEY REFERENCES media_assets (id) ON DELETE CASCADE,
    keep_ranges    int8multirange NOT NULL CHECK (NOT isempty(keep_ranges))
);
```

主キーがメディアアセットの ID なのは、行の寿命が派生物の行と同時だからである（不変条件 12）。録画の行と同時に生まれて同時に死ぬのは、メディアアセットの側である。書き手も encode worker の 1 人で、同じトランザクションでパスと一緒に書くので、本体の列にしない（不変条件 13）。

残す区間は、原本の最初の映像フレームを 0 とするミリ秒の半開区間である（チャプターの区間と同じ単位）。境界はフレーム境界へ量子化済みである。空はカット版を作れないことを意味するので、CHECK で表現不可能にする（不変条件 10）。

値は JLSE のフレーム番号（最初の映像フレームが 0）から作る。ただし当てる側は、入力の最早 start_time を 0 とする秒として使う。カット版の切り出しとプレイヤーの現在時刻がそうである。放送 TS は音声が映像より先に始まるので、2 つの原点はその開始差だけずれる（大きさは未測定）。

行の不在は「カット版ではない」を表す（カットしないエンコード結果。不変条件 10）。API はこの不在を、エンコード結果の cut の偽に写す。「cut = false」を表す列をメディアアセットに足すと、2 つの主張が片方だけ古くなる。

<details><summary>根拠</summary>

- カットするプロファイル: `cut: true`
- 主キー: `media_asset_id`
- 同時に書くパス: `rel_path`、書き手 `EncodeWorker`
- 残す区間: `keep_ranges`、同じ単位の表 `recording_chapter_spans`
- カット版の切り出し: `internal/ffargs/cut.go`、プレイヤーの時刻: `currentTime`
- API への写し方: `encodedAssets[].cut`
- 足さない列: `cut = false`

</details>

### media_asset_thumbnail_seeks — サムネイルの抽出位置

**サムネイルの抽出位置は、そのファイルを実際に切り出した位置という事実である**（不変条件 9）。画像から取り直せず、書き手は thumbnail worker だけなので、衛星表に置く。位置が不明なものは、行が無いことで表す。

サムネイルの JPEG の内容から抽出時刻は取り直せない。書き手が thumbnail worker だけなので、本体の列にせず、同じトランザクションでアセットを INSERT / UPDATE する衛星表に置く（不変条件 12 / 13）。

```sql
CREATE TABLE media_asset_thumbnail_seeks (
    media_asset_id bigint PRIMARY KEY REFERENCES media_assets (id) ON DELETE CASCADE,
    seek_ms        bigint NOT NULL CHECK (seek_ms >= 0)
);
```

抽出位置は、チャプターの区間やカット版の残す区間と同じ、原本の時間軸のミリ秒である。サムネイルを作るたび、必ず 1 行書く。行の不在は、この表より前に作られ位置が不明なサムネイルだけを意味する。不明は行の不在で表し、NULL の列は使わない（不変条件 10）。

主キーと FK がメディアアセットを指すのは、行の寿命がサムネイルのアセットと同時だからである。カタログ export / rescue では運ばない。失われた位置は、チャプターがある録画なら、次の reconcile pass で選び直せる。

<details><summary>根拠</summary>

- 抽出位置の列: `seek_ms`
- 同じ時間軸の列: `recording_chapter_spans` / `media_asset_cuts.keep_ranges`

</details>
