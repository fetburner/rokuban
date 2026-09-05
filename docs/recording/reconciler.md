> [recording.md](../recording.md) §3「Rokuban 側のコンポーネント」の一部。索引から辿る。ファイル名テンプレートは [contentpath.md](contentpath.md)、大量削除サーキットブレーカーは [breaker.md](breaker.md)。

### 3.2 reconciler（宣言的同期）

`reservations`（desired）と `schedule_sync`（observed: `GET /api/recording/schedules` の観測結果）の差分を POST/DELETE で消す、レベルトリガーの宣言的同期ループ。

- **tags 対応付け**: mirakc schedule の `tags` に programId を埋め込む（例: `program:1234`）。手動で mirakc に入れられた schedule との判別もタグで可能。programId は EPG にある間ずっと安定している。reservations 行は ruler の判断で削除・再作成されることがあるため、tag には reservations 側の列ではなく programId を使う（不変条件 9「導出器が作るキーを宛先にしない」）
- **contentPath 生成**: `recording.basedir` 相対パス必須。ファイル名テンプレート（[contentpath.md](contentpath.md)）の展開もここで行う。生成はテンプレートから初回作成時のみ行い、以後の再作成（後述の差分反映）は、明示 override（`overrides.contentPath`）があればその値、無ければ observed（mirakc に登録済みの schedule）の contentPath を引き継ぐことで実質固定される（`reservations.base` に生成値を書き戻すコードは無い）
- **冪等**: 何度落ちても再実行で収束する。時刻精度もプロセス生存性も要求されない
- **終了済み番組は作らない**: 番組の終了時刻（`program_snapshots.start_at + duration_ms`）を過ぎた予約には `POST` しない。放置すると mirakc が数秒で `need-rescheduling` として failed にし、`recordings` に content_length=0 の failed 行を量産する。判定は「番組終了後の GC」（[ruler.md](ruler.md)）とは別物の、`never_scheduled_events` への欠測記録（`recordNeverScheduled`）と同じ式・同じ材料を使う——ずらすと同じ予約が毎パス作成対象のまま残って POST を撃ち続ける

**reconciler はシングルトンではなく ruler と同じ形の River ジョブ**（`internal/worker` の `ReconcilePassWorker`）。周期的・冪等・パスを跨ぐ状態を持たない（サーキットブレーカーの閾値判定もパスごとに読み直す）という性質が ruler / epg_sync と同じなので、排他は advisory lock ではなくジョブロック + `UniqueOpts`（サイト単位）で担保する（[データ層](../data.md) §2）。

起動契機は ruler と同じ形で 3 つあるが、**定期パスが真実**で残り 2 つは投入を早めるヒントに過ぎない。ヒントを落としても定期パスが拾う。

| 契機 | 種別 |
|---|---|
| 定期（既定 30 秒） | **真実**。デプロイ形態に応じて River `PeriodicJobs` か k8s CronJob が投入する（[データ層](../data.md) §2） |
| 予約の作成 / 取消 | ヒント。api が予約の書き込みと**同一トランザクションで** `InsertTx` する（dual-write にならない） |
| ruler パスの完了 | ヒント。ruler_pass ワーカーがパス完了時に投入する（base が変われば mirakc に反映すべき差分が増えるため） |

ヒントは `UniqueOpts{ByArgs, ByState}` で合流する。予約を連続で作成/取消してもパスは 1 回で足りる。副産物として、予約の作成/取消が mirakc へ反映されるまでの待ち時間が最大 30 秒から実質即座になる。

#### 予約オプションの差分反映

reconciler は存在の突き合わせだけでなく、**effective options と `schedule_sync.options`（mirakc の観測結果）の差分も消す**。ruler が base を毎パス再計算する以上、ルール編集で既存予約の effective が変わるため、これは編集 UI の前提ではなく **ruler の前提**である。

**mirakc に schedule の更新 API はない**（`GET` / `POST` / `GET{id}` / `DELETE{id}` の 4 つだけ）。反映は DELETE → POST の再作成になり、その間 schedule が存在しない窓ができる。そのため差分の対象を最小化する。

| フィールド | 差分対象 | 理由 |
|---|---|---|
| `priority` | **する** | チューナー調停の優先度。ルール編集・overrides 編集の実質的な唯一の変更対象 |
| `contentPath` | **`overrides.contentPath` が明示指定されているときだけする**（テンプレート生成値はしない） | 下記 |
| `preFilters` / `postFilters` | 現状しない | 常に空で運用しており差分が生じない |
| `logFilter` | しない | 未使用 |
| `tags` | **する**（不一致のときだけ） | 下記 |

**`tags` の不一致も再作成の契機にする。** tags は ingest が record と予約を突き合わせる経路で、schedule に古い・別の値の tag が残っていると録画が別の予約に紐付くため、`priority` が一致していても tag が食い違えば再作成する。不一致判定は「tags の `program:{programId}` が desired な programId と一致するか」: 自分が作った schedule（`mirakc.IsOurs` が true）で、かつ `mirakc.FindProgramTag` の値が desired な `programId` と一致しないものすべてが再作成の対象になる。programId は EPG にある間ずっと安定なので、正しくタグ付けされた schedule が「同じ予約」の生存中に不一致を起こすことはない。

**差分の対象にするのは自分が作った schedule だけ。** tag のない schedule（mirakc を直接叩いた・別のツールが作った）は観測はするが触らない。外部が作った schedule と取り合いになるのを避けるためで、既存の DELETE 側と同じ判定（`mirakc.IsOurs` が false なら対象外。rokuban の tag があれば true）。

**テンプレート生成の `contentPath` は初回生成値を固定し、以後変更しない**（`overrides.contentPath` で明示指定した値は差分反映の対象。下記参照）**。** ただし固定の実体は `reservations.base` への書き戻しではない —— `base` / `reservations` の列に contentPath を焼く書き手は存在しない。実際に固定を実現しているのは、再作成時に observed の contentPath を引き継ぐこと（`internal/reconciler/reconciler.go` の `recreateSchedule`。下記「再作成の POST は observed の contentPath を引き継ぐ」参照）で、schedule が mirakc 側で外部に削除されて observed が無くなった場合（EPG が一度消えて再実体化した等）は、次パスがテンプレートから新規生成する通常の作成として扱われる —— 「固定」は「同一 schedule の再作成の間」だけ有効な機構上の性質であり、schedule 自体が消えて張り直された場合には及ばない。reconciler は番組名からパスを生成するため、EPG の番組名が変われば生成結果も変わる。これを差分と見なすと **EPG 更新のたびに schedule が消えて作り直される** churn になる。差分書き込みという設計は desired が安定していることを前提として要求する（同率 priority のタイを全順序で潰したのと同じクラスの問題。§3.1）。ファイル名を変えたい場合はユーザーが overrides で明示的に指定する。

差分対象にするのは `opts.ContentPath` が非 nil かつ非空のときだけ。desired は `SanitizeContentPath(*opts.ContentPath)`、比較相手は observed の生値（POST する値と比較する値を同じにして 1 パスで収束させる）。この区別に列も伝播も要らないのは、`reservations.base` に contentPath を載せる書き手が存在しない（ruler の `computeBase` が意図的に除外している）ため、effective の非 nil = ユーザーの明示指定と同値になるから。**ruler が base に contentPath を載せた瞬間にこの同値が崩れ、テンプレート生成値が差分に混ざって churn が戻る**（これが今でも先に浮かぶ壊し方）。

テンプレート生成値は従来どおり固定。EPG の番組名で動く値を差分にすると EPG 更新のたびに DELETE+POST になる（上記の理由のとおり）。

override の削除（reset）は既存 schedule に反映しない。戻り先がテンプレート生成値（安定でない値）なので、反映すると churn が戻る。set は常に反映・reset は常に非反映で、priority を同時に触ったかどうかには左右されない。

再作成は `state == "scheduled"` の allowlist の下でだけ起きるので、録画開始後の変更は反映されない（未反映は `rokuban_reconcile_pending_diff{action="update_deferred"}`）。ファイルが 1 バイトも書かれていない schedule だけを張り替えるので、宛先変更で原本が取り残される経路は無い。

比較は mirakc が `options.contentPath` をそのまま返す（正規化しない）ことに依存する。この前提は `internal/mirakc/conformance` の `TestConformance/ContentPathRoundTrip` が mirakc 4.0.0-dev.0 相当に対して判定している。正規化して返す実装だと毎パス再作成になるので、`rokuban_reconcile_pending_diff{action="update"}` がゼロに戻らないことと `reason=content_path` の再作成ログの反復で観測する。

**再作成の POST は observed の `contentPath` を引き継ぐ**（テンプレートから再生成しない）。「差分と見なさない」だけでは priority 変更で再作成するときに何を入れるかが決まらない。再生成すると EPG の番組名が変わっていれば別のパスになり、**priority を変えただけでファイル名が変わる**という副作用になる。引き継げば「初回生成値に固定し以後変更しない」が文字どおり保たれる。引き継ぐ値は自分が書いたものの往復だが、mirakc 側を直接触られていた場合の保険として `SanitizeContentPath` は通す。明示 override があるときだけそれが observed への引き継ぎに勝ち、その場合もテンプレート再生成には落ちない（決定は `explicitContentPath` の 1 箇所に集約してある）。

#### 再作成のガード

**ガードは時刻の閾値ではなく状態で判定する。しかも blocklist ではなく allowlist にする — `state == "scheduled"` のときだけ再作成する。**

blocklist（「`tracking` / `recording` の予約は触らない」）にしない理由は 2 つ:

- `rescheduling`（延長追従中）も `finished` / `failed` も、削除して作り直してよい状態ではない
- mirakc が将来状態を増やしたとき、blocklist は**未知の状態を「触ってよい」側に落とす**。allowlist は安全側に倒れる

state の文字列は `internal/mirakc` に定数として置く。持ち越した件数はログに出す — 黙って落とすと「収束しない」の原因が見えなくなる。

#### 1 パスの再作成数に上限を設ける

**ルールの priority を編集すると、マッチしている全予約が再作成対象になる。** N=200 なら 1 パスで 400 回の mirakc 呼び出しになる（予約単位の編集だけを想定した見積もりはこれを数え落とす。末尾「経緯と失敗事例」）。

`MaxRecreatesPerPass`（既定 20）でレート制限し、レベルトリガーで数パスに分けて収束させる。持ち越した件数はログに出す（黙って切り捨てると「全部反映した」ように見える）。

これは `MaxDeletesPerPass`（大量削除サーキットブレーカー。[breaker.md](breaker.md)）とは**別の機構**である。ブレーカーは「導出された削除」を止めるもので超えたら**何も削除しない**、こちらは単なるレート制限で上限までは実行して残りを次パスに送る。**再作成の DELETE をブレーカーの数に混ぜない** — 混ぜるとルールの priority 一括変更でブレーカーが誤作動する（再作成は desired の消滅ではない）。

#### DELETE 成功 → POST 失敗

schedule が消えたまま次のパスまで残る。レベルトリガーで次パスが再作成するが、その間に開始時刻を越えると取りこぼす。**専用のカウンタメトリクス + `slog.Error`** で観測する。`quality_events` には書けない —— それは `recordings` テーブルの列で、まだ開始していない番組には recordings 行が存在しないことがある。allowlist のガードにより再作成の対象は `scheduled`（開始まで間がある）だけなので、取りこぼしの窓自体は元々小さい。

**upstream への要望**: `priority` の部分更新 API があれば再作成の窓ごと消える。`RecordingOptions` 全体を差し替える汎用 PUT より通りやすく、mirakc 側が触る内部状態（スケジューラのキュー）も小さい。priority は開始前の schedule に対して mirakc のスケジューラが素直に扱える性質のフィールドでもある（予約オプション一覧表のとおり、録画開始後は効かない可能性が高い・未測定）。#8 に調査メモとして残す。

---

#### 経緯と失敗事例

- **「tags は不変」の見積もり誤り**: 当初の差分対象の表は「tags は reservation id で不変」として差分対象から外していた。これは「同じ予約に対しては不変」であって「同じ番組に対して不変」ではなく、予約が削除されて同じ番組に別の予約が作られると古い tag が残り、録画が別の予約に紐付く（issue #19 のコメント）
- **終了済み番組への POST**: 終了済み判定を入れる前は failed 行の量産が実際に起きていた（issue #134。理由は本文のとおり）
- **再作成ガードは当初 blocklist**（「`tracking` / `recording` の予約は触らない」）だった。issue #19 のコメントで allowlist に変えた（理由は本文のとおり）
- **`MaxRecreatesPerPass` の見積もり誤り**: 当初の「再作成が走るのは『ユーザーが優先度を変えたとき』だけなので、この単純なガードで足りる」という見積もりは**予約単位の編集を想定していて、ルール単位の編集を数えていなかった**
- **「DELETE 成功 → POST 失敗を `quality_events` に記録する」案**: `quality_events` は `recordings` テーブルの列で、まだ開始していない番組には recordings 行が存在しないため書く先がなく、実装できなかった（issue #19 のコメント）
- reconciler のジョブ化（常駐シングルトン → `ReconcilePassWorker`）は issue #24 M2-17
