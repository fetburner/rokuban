> [recording.md](../recording.md) §3.2「reconciler」の一部（ファイル名テンプレート）。索引から辿る。

#### ファイル名テンプレート

`filenameTemplate`（予約オプション。[reservation-model.md](reservation-model.md) §4.2 の一覧表参照）は Go の [`text/template`](https://pkg.go.dev/text/template) 記法。reconciler が予約行のスナップショットだけを使って展開し（`internal/contentpath` パッケージ。`internal/reconciler` の `buildContentPath` から呼ばれる）、拡張子は含まない前提で常に `.m2ts` を付す。未指定・空文字なら既定の `DefaultTemplate`（見た目は `YYYYMMDD/HHMMSS_タイトル_サービスID.m2ts` と同じ）を使う。既定も他の template と同じく JST で解決するため、サーバー TZ が JST 以外の環境では既定パスが変わる。

`text/template` を採る理由は、**ルール作成/更新時にテンプレートを検証して 400 で弾ける**こと（`internal/api/rules.go` の `validateRuleInput` が `internal/contentpath.Validate` を呼ぶ。既存の正規表現検証と同じ場所・同じ形）。変数名の誤りが黙って空文字になる記法だと、ユーザーは数週間後にファイル名が崩れて初めて気づく（末尾「経緯と失敗事例」参照）。

##### 使えるフィールド

`internal/contentpath.Data` の公開フィールドに対応する。

| フィールド | 値 | 出所 |
|---|---|---|
| `{{.StartAt}}` | 番組開始時刻（JST の `time.Time`）。`{{.StartAt.Format "2006-01"}}` のように任意の書式を書ける | `program_snapshots.start_at` |
| `{{.Year}}` | 4 桁年（JST） | 同 |
| `{{.ShortYear}}` | 2 桁年（JST） | 同 |
| `{{.Month}}` `{{.Day}}` `{{.Hour}}` `{{.Min}}` `{{.Sec}}` | 2 桁ゼロ埋め（JST） | 同 |
| `{{.DOW}}` | 曜日（`日`〜`土`） | 同 |
| `{{.Title}}` | 番組名（パス成分としてサニタイズ済み） | `program_snapshots.title` |
| `{{.Channel}}` | 物理チャンネル（同上） | `program_snapshots.channel` |
| `{{.ServiceID}}` | サービス ID | `program_snapshots.service_id` |
| `{{.ChannelType}}` | チャンネル種別（同上） | `program_snapshots.channel_type` |

例:

```
{{.Year}}/{{.Month}}/{{.Title}}_{{.Hour}}{{.Min}}
```

**非対応（解決はできるが、まだ足していない）**: 局名（EPGStation の `%CHNAME%` 相当）。`program_snapshots.service_name` は NOT NULL で存在し `buildContentPath` はその場で持っているので、**技術的な障害はない**。`Data` に足すかどうかは「足す PR」で決める（不変条件 11: フィールドは、それを書く/使うコードと同じ PR で決める）。ここで先に形だけ固定しない。

**非対応（スナップショットからは解決できない）**: mirakc 内部 ID（`%CHID%` 相当）/ EPGStation の番組 ID（`%ID%` 相当）。予約行のスナップショットだけからは解決できず、mirakc への問い合わせや EPG プロジェクションの JOIN が要る。reconciler は mirakc に触れず（不変条件 1）ファイル I/O 専任という設計に反するため対応しない。

`Data` に存在しないフィールドを参照するとテンプレートは無効になり、ルール作成/更新時点で 400 になる（後述）。

##### サニタイズと階層の規約

- `Title` / `Channel` / `ChannelType` は `internal/contentpath.NewData` の時点で `sanitizeComponent` を通した「1 パス成分に収まる」文字列になっている（ただし空文字は空文字のまま）。番組名に `/` が普通に入る（「A/B」等）ため、データ由来の `/` が区切りに昇格することはない
- **階層を作れるのはテンプレートに書かれた `/`（および `{{.StartAt.Format "2006/01"}}` のようにユーザーが明示的に書いた書式）だけ**
- **拡張子はテンプレートに含めない**。常に `.m2ts` を付す
- 展開結果は最後に必ず `internal/contentpath.SanitizeContentPath` を通すため、テンプレート自体に `..` や絶対パスが書かれていてもパストラバーサル・意図しない絶対パスにはならない
- 時刻は必ず JST で解決する（サーバーのタイムゾーン設定に依存させない）

##### ルール作成時の検証

`text/template` として `Parse` した後、サンプルデータに対して `Execute` まで行って初めて有効と判定する（`{{.Foo}}` のような未知フィールドは `Parse` では素通りし、`Execute` で初めてエラーになるため）。構文エラー・未知フィールドはどちらもルール作成/更新 API で 400 になる。

##### EPGStation からの変換（`rokuban import epgstation`）

`rokuban import epgstation --rules` は EPGStation の `recordedFormat`（`%変数%` 記法）を移行する。変換は以下の表に従って `text/template` 記法へ機械的に行う。

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
| `%CHNAME%` / `%CHID%` / `%ID%` | **未対応**（予約行のスナップショットだけからは解決できない。上記「非対応」参照） |

---

#### 経緯と失敗事例

- **`%変数%` 記法からの方針転換**: 当初は EPGStation 互換の `%変数%` 記法で実装していたが、`text/template` に切り替えた。`%変数%` では変数名の誤り（`%TITEL%`）が黙って空文字になり、録画時に警告ログが出るだけで、ユーザーは数週間後にファイル名が崩れて初めて気づく。`text/template` ならルール作成/更新時にテンプレートを検証して 400 で弾けるため、「未対応の変数は黙って空文字に置換して警告」という妥協した方針そのものが不要になった
