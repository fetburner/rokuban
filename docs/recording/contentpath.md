> [recording.md](../recording.md) §3.2「reconciler」の一部（ファイル名テンプレート）。索引から辿る。

#### ファイル名テンプレート

mirakc に渡す録画の contentPath（ファイル名）は、ユーザーが書いたテンプレートを、Go の標準のテンプレート記法として予約のスナップショットだけで展開して決める。未指定なら既定を使う。ルールの作成・更新時に検証し、誤りは 400 で弾くので、ファイル名の誤りに数週間後に気づくことが無い。

テンプレートが決めるのは mirakc に渡す contentPath だけである。Rokuban が保存する原本のパスは `sites/{site}/{uuid}/{basename}` で、basename だけを使う。テンプレートのディレクトリ部分は最終パスに残らない。録画ごとに再利用されない path にするためで、理由は [storage/contract.md](../storage/contract.md) の「rel_path の名前空間」にある。

記法は Go の標準のテンプレート記法である。展開するのは reconciler で、テンプレートは拡張子を含まない前提で、展開結果の末尾には常に拡張子を付す。このテンプレートは予約オプションの 1 つで、[reservation-model.md](reservation-model.md) §4.2 の一覧表にある。

既定のテンプレートは、見た目が日付ディレクトリの下に「時刻_タイトル_サービスID」を置く形と同じである。既定も他のテンプレートと同じく JST で解決する。そのため、サーバーのタイムゾーンが JST 以外の環境では既定のパスが変わる。

Go の標準のテンプレート記法を採る理由は、ルール作成・更新時にテンプレートを検証して 400 で弾けることである。検証は、既存の正規表現の検証と同じ場所で、同じ形で行う。変数名の誤りが気づかないうちに空文字になる記法だと、ユーザーは数週間後にファイル名が崩れて初めて気づく。

<details><summary>根拠</summary>

- 記法: [`text/template`](https://pkg.go.dev/text/template)
- 展開と呼び出し元: `internal/contentpath` パッケージ。`internal/reconciler` の `buildContentPath` から呼ばれる
- 予約オプション名: `filenameTemplate`
- 拡張子: `.m2ts`
- 既定: `DefaultTemplate`（`YYYYMMDD/HHMMSS_タイトル_サービスID.m2ts`）
- 検証: `internal/api/rules.go` の `validateRuleInput` が `internal/contentpath.Validate` を呼ぶ

</details>

##### 使えるフィールド

使えるのは、開始時刻、年月日時分秒、曜日、番組名、物理チャンネル、サービス ID、チャンネル種別である。局名はまだ足しておらず、mirakc と EPGStation の ID は、スナップショットから解決できないので対応しない。

存在しないフィールドを参照するとテンプレートは無効になり、ルール作成・更新時点で 400 になる（後述）。フィールドは `internal/contentpath.Data` の公開フィールドに対応する。

| フィールド | 値 | 出所 |
|---|---|---|
| `{{.StartAt}}` | 開始時刻（JST の `time.Time`） | `program_snapshots.start_at` |
| `{{.Year}}` | 4 桁年（JST） | 同 |
| `{{.ShortYear}}` | 2 桁年（JST） | 同 |
| `{{.Month}}` `{{.Day}}` `{{.Hour}}` `{{.Min}}` `{{.Sec}}` | 2 桁ゼロ埋め（JST） | 同 |
| `{{.DOW}}` | 曜日（`日`〜`土`） | 同 |
| `{{.Title}}` | 番組名（サニタイズ済み） | `program_snapshots.title` |
| `{{.Channel}}` | 物理チャンネル（同上） | `program_snapshots.channel` |
| `{{.ServiceID}}` | サービス ID | `program_snapshots.service_id` |
| `{{.ChannelType}}` | チャンネル種別（同上） | `program_snapshots.channel_type` |

`{{.StartAt.Format "2006-01"}}` のように、開始時刻には任意の書式を書ける。例を示す。

```
{{.Year}}/{{.Month}}/{{.Title}}_{{.Hour}}{{.Min}}
```

##### 局名の扱い

局名は技術的な障害が無く、解決はできる。ただし、まだフィールドとして足していない。足すかどうかは、足す PR で決める。ここで先に形だけ固定しない（不変条件 11: フィールドは、それを書く・使うコードと同じ PR で決める）。

局名は EPGStation の `%CHNAME%` 相当である。`program_snapshots.service_name` は NOT NULL で存在し、`buildContentPath` がその場で持っている。足す先は `Data` である。

##### 解決できない ID

mirakc 内部の ID と EPGStation の番組 ID は、予約行のスナップショットだけからは解決できないので対応しない。解決には mirakc への問い合わせや EPG プロジェクションの JOIN が要る。reconciler は mirakc に触れず（不変条件 1）、ファイル I/O 専任という設計に反する。

mirakc 内部 ID は `%CHID%` 相当、EPGStation の番組 ID は `%ID%` 相当である。

##### サニタイズと階層の規約

番組名などデータ由来の値に区切りの文字が入っても、パスの区切りにはならない。contentPath で階層を作れるのは、テンプレートにユーザーが明示的に書いた区切りだけで、展開結果は最後に必ずパスの正規化を通る。

番組名にはスラッシュが普通に入る（「A/B」等）ので、データ由来のスラッシュが区切りに昇格することは無いようにしてある。テンプレートに書かれた `/` と、`{{.StartAt.Format "2006/01"}}` のようにユーザーが書式へ明示的に書いた `/` が、mirakc 側の contentPath の階層になる。保存先の階層にはならない（上の「保存先」を参照）。

`Title` / `Channel` / `ChannelType` は、`internal/contentpath.NewData` の時点で `sanitizeComponent` を通し、1 つのパス成分に収まる文字列にする。ただし空文字は空文字のままである。

展開結果は最後に必ず `internal/contentpath.SanitizeContentPath` を通す。そのため、テンプレート自体に `..` や絶対パスが書かれていても、パストラバーサルや意図しない絶対パスにはならない。拡張子はテンプレートに含めず、常に `.m2ts` を付す。時刻は必ず JST で解決し、サーバーのタイムゾーン設定には依存させない。

##### ルール作成時の検証

検証は、構文を解析した後、サンプルデータに対して展開まで行って初めて有効と判定する。未知のフィールドは解析ではすり抜け、展開で初めてエラーになるからである。構文エラーも未知フィールドも、ルール作成・更新 API で 400 になる。

解析は `Parse`、展開は `Execute` である。`{{.Foo}}` のような未知フィールドが `Parse` を通り、`Execute` で初めて落ちる。

##### EPGStation からの変換（`rokuban import epgstation`）

移行コマンドは、EPGStation のファイル名の書式を、次の表に従ってテンプレート記法へ機械的に変換する。局名・チャンネル ID・番組 ID の変数は、予約のスナップショットだけからは解決できないので未対応である（上の「局名の扱い」「解決できない ID」を参照）。

コマンドは `rokuban import epgstation --rules` で、変換元は EPGStation の `recordedFormat`（`%変数%` 記法）、変換先は `text/template` 記法である。未対応の変数は `%CHNAME%` / `%CHID%` / `%ID%` である。

| EPGStation | Rokuban |
|---|---|
| `%YEAR%` | `{{.Year}}` |
| `%SHORTYEAR%` | `{{.ShortYear}}` |
| `%MONTH%` | `{{.Month}}` |
| `%DAY%` | `{{.Day}}` |
| `%HOUR%` | `{{.Hour}}` |
| `%MIN%` | `{{.Min}}` |
| `%SEC%` | `{{.Sec}}` |
| `%DOW%` | `{{.DOW}}` |
| `%TITLE%` | `{{.Title}}` |
| `%CH%` | `{{.Channel}}` |
| `%SID%` | `{{.ServiceID}}` |
| `%TYPE%` | `{{.ChannelType}}` |
| `%CHNAME%` / `%CHID%` / `%ID%` | 未対応 |

