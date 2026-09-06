> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§9 / §9.5）。

## 9. epg_services / epg_programs — EPG プロジェクション（使い捨てキャッシュ）

DDL の権威は `internal/db/migrations`（`epg_services` / `epg_programs` テーブル定義）。**真実は常に mirakc**であり、これは
レベルトリガーでいつでも全量再構築できる使い捨てキャッシュ。永続資産（reservations /
recordings / media_assets）とは寿命が違うためスキーマ v1 から分離している。

存在理由は不変条件「**api ロールは EPG を含むすべてのデータ読み取りを Postgres だけで
完結させる**」。api が mirakc に問い合わせるパスを作らないため、プロジェクションは
**プロダクトが画面に描画するものを全部持つ**（= UI 完全形）。逆に mirakc の運用状態
（生 EIT/SI、チューナー状態）は入れない。

```sql
CREATE TABLE epg_services (
    site                  text    NOT NULL,
    network_id            integer NOT NULL,
    service_id            integer NOT NULL,
    type                  integer NOT NULL,
    logo_id               integer NOT NULL,
    remote_control_key_id integer NOT NULL,
    name                  text    NOT NULL,
    channel_type          text    NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel               text    NOT NULL,
    has_logo_data         boolean NOT NULL DEFAULT false,
    observed_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, network_id, service_id)
);

CREATE TABLE epg_programs (
    site        text    NOT NULL,
    program_id  bigint  NOT NULL,
    network_id  integer NOT NULL,
    service_id  integer NOT NULL,
    event_id    integer NOT NULL,
    start_at    timestamptz NOT NULL,
    duration_ms bigint  NOT NULL,
    end_at      timestamptz NOT NULL,   -- start_at + duration_ms（同期時にアプリが計算）
    is_free     boolean NOT NULL DEFAULT true,
    name        text    NOT NULL DEFAULT '',
    description text    NOT NULL DEFAULT '',
    genre_lv1   smallint[] GENERATED ALWAYS AS (genre_lv1_of(genres)) STORED,
    extended    jsonb,   -- 拡張形式イベント（出演者等）
    genres      jsonb,   -- lv1 / lv2 / un1 / un2 の全量
    video       jsonb,   -- 映像属性
    audios      jsonb,   -- 音声属性
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);
```

- **site を含むキーで切る**: 地上波のチャンネル構成はサイトの地域ごとに異なる
- **クエリ軸は型付きカラム、詳細は jsonb**: サービス / 時間範囲 / ジャンル / 無料が型付き。
  `genre_lv1` は絞り込み用に lv1 だけを重複なく取り出した配列（GIN）で、`recordings` と同じ関数で導出する。詳細は `genres` jsonb 側
- **`end_at` は生成列にしない**: `timestamptz + interval` が STABLE（TimeZone 設定に依存）で
  IMMUTABLE を要求する生成列に使えないため、同期時にアプリが計算して書く
- **pg_trgm GIN** を `name` / `description` に張る。LIKE だけでなく正規表現マッチ（`~`）も
  加速するので、ruler のルール評価が同じインデックスに乗る
- **autovacuum を既定より積極的に**: 全量 upsert を繰り返すため churn が大きい
- `has_logo_data` / `logo_id` は mirakc の `Service` 構造体をそのまま射影しているだけの列。
  局ロゴの再取得・自前配信はドロップした（[データ層](../data.md)「サービスロゴ: ドロップ」）が、
  この 2 列の削除は不要

### 同期と有界性

worker ロールの River 定期ジョブ `epg_sync`（既定 10 分間隔、専用キューで同時実行 1）が
`GET /api/services` と `GET /api/programs` を全量取得して upsert する。差分同期はしない。

1 パスで 2 種類の削除をする:

| 削除 | 条件 | 意図 |
|---|---|---|
| stale スイープ | `observed_at < 基準時刻` | mirakc から消えた行（番組編成変更・サービス削除）を落とす |
| ローリングウィンドウ | `end_at < 基準時刻 - retention_grace`（既定 24h） | 放送済み番組を刈り取る。**永遠に太らない** |

削除で足を踏み外さないための規律が 3 つある。

- 基準時刻は**アプリのクロックではなく `SELECT now()` で DB から取る**。クロックスキューで
  基準時刻が全 `observed_at` より後になると、毎パスでプロジェクション全体を消して
  再投入する churn 事故になる
- **番組の stale スイープは物理チャンネル単位に絞る**。mirakc の EPG 収集
  （`jobs.update-schedules` = 1 回チューニングして `collect-eits --sids=<そのチャンネルの SID>`）は
  **物理チャンネルごと**に実行され、既定では 1 日 2 回・timeout 10 分。録画とのチューナー競合や
  timeout で 1 チャンネルだけ番組が返らないことがある。サイト単位でスイープすると
  そのチャンネルの番組表だけが消えるため、**今回番組を返したチャンネルに属するサービスの行しか
  削除しない**。対象チャンネルのサービスは番組 0 件のものも含める（マルチ編成が終わった
  サブサービスの古い行を消せるようにするため。サービス単位に絞ると残ってしまう）
- **mirakc が空を返したらスイープを見送る**。再起動直後は EPG を読み込み終えておらず
  空リストを返しうる。そのままスイープすると番組表が丸ごと消え、次の同期までの数分間
  UI が空になる。削除しなくても次のパスが収束させる（レベルトリガー）。
  大量削除の前で立ち止まる規律は reconciler のサーキットブレーカーと同じ

`epg_programs` に channel 列は持たないので、スイープは `(network_id, service_id)` で対象を指す。
クエリは `network_id` ごとに分けて呼ぶ（1 つの TS は 1 つの original_network_id を持つので
チャンネルより粗くならず、可変長の組を SQL に渡さずに済む）。

なお `observed_at` には**意図的にインデックスを張っていない**。この列は毎パスで全行が
更新されるため、インデックスがあると HOT update が一切効かなくなりブロートが悪化する。
スイープの seq scan より、更新経路を HOT に保つ方が churn の総量では有利。

投影できない行は捨てて同期は続行する（1 行で全体を落とさない）:

- `startAt` がない番組 — 時間軸に置けない
- **`name` がない番組** — サブサービスの影の行（後述）
- `channel_type` が GR/BS/CS/SKY 以外のサービス — CHECK 制約に載らない

### サブサービス: 番組は絞る、サービスは絞らない

地上波は 1 物理チャンネルが複数サービスに分かれる（`ＮＨＫ総合１/２`、`ＲＮＣ西日本テレビ１/２/３` 等）。
マルチ編成でないとき、サブサービス側は**同じ eventId で `name` が null の影の行**として
返るため、1 番組が 2〜3 行に重複する。実測では 7139 件中 4459 件が影の行だった。

- **番組は `name` を持つ行だけ投影する**（7139 → 2680、62% 減）。EPG 射影の唯一の実性能懸念
  である churn / autovacuum 追従に直接効く
- **サービスは全件投影する**。19 行で churn が無視でき、マルチ編成を持つサブサービス
  （`ＮＨＫ総合２` の高校野球等）を隠せない。番組のない空列を隠すのは UI の関心事とし、
  「そのサービスに窓内の番組が 1 件でもあるか」で判定する

影の行を落としても録れなくなる番組はない。mirakc の録画は `filter-program --sid --eid` で
サービス単位に絞るため、影の行を予約しても得られるものは同じ shared グループの親と同じか空。
逆にマルチ編成の実番組は `name` を持つのでこのフィルタを通り、予約・録画できる。

EPGStation は `relatedItems` を見る `isMainProgram()` と name チェックの二段で同じことを
しているが、実データ 7139 件で「`name` があって shared の main でない番組」が 0 件だったため、
`name` の有無だけで同一の結果になる（`relatedItems` の移植は不要）。

予約・録画はこのテーブルに依存しない。予約の GC は `program_start_at + program_duration_ms`
で判定し（§3）、録画した番組情報は recordings に非正規化スナップショットされる（§5）。
だから刈り取りが永続資産を壊すことはない。

## 9.5 tuner_sync — チューナー射影（使い捨てキャッシュ）

DDL の権威は `internal/db/migrations`（`tuner_sync` テーブル定義）。mirakc の `GET /api/tuners` の観測結果で、
`epg_services` / `epg_programs` と同じ**使い捨てプロジェクション**。真実は常に mirakc 側にあり、
レベルトリガーでいつでも全量再構築できる。容量超過の判定（[データ層](../data.md) §6.5）が使う。

存在理由は不変条件 1（api ロールは mirakc に問い合わせない）。EPGStation のように起動時に
`/api/tuners` を叩いて in-memory に持つ形は取れない。チューナーの**対応種別が必須**
（GR 専用チューナーに BS は載らない）なので、本数を設定に手書きする案も成立しない。

```sql
CREATE TABLE tuner_sync (
    site          text    NOT NULL,
    tuner_index   integer NOT NULL,   -- mirakc のレスポンスの index
    name          text    NOT NULL,
    types         text[]  NOT NULL CHECK (types <@ ARRAY['GR','BS','CS','SKY']),
    is_available  boolean NOT NULL,
    is_fault      boolean NOT NULL,
    observed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, tuner_index)
);
```

- **投影しないもの**: `users` / `isFree` / `isUsing` / `command` / `pid`。**現在の利用者は容量から
  引かない** --- 一時的な占有であり将来の区間の容量とは無関係で、「見えない消費者は数えない =
  下界を主張する」性質と一貫する。`internal/mirakc.Tuner` にもこれらのフィールドを持たせない
  （フィールドがあると投影する経路ができる）
- **PK は `(site, name)` ではなく `(site, tuner_index)`。**
  `name` は運用者が付ける値なので同名が 2 本あると upsert で 1 行に潰れ、`cap(A)` を過少に
  数えて**警告が過剰に出る方向へ恒久的にずれる**（毎パス上書きされるので自己修復しない）。
  [データ層](../data.md) §6.5 は既知の盲点がすべて「警告を見逃す」方向に偏っていることを設計上の
  性質として挙げており、これはその性質を崩す。`tuner_index` は並び替えで振り直されるが、
  毎パス全量再構築するので誤りが残らない
- `types` に `cardinality > 0` の CHECK は置かない。空配列のチューナーは `cap(A)` に数えられない
  だけで無害であり、想定外の上流データで同期パス全体を失敗させる方が損。重複・順序の正規化も
  `cap(A)` が集合の交差判定なので不要（`rules.encode_profiles` のような正規集合チェックは要らない）
- **行が 1 行も無いサイトでは容量判定が何も主張しない**（[データ層](../data.md) §6.5「実装で決めたこと」）。
  「0 本」と「まだ同期していない」を射影から区別できないため

## 経緯と失敗事例

- EPG プロジェクションは M1-6、`tuner_sync` は M2-10 の成果物。
  存在理由の元 issue は #3、サブサービスの扱いは issue #17、大量削除で立ち止まる規律は issue #11
- **`tuner_sync` の PK**: issue #21 は投影列に `index` を挙げているのに DDL 案が持たず
  PK が `name` になっていた。この不整合を `index` を採る側で解消した（本文 §9.5 の理由）
