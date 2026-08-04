> [recording.md](../recording.md) §3「Rokuban 側のコンポーネント」の一部。ruler は [ruler.md](ruler.md)、watcher は [watcher.md](watcher.md)、reconciler は [reconciler.md](reconciler.md)。

### 3.2 reconciler（宣言的同期）

`reservations`（desired）と `schedule_sync`（observed: `GET /api/recording/schedules` の観測結果）の差分を POST/DELETE で消す、レベルトリガーの宣言的同期ループ。

- **tags 対応付け**: mirakc schedule の `tags` に programId を埋め込む（例: `program:1234`）。手動で mirakc に入れられた schedule との判別もタグで可能。**M3-1 以前は reservation id を埋めていた**（`rokuban:reservation=1234`）が、`reservations.id` は ruler の導出削除・再実体化で変わりうる不安定な値だったため、EPG にある間ずっと安定な programId に変えた（issue #53「導出器が作るキーを宛先にしない」）。旧形式の schedule は下記「tags の不一致」の再作成でレベルトリガーに新形式へ移行する
- **contentPath 生成**: `recording.basedir` 相対パス必須。ファイル名テンプレートの展開もここで行う。生成はテンプレートから初回作成時のみ行い、以後の再作成（後述の差分反映）は observed（mirakc に登録済みの schedule）の contentPath を引き継ぐことで実質固定される（`reservations.base` に生成値を書き戻すコードは無い）
- **冪等**: 何度落ちても再実行で収束する。時刻精度もプロセス生存性も要求されない
- **終了済み番組は作らない**: 番組の終了時刻（`program_snapshots.start_at + duration_ms`）を過ぎた予約には `POST` しない。放置すると mirakc が数秒で `need-rescheduling` として failed にし、`recordings` に content_length=0 の failed 行を量産する（issue #134）。判定は下記「番組終了後の GC」とは別物の、never-scheduled の試行行の記録（`recordNeverScheduled`。issue #98）と同じ式・同じ材料を使う——ずらすと同じ予約が毎パス作成対象のまま残って POST を撃ち続ける

**reconciler はシングルトンではなく ruler と同じ形の River ジョブ**（`internal/worker` の `ReconcilePassWorker`。issue #24 M2-17）。周期的・冪等・パスを跨ぐ状態を持たない（サーキットブレーカーの閾値判定もパスごとに読み直す）という性質が ruler / epg_sync と同じなので、排他は advisory lock ではなくジョブロック + `UniqueOpts`（サイト単位）で担保する（[データ層](../data.md) §2）。

起動契機は ruler と同じ形で 3 つあるが、**定期パスが真実**で残り 2 つは投入を早めるヒントに過ぎない。ヒントを落としても定期パスが拾う。

| 契機 | 種別 |
|---|---|
| 定期（既定 30 秒） | **真実**。デプロイ形態に応じて River `PeriodicJobs` か k8s CronJob が投入する（[データ層](../data.md) §2） |
| 予約の作成 / 取消 | ヒント。api が予約の書き込みと**同一トランザクションで** `InsertTx` する（dual-write にならない） |
| ruler パスの完了 | ヒント。ruler_pass ワーカーがパス完了時に投入する（base が変われば mirakc に反映すべき差分が増えるため） |

ヒントは `UniqueOpts{ByArgs, ByState}` で合流する。予約を連続で作成/取消してもパスは 1 回で足りる。副産物として、予約の作成/取消が mirakc へ反映されるまでの待ち時間が最大 30 秒から実質即座になる。

#### ファイル名テンプレート

`filenameTemplate`（予約オプション。§8）は Go の [`text/template`](https://pkg.go.dev/text/template) 記法。reconciler が予約行のスナップショットだけを使って展開し（`internal/contentpath` パッケージ。`internal/reconciler/contentpath.go` の `buildContentPath` から呼ばれる）、拡張子は含まない前提で常に `.m2ts` を付す。未指定・空文字なら従来どおりの固定形式（`YYYYMMDD/HHMMSS_タイトル_サービスID.m2ts`）のまま（後方互換）。

**方針転換の理由**: 当初は EPGStation 互換の `%変数%` 記法で実装していたが、`text/template` に切り替えた。`%変数%` では変数名の誤り（`%TITEL%`）が黙って空文字になり、録画時に警告ログが出るだけで、ユーザーは数週間後にファイル名が崩れて初めて気づく。`text/template` なら**ルール作成/更新時にテンプレートを検証して 400 で弾ける**（`internal/api/rules.go` の `validateRuleInput` が `internal/contentpath.Validate` を呼ぶ。既存の正規表現検証と同じ場所・同じ形）。「未対応の変数は黙って空文字に置換して警告」という妥協した方針そのものが不要になった。

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

**非対応**: チャンネル名（EPGStation の `%CHNAME%` 相当）/ mirakc 内部 ID（`%CHID%` 相当）/ EPGStation の番組 ID（`%ID%` 相当）。いずれも予約行のスナップショットだけからは解決できず、mirakc への問い合わせや EPG プロジェクションの JOIN が要る。reconciler は mirakc に触れず（不変条件 1）ファイル I/O 専任という設計に反するため対応しない。`Data` に存在しないフィールドを参照するとテンプレートは無効になり、ルール作成/更新時点で 400 になる（後述）。

##### サニタイズと階層の規約

- `Title` / `Channel` / `ChannelType` は `internal/contentpath.NewData` の時点で `sanitizeComponent` を通した「1 パス成分に収まる」文字列になっている（ただし空文字は空文字のまま）。番組名に `/` が普通に入る（「A/B」等）ため、データ由来の `/` が区切りに昇格することはない
- **階層を作れるのはテンプレートに書かれた `/`（および `{{.StartAt.Format "2006/01"}}` のようにユーザーが明示的に書いた書式）だけ**
- **拡張子はテンプレートに含めない**。常に `.m2ts` を付す
- 展開結果は最後に必ず `internal/contentpath.SanitizeContentPath` を通すため、テンプレート自体に `..` や絶対パスが書かれていてもパストラバーサル・意図しない絶対パスにはならない
- 時刻は必ず JST で解決する（サーバーのタイムゾーン設定に依存させない）

##### ルール作成時の検証

`text/template` として `Parse` した後、サンプルデータに対して `Execute` まで行って初めて有効と判定する（`{{.Foo}}` のような未知フィールドは `Parse` では素通りし、`Execute` で初めてエラーになるため）。構文エラー・未知フィールドはどちらもルール作成/更新 API で 400 になる。

##### M3: EPGStation からの変換（`rokuban import epgstation`）

EPGStation の `recordedFormat`（`%変数%` 記法）を移行する際は、M3 の `rokuban import epgstation` で以下の変換表に従って `text/template` 記法へ機械的に変換する。

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

#### 予約オプションの差分反映

reconciler は存在の突き合わせだけでなく、**effective options と `schedule_sync.options`（mirakc の観測結果）の差分も消す**。ruler が base を毎パス再計算する以上、ルール編集で既存予約の effective が変わるため、これは編集 UI の前提ではなく **ruler の前提**である（issue #19）。

**mirakc に schedule の更新 API はない**（`GET` / `POST` / `GET{id}` / `DELETE{id}` の 4 つだけ）。反映は DELETE → POST の再作成になり、その間 schedule が存在しない窓ができる。そのため差分の対象を最小化する。

| フィールド | 差分対象 | 理由 |
|---|---|---|
| `priority` | **する** | チューナー調停の優先度。ルール編集・overrides 編集の実質的な唯一の変更対象 |
| `contentPath` | **しない**（observed の contentPath を引き継ぐ） | 下記 |
| `preFilters` / `postFilters` | M3 から | M1/M2 では常に空 |
| `logFilter` | しない | 未使用 |
| `tags` | **する**（不一致のときだけ） | 下記 |

**`tags` の不一致も再作成の契機にする。** 当初この表は「tags は reservation id で不変」として差分対象から外していたが、これは「同じ予約に対しては不変」であって「同じ番組に対して不変」ではない。予約が削除されて同じ番組に別の予約が作られると、mirakc 側の schedule には**古い `reservation_id` の tag が残る**。tags は ingest が record と予約を突き合わせる経路なので、古い tag のままだと録画が別の予約に紐付く。`priority` が一致していても tag が食い違えば再作成する（issue #19 のコメント）。

**M3-1 で tag を `program:{programId}` に変えた**（issue #53）ため、この不一致判定は「tags が新形式でない（旧形式のまま、または全く別の値）」に広がった。自分が作った schedule（`mirakc.IsOurs` が true）で、かつ `mirakc.FindProgramTag` の値が desired な `programId` と一致しないものすべてが再作成の対象になる。旧形式の schedule はこの分岐で新形式に移行する（新しい移行コードは書かず、既存の DELETE→POST 機構がレベルトリガーで移行を完了させる）。programId は EPG にある間ずっと安定なので、正しくタグ付けされた schedule が「同じ予約」の生存中に不一致を起こすことはない。

**差分の対象にするのは自分が作った schedule だけ。** tag のない schedule（mirakc を直接叩いた・別のツールが作った）は観測はするが触らない。外部が作った schedule と取り合いになるのを避けるためで、既存の DELETE 側と同じ判定（`mirakc.IsOurs` が false なら対象外。新旧いずれかの形式の tag があれば true）。

**`contentPath` は初回生成値を固定し、以後変更しない。** ただし固定の実体は `reservations.base` への書き戻しではない —— `base` / `reservations` の列に contentPath を焼く書き手は存在しない。実際に固定を実現しているのは、再作成時に observed の contentPath を引き継ぐこと（`internal/reconciler/reconciler.go` の `recreateSchedule`。下記「再作成の POST は observed の contentPath を引き継ぐ」参照）で、schedule が mirakc 側で外部に削除されて observed が無くなった場合（EPG が一度消えて再実体化した等）は、次パスがテンプレートから新規生成する通常の作成として扱われる —— 「固定」は「同一 schedule の再作成の間」だけ有効な機構上の性質であり、schedule 自体が消えて張り直された場合には及ばない。reconciler は番組名からパスを生成するため、EPG の番組名が変われば生成結果も変わる。これを差分と見なすと **EPG 更新のたびに schedule が消えて作り直される** churn になる。差分書き込みという設計は desired が安定していることを前提として要求する（同率 priority のタイを全順序で潰したのと同じクラスの問題。§3.1）。ファイル名を変えたい場合はユーザーが overrides で明示的に指定する。

ただしこの決定には未解決の一貫性の穴がある: **churn の原因は「テンプレートから生成された」パスが EPG の番組名変更で動くことで、ユーザーが `overrides.contentPath` に明示指定した値は動かない**。にもかかわらず現状は両者を区別せず差分対象外にしているため、既存予約の contentPath を上書きしても schedule には反映されない（priority も同時に変えれば道連れで反映される、という一貫性のない挙動になる）。`opts.ContentPath != nil` のときだけ差分対象にする改良が考えられるが、決定を変える話なので M2-4 では実装せず issue #19 のコメントに提起した。

**再作成の POST は observed の `contentPath` を引き継ぐ**（テンプレートから再生成しない）。「差分と見なさない」だけでは priority 変更で再作成するときに何を入れるかが決まらない。再生成すると EPG の番組名が変わっていれば別のパスになり、**priority を変えただけでファイル名が変わる**という副作用になる。引き継げば「初回生成値に固定し以後変更しない」が文字どおり保たれる。引き継ぐ値は自分が書いたものの往復だが、mirakc 側を直接触られていた場合の保険として `SanitizeContentPath` は通す。

#### 再作成のガード

**ガードは時刻の閾値ではなく状態で判定する。しかも blocklist ではなく allowlist にする — `state == "scheduled"` のときだけ再作成する。**

当初の決定は「`tracking` / `recording` の予約は触らない」という blocklist だった。allowlist にした理由は 2 つ（issue #19 のコメント）:

- `rescheduling`（延長追従中）も `finished` / `failed` も、削除して作り直してよい状態ではない
- mirakc が将来状態を増やしたとき、blocklist は**未知の状態を「触ってよい」側に落とす**。allowlist は安全側に倒れる

state の文字列は `internal/mirakc` に定数として置く（それまでどこにも定数化されていなかった）。持ち越した件数はログに出す — 黙って落とすと「収束しない」の原因が見えなくなる。

#### 1 パスの再作成数に上限を設ける

**ルールの priority を編集すると、マッチしている全予約が再作成対象になる。** N=200 なら 1 パスで 400 回の mirakc 呼び出しになる。当初の「再作成が走るのは『ユーザーが優先度を変えたとき』だけなので、この単純なガードで足りる」という見積もりは**予約単位の編集を想定していて、ルール単位の編集を数えていなかった**。

`MaxRecreatesPerPass`（既定 20）でレート制限し、レベルトリガーで数パスに分けて収束させる。持ち越した件数はログに出す（黙って切り捨てると「全部反映した」ように見える）。

これは `MaxDeletesPerPass`（大量削除サーキットブレーカー）とは**別の機構**である。ブレーカーは「導出された削除」を止めるもので超えたら**何も削除しない**、こちらは単なるレート制限で上限までは実行して残りを次パスに送る。**再作成の DELETE をブレーカーの数に混ぜない** — 混ぜるとルールの priority 一括変更でブレーカーが誤作動する（再作成は desired の消滅ではない）。

#### DELETE 成功 → POST 失敗

schedule が消えたまま次のパスまで残る。レベルトリガーで次パスが再作成するが、その間に開始時刻を越えると取りこぼす。**専用のカウンタメトリクス + `slog.Error`** で観測する。

当初の決定は「`quality_events` に記録する」だったが、**`quality_events` は `recordings` テーブルの列**で、まだ開始していない番組には recordings 行が存在しない。書く先がないので実装できない（issue #19 のコメント）。allowlist のガードにより再作成の対象は `scheduled`（開始まで間がある）だけなので、取りこぼしの窓自体は元々小さい。

**upstream への要望**: `priority` の部分更新 API があれば再作成の窓ごと消える。`RecordingOptions` 全体を差し替える汎用 PUT より通りやすく、mirakc 側が触る内部状態（スケジューラのキュー）も小さい。priority は開始前の schedule に対して mirakc のスケジューラが素直に扱える性質のフィールドでもある（§4.5 のとおり録画開始後は効かない）。#8 に調査メモとして残す。

#### 大量削除サーキットブレーカー

予約は「ルール x EPG」から導出されるため、EPG の一時欠損（mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に「不要」と判定し、reconciler がそれを mirakc へ忠実に反映（= 一斉 DELETE）してしまう。EPGStation#692（予約と録画が勝手に消える）はこの障害クラスの実例。

対策:

- **1 回の ruler パスでの削除数に閾値**（`ruler.max_deletes_per_pass`）を設け、超えたら削除を実行せず停止してアラート。手動確認後に再開
- **ブレーカーが数えるのは `toDelete`（既存予約のうち desired から外れた行）で、外れた理由が EPG の一時欠損かユーザーの明示操作かを区別しない。** desired は「(ルール勝者 − intent skip) ∪ investment（record 意図 ∪ overrides）」から導出されるため、ルールの削除・編集で勝者が変わる／intent skip を立てる／intent をクリアする（`DELETE .../intent`）／record 意図も勝者ルールも無く最後の investment だった overrides を消す（`DELETE .../overrides`、または全フィールドを reset する `PATCH`）といったユーザーの明示操作も同じ `toDelete` に混ざり、ラッチ中は他の導出削除と同様にカウント・保留される（非網羅）。ただし intent skip / intent クリアが削除候補に現れるのは、その番組に他の investment（overrides 等）が残っておらず（残っていれば desired が union で救い、DELETE 文自体の `NOT EXISTS (program_investments)` ガードでも救われる）、かつ EPG 射影にまだ番組がある（消えていれば `stillProjectedSubset` が凍結する）場合に限る。区別しないのは単純さを優先した設計判断で、代わりに影響件数の内訳を提示する確認 UI が安全装置になる
- **実害は経路によって異なる。** intent skip は `intent.action='skip'` により `effective.skip` を立てるため、ラッチ中に予約行が残っても `listDesired`（`db.EvaluateSyncCandidates` の `Skipped`）が同期対象から除外し、録画そのものは防がれる（実害は予約一覧の表示上の残留のみ。`TestReconciler_SkippedReservationNotScheduled` が固定）。**intent クリア（`DELETE .../intent`）はこの限りではない** —— `program_intents` の行を消すだけで `effective.skip` を立てないため、ラッチ中に残る予約行は `listDesired` から除外されず、既存 schedule も消えず、番組は録画され続ける。この経路の扱い（録画も止めるか、導出削除の対象から外すか、現状のまま許容するか）は未決（issue #154）
- **不変条件: 録画済みデータ（media_assets）に至る自動削除経路は retention reconcile のみ**。EPG・予約側の状態変化から録画物の削除に到達するパスを作らない
- programId が EPG から消えた予約は即削除せず猶予を置く（mirakc 自身も removed-from-epg を理由付き failed として通知してくる）。なお実装の `orphaned` state はこの用途ではなく「番組終了後に schedule が観測されなかった」を意味し、issue #98 以降は `recordings` に never-scheduled 行が存在するかどうかから読むたびに導出する（[schema.md](../schema.md) §3）

##### 止められる場所は ruler だけ（M2-5）

M1-4 では ruler と reconciler の両方に削除件数の閾値を置いていたが、**reconciler 側は誤発火しかしないので撤去した**（issue #2 のコメント）。reconciler が「消すべき schedule」と判断する経路は 3 つあり、reconciler からはどれも「desired に無い schedule がある」で区別できない:

| 経路 | ruler の `MaxDeletesPerPass` の対象か |
|---|---|
| ruler が EPG の変化から導出削除した | 対象。ruler のブレーカーが既に通している |
| ユーザーの明示操作（ルールの削除・編集で勝者が変わる、intent skip、intent クリア、最後の investment だった overrides の削除など）で desired から外れた | 対象。上記「大量削除サーキットブレーカー」のとおり区別せず同じ `toDelete` に混ざる |
| 番組終了後の GC が予約行を刈った | **対象外**。GC は `runGC` という別経路で `runPassForSite` の `toDelete` を通らない（下記「番組終了後の GC」） |

reconciler から見える 3 経路のうち 2 つは ruler のブレーカーで既に止められており、残る GC は時刻の比較だけで決定的に定まるためそもそも止める必要がない。reconciler にもう一段ブレーカーを置くと、この区別ができないまま GC の正常な一括削除（「長時間停止していた場合、再開後に溜まった期限切れ行を一括で消す」）にも誤発火する。

守る価値もない。**reconciler が DELETE する時点で「録画しない」決定は DB にコミット済み**である（不変条件「コミット = DB 行」）。誤りなら止めるべき場所は ruler で、reconciler で止めるのは「DB に合わせることを拒否する」ことにしかならず、mirakc に不要な schedule を残し続ける。

**ただし全損だけは別のシグネチャで守る。** 件数の閾値を外すと `listDesired` がバグや障害で空を返したときに自分が作った全 schedule を削除する経路が無防備になる。これは件数ではなく形で捕まえる:

```
desired が空 かつ 自分の tag が付いた schedule が 1 つ以上観測されている → 削除せず発動
```

GC・ユーザー操作では他の予約が残るので誤発火しない。全損だけを捕まえる（`breaker.ReconcileTotalLoss`）。

##### 発動はラッチ（M2-5）

M1-4 の骨格はパス内で完結していて、次のパスでは何も覚えていなかった。「手動確認後に再開」には**人間が確認するまで止まり続けるラッチ**が必要で、それはプロセスをまたぐ永続状態である（`circuit_breakers` 表。[schema.md](../schema.md) §3.6）。**レベルトリガー設計の中で数少ない導出できない状態** — 誰かが確認したという事実は再取得できない。

- **行の存在そのものが「発動中」**。停止していない状態を表す行は無い。再開は行の DELETE
- 件数が閾値以下に戻っても**自動では解けない**。EPG が回復して候補がゼロになれば実害はないが、自動復帰させると「一瞬止まって復帰した」がアラートに残らず、EPG が繰り返し欠損する状況を見逃す
- **止めるのは削除だけ。** 発動中でも予約の作成・base の更新・schedule の作成は続く（レベルトリガーで収束させたい他の差分は止めない）
- **GC は発動中でも動く**（下記「番組終了後の GC」の理由がそのまま効く）
- `detail` に「何が消されようとしていたか」の抜粋（最大 20 件の programId と題名）を焼く。**手動確認には対象が見える必要がある**
- 再開は `POST /api/sites/{site}/breakers/{name}/resume`（issue #102。資源の PK が `(site, name)` であることに合わせる）。`DELETE /api/sites/{site}/breakers/{name}` にしないのは、運用者から見た操作が「行を削除する」ではなく「確認したので再開する」だから（行が消えるのは実装詳細）

#### 番組終了後の GC

`reservations` / `program_intents` / `program_overrides` の物理削除（GC）は ruler の 1 パス内で、全サイト評価の後に 1 回だけ行う（`internal/ruler/ruler.go` の `runGC`）。Phase 1（#27）以降、実際に DELETE するのは `program_snapshots` の 1 表だけで、対象は `start_at + duration_ms < now() - 猶予` を満たす行（`reservations` の active/detached/orphaned を問わない）。`reservations` / `program_intents` / `program_overrides` はこの表への `(site, program_id)` FK が `ON DELETE CASCADE` なので、スナップショットが消えると 3 表とも一緒に落ちる（移行前は表ごとに別々の DELETE 文があった）。猶予には既存の `epg.retention_grace`（既定 24h、EPG プロジェクションのローリングウィンドウと同じ設定）をそのまま流用する。専用の設定項目を増やさず、「EPG から消える」と「予約・意図として GC される」の寿命を揃える。`recordings` は `reservations` への FK を持たない（`reservation_id` 列は issue #158 で削除済み）ので、この削除で録画履歴（recordings/media_assets。issue #98 の never-scheduled 行を含む）が失われることはない。

**GC は大量削除サーキットブレーカー（`MaxDeletesPerPass`）の対象にしない。** ブレーカーが守るのは「ルール x EPG」の評価結果から導出される削除だけで、EPG の一時的な欠損・フリッカーに引きずられて予約を大量に消してしまう事故（上記 EPGStation#692 のクラス）を防ぐためのもの。GC の削除対象は時刻の比較だけで決定的に定まり、EPG の状態には一切左右されない。むしろ reconciler/ruler が長時間停止していた場合、再開後に溜まった期限切れ行を一括で消すのは正常な挙動であり、ここをブレーカーで止めると実害のない削除が積み上がり続けるだけになる。

