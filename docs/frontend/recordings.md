> [frontend.md](../frontend.md) の一部。索引から辿る

# 録画一覧・検索・再生

## 録画単体の着地先

録画は一覧内展開でしか見られず単体の URL を持たなかったため、skip 理由
（「重複（録画 #345）」）や予約 → 録画の導線がリンクの終点を持てなかった。
着地先は `/recordings/$id`。`recordings.id` は ingest（watcher）が一度作ったら
変わらない不可逆な事実の id なので、そのまま URL に使ってよい ---
`reservations.id` を URL に使わず `(site, programId)` を宛先にした予約詳細
（`pages/reservation-detail.tsx`。`reservations` は ruler の導出削除・
再実体化で id が変わりうる）とは事情が異なる。「一覧内スクロール + 展開」は
無限リストで対象が読み込み済みとは限らず成立しないため、別ルートにした。

本体（プレイヤー・メタデータ・削除系操作。下記「ブラウザ再生」節と
「ドロップ統計」節が対象とするもの）は単体ページ
（`pages/recording-detail.tsx`）の `RecordingDetail` が持つ。一覧はこの本体を
インライン展開せず、行本体から単体ページへ移動する。以下の各節（ブラウザ再生の
出し分け・ごみ箱の非表示規律）は単体ページに適用される。

**単体ページ自身のクエリキーは、一覧の invalidate に前方一致させてある**
（`pages/recording-detail.tsx` の `recordingDetailQueryKey`。先頭要素を
`getListRecordingsQueryKey` と同じ `'/api/recordings'` に揃える）。
削除 / 復元 / 完全削除 / 追加エンコードはすべて `RecordingDetail` の下から
`queryClient.invalidateQueries({ queryKey: ['/api/recordings'] })` を呼ぶだけ
（`RecordingActions.invalidate` / `AddEncodeProfilesAction` の `onSuccess`、
どちらも `pages/recordings.tsx`）で、単体ページも自動的に再検証される。
prop で 1 段ずつ配線する形（`onMutated` のような穴）は採らなかった ---
`RecordingDetail` の下に mutater を足すたびに「単体ページへの配線を通す」
ことを覚えていないといけない形は、通し忘れても型エラーにならず黒く抜ける
（最初の実装がこれで、`RecordingActions` にだけ配線したため
`AddEncodeProfilesAction` が素通しになり「事後エンコードを依頼しても単体
ページの『追加済み』表示が更新されない」を実機再現された）。

`GET /api/recordings/{id}` はごみ箱の録画も 200 で返す（メタデータの
可視性はメディア配信の 404 契約とは別の判断。[api/rest.md](../api/rest.md)
「録画単体」）。単体ページはこの `deletedAt` の有無で、下記のごみ箱の
出し分け規律をそのまま適用する。

## 録画のブラウザ再生

**VOD は MP4 progressive + Range。** streamer の `GET /api/recordings/{id}/file?profile=<name>` を
ネイティブ `<video controls>` の src に渡す。HLS / hls.js は使わない（家庭 LAN の
オンデマンドではセグメント化のコストに見合わない。決定は [api.md](../api.md)）。

- 利用可能なプロファイルは `Recording.encodedAssets`（active な encoded のみ。各要素は
  `profile` + `sizeBytes`）。複数ならセレクタ。encoded が無ければプレイヤーは出さず、
  原本があるときだけ VLC 向けリンクを出す
- **再生位置は localStorage**（キー: 録画 ID + プロファイル）。サーバー側視聴履歴は作らない
- 原本 TS はブラウザでは再生せず、ダウンロード / VLC リンクとして残す
- **ごみ箱ビューではサムネイル・プレイヤー・原本リンクを一切出さない。**
  配信側（`GetOriginalMediaAssetForServing` 等）は `recordings.deleted_at IS NOT NULL`
  を 404 にする契約（[api.md](../api.md) §メディア配信）なので、出しても必ず 404 になる。
  復元してから見る運用にする。ごみ箱一覧が `encodedAssets` を射影しないままなのも
  同じ理由（プレイヤーを出さないので揃える必要がない）

### 操作点にサイズを常置する（値札）

**資源を消費する操作には実行前に値札。値札は事実（実測サイズ）のみ**（転送時間の
見積などは書かない）。プロファイルセレクタの各選択肢（`<option>`）・ダウンロード
リンク・VLC リンクのすべてに `formatBytes`（`lib/format.ts`）のサイズを常置する
（`components/recording-player.tsx` の `assetOptionLabel`）。プロファイルが 1 つ
（= セレクタ自体を出さない）でも、押す前にサイズを見せるという方針は変わらないため
キャプションとして常に出す。

**サイズが取れない資産は隠すのではなくサイズだけ省く。** `EncodedAsset.sizeBytes`
（openapi.yaml）は省略可能な形にしてある --- 選択肢（プロファイル名）自体は
分類の失敗（ここではサイズの欠落）を理由に隠さない。ドロップ統計の「分類できな
かった PID は種別を空にして PID 数値だけ出す」（下記「ドロップ統計」節）と同じ
判断。`media_assets.size_bytes` は `NOT NULL` 列制約なので active な encoded 行が
ある限り実際には常にサイズが付く（`media_assets.size_bytes` は列制約が
`NOT NULL`。同テーブルの CHECK 制約は `kind` / `profile` / `state` にしか
掛かっておらず `size_bytes` には無い）が、型としては
省略可能にしてこの表示規律をテストで固定している（`recording-player.test.tsx`）。

### 一覧の行は詳細へのリンクにする

録画一覧はインライン展開も常時の「再生」列も持たず、行本体を
`/recordings/$id` へのリンクにする。再生可能な行だけ右端に塗りのボタンを置くと
行の形が揃わず、単体ページができた後も一覧が同じ `RecordingDetail` を二重に
抱えることになる。視聴・削除・エンコードはすべて上記「録画単体の着地先」に寄せる。

ごみ箱の行と `encodedAssets` が空の行も詳細へリンクする。前者は詳細側が
`deletedAt` を見てプレイヤーを出さず、後者は原本のダウンロード / VLC リンクを
詳細側が引き受ける。再生可否は一覧から詳細へ行けるかどうかの条件にしない。

一覧から `.play()` 相当を呼ばない判断は、詳細へ寄せても変わらない。詳細は
`RecordingPlayer` のネイティブ `<video controls>` を表示するだけで、利用者が controls
の再生ボタンを明示的に押すまで `.play()` を呼ばない。コストのかかる本編転送を
一覧の行選択だけで始めないという値札の方針を保つ。

**`<video>` に `tabIndex` は明示しない。** 実 Chromium で測った結果、
`<video controls>` は tabindex 無しでもそれ自体が Tab stop になり、`tabIndex={-1}` を
付けると Tab 順から完全に外れる。`web/e2e/design.mjs` は録画一覧の行リンクを Enter で
開き、詳細ページで `<video>` に Tab 到達できることを実ブラウザで機械判定する。

## 取り込み中であることを画面に出す

`status = finished` は mirakc の録画完了であって取り込み完了ではない。原本が
コミットされるまで再生も事後エンコードもできないのに、それを表すものが
`sizeBytes` の省略しか無かった。`Recording.ingest`（サーバーが DB 行から毎回導出する。
[recording/ingest.md](../recording/ingest.md) §5.6）を 2 段に分けて出す。

- **行のバッジ**（`IngestBadge`。一覧の行と単体ページのヘッダー）: 「取り込み待ち」/
  「取り込み中 42%」/ 分母が無ければ「取り込み中 1.2 GB」。進捗が 60 秒以上更新されて
  いなければ「（停滞）」を添える
- **展開部の「取り込み」欄**（`RecordingDetail` の `<dl>`）: 分母が取れていれば
  「転送中 1.2 GB / 3.4 GB（35%）」まで出す

**表示しないケースを決めておく**（言うことが無いときに行を並べない）:

- 正常に取り込めて原本もある録画（`committed` かつ `sizeBytes` あり）: 何も出さない
- `status = 'recording'`: 取り込みがまだ始まっていないのは正常であって「待っている」を
  知らせる情報ではない。録画中の全行に「取り込み待ち」が並ぶのは何も言っていないのと同じ
- `ingest` そのものが無い（API が古い）: 推測で埋めない
- **ingest ジョブが来ない録画（`failed` / `canceled`）**: `state` が `unknown` で
  返るので何も出ない。ここで「取り込み待ち」を出すと**来ない未来を UI が断定する**
  ことになり、`sizeBytes` について直したはずの誤りを `pending` について作り直す
  --- `pending` の根拠を `record_sync` 行の**存在**に取るとこれが起きる（watcher が
  ingest を投入するのは record が `finished` のときだけで、`record_sync` 行は消え
  ないため、`failed` / `canceled` の録画が永久に「取り込み待ち」を名乗る）。根拠は
  **watcher が ingest を投入する条件と同じ述語**（`record_sync.status = 'finished'`）
  に揃える。**状態の名前が「これから起きる」を含むなら、起きる根拠を述語として
  書けるか確かめる**

**色は使わない**（`bg-muted` のまま）。停滞も含めて状況の説明であって、タリー
（いま電波に乗っている）でも destructive（取り返しがつかない）でもない
（[design.md](design.md)「色は信号のみ」）。停滞は文言で言う。

**「原本は削除済み」と言えるのは `committed` かつ `sizeBytes` 無しのときだけ。**
`sizeBytes` の省略は「取り込んだ後に消した」と「まだ取り込めていない」の両方を
含むので、これだけを見ると権限不足で ingest がリトライ中の録画に「削除済み」と
読める表示が出る。
サーバーが「`kind='original'` の行が `state` を問わず存在するか」を見て `committed` を
返すので、UI 側はその区別をそのまま使える。

**進捗は REST の再取得で収束させる**（SSE はヒント。不変条件 5）。ただし
**5 秒間隔を張るのは「進捗の数字がいま動いている」間だけ**にする
（`lib/ingest.ts` の `hasLiveIngestProgress`）。一覧は無限リストなので、
常時ポーリングすると積んだページ全部を取り直すことになる。

短い周期を張らないもの（`state` としては未完了でも）と、その代わりに何が
収束させるか:

- **`pending`**: いつ始まるかはこちらから分からない。**失敗し続けている ingest も
  ここに落ちる** --- 権限不足で `MkdirAll` に失敗する場合、進捗行が書かれる前に
  落ちるので、River が再試行し続ける間ずっと `pending` のまま（実環境で起きた
  壊れ方）
- **停滞した `transferring`**: River のバックオフ待ちや、discard 後に
  record_sweep（5 分周期）が再投入するのを待っている状態。分オーダーでしか
  動かないものを 5 秒で叩かない。**再開すれば `observedAt` が新しくなって自動で
  短い周期に戻る**（自己回復する）
- **`status = 'recording'`**: 録画中に取り込みの数字は動かない

いずれも [shell.md](shell.md) の 60 秒 invalidate（`lib/events.ts` の
`operationalRefreshIntervalMs`）が拾うので、放置ではなく「周期を落とす」だけになる。
**「終わっている画面はポーリングしない」を保つには、未完了の状態すべてを短い周期の
対象にしてはならない** --- `record_sync` 行が消えない以上「未完了」は恒久的に残り
うるので、失敗録画が 1 件でも読み込み済みページにあればポーリングが恒久化する。

## エンコードの待ち・実行中・失敗を画面に出す

`Recording.encodeProfiles`（desired）と `Recording.encodedAssets`（observed、
再生可能なもの）の差だけでは「まだ来ていない」としか言えず、いま走っているのか
失敗して再試行待ちなのかを区別できなかった。`Recording.encodeStatus`
（openapi.yaml。desired のうち observed にまだ現れていないプロファイルだけを
`queued` / `running` / `failed` のいずれかで列挙する）をバッジで出す
（`EncodeStatusBadges`。一覧の行と単体ページのヘッダー、`IngestBadge` の隣）。

- **`running` は進捗イベントを受け取るまで `%` を出さない。** worker が実入力を
  ffprobe して得た duration を分母にし、ffmpeg の `-progress pipe:1` の `out_time`
  から割合を作る。duration を取得できない場合や SSE を取りこぼした場合は
  架空の割合で埋めず「エンコード中」のままにする
- **プロファイル未設定・全プロファイル完了済みの録画では `encodeStatus` が
  省略される**ので、`EncodeStatusBadges` は何も描かない --- 機能しないキュー
  画面や空の進捗バーを出さない判断はサーバー側のこの省略で表現されており、
  フロントはそれをそのまま描くだけでよい
- **`failed` だけ destructive**（`bg-destructive/10` + `text-destructive`。
  `DropBadges` と同じ判断: 実害があるので色で目立たせる）。`queued` /
  `running` は `IngestBadge` と同じ `bg-muted`（状況の説明であって信号ではない。
  [design.md](design.md)「色は信号のみ」）
- **バッジにはプロファイル名を前置する**（`h264: エンコード失敗`）。事後追加で
  複数プロファイルを依頼した録画では「どのプロファイルが失敗したか」が言えないと
  運用判断に使えない（ドロップ統計の種別列と同じ判断）
- **`queued` は「来る根拠」があるものしか出ない**（サーバー側の導出。ごみ箱の
  録画とプロファイル自体が丸ごと省略されるので、フロントは何もフィルタしない）。
  `failed` は「二度と来ない」の断定でもない --- River の既定リトライと、録画単位の
  恒久失敗（入力ファイルの破損など）に対する `EncodeReconcileWorker` の 15 分ごとの
  再投入のたびに `running` へ戻る（`queued` へは戻らない --- 試行行は成功するまで
  消えないので `queued` は初回試行の前だけ）。判断の詳細は
  [schema/recordings.md](../schema/recordings.md) の `recording_encode_attempts`
  節
- ホームに出す場合は失敗録画と同じ「異常」の置き場に合わせる想定だが、本機能
  自体はホームへの統合を含まない --- 一覧・単体ページで沈黙と区別できれば足りるため
- **完了・失敗の真実は引き続き REST の `encodeStatus` / `encodedAssets`。**
  `encode-progress` SSE は接続中だけ届く揮発テレメトリで、`running` の文言に割合を
  重ねるためだけに使う。durable 状態が `running` でなくなったら最後の値を破棄し、
  `progress=end` や 100% だけで完了を断定しない
- **進捗はテーブルへ保存しない。** 再起動後に不要な秒単位の値で WAL・dead tuple・
  vacuum 対象を増やさず、worker → PostgreSQL NOTIFY → notifier の EventHub → SSE
  という best-effort 経路で最大 1 回/秒だけ運ぶ。画面を途中から開いた場合は REST
  由来の `running` を先に出し、次のイベントから割合を表示する
- `queued` / `running` / `failed` の収束は既存の
  `operationalRefreshIntervalMs`（`/api/recordings` を再取得する 60 秒周期。
  `web/src/lib/events.ts`）にも乗る。背面タブでは定期取得を投げず、前面へ戻ったときは
  `refetchOnWindowFocus` が拾う。進捗イベントを取りこぼしても durable 状態の完了・
  失敗判定はこの再取得で正しく収束する

## ドロップ統計はバッジ + 展開

録画一覧に drop / error / scrambled を色付きバッジで出す。**0 のものは出さない**ので
正常な録画ではバッジが 1 つも出ない。PID 別内訳は行を展開して見せ、
モバイルで表を横スクロールさせない。

展開表には**種別**列を置く（映像 / 音声 / PAT 等）。PID 数値だけの統計では
「映像が壊れたのか EIT が数回落ちただけか」を区別できず運用判断に使えないため
（[recording.md](../recording.md) §1「例外の境界」）。分類できなかった PID は
種別を空にして PID 数値だけ出す — 分類の失敗で統計そのものを隠さない。

## 録画検索は `/recordings` に同居する

EPGStation にある「録画済みの検索」に対応する機能だが、`/search`（EPG 検索。
[search.md](search.md)）とは**別ルートにしない**。録画検索は録画一覧そのものの
絞り込みであり、`/search` を
独立ルートにした理由（「番組表は EPG を時間軸で眺める画面、検索は ruler と同じ条件
コンパイラを叩く別の問いの画面」）が録画には当てはまらない --- 録画一覧に検索専用の
別画面を作ると、絞り込み結果と一覧が別画面になり「絞り込んだ録画をそのまま操作
（削除・復元・エンコード追加）する」という主用途が 2 画面に分裂する。

**`/search` と条件モデルを共有しない。** `ProgramSearchRequest`（`internal/rulequery`
を通る EPG 検索・ルール条件）と `GET /api/recordings` の絞り込みは別のクエリで、
フィールドの意味も重ならない（録画検索の `status` / `source` は録画の観測・出自、
`/search` にはそもそも無い次元）。あちらの条件をこちらに持ってくる導線（相互流用）も
作らない --- 両者は別の問いに答えている。

### 条件は URL に持つ。`lib/recording-search.ts` に純関数として集約する

`RecordingsPageSearch`（型）・`parseRecordingsSearch`（`validateSearch`）・
`buildListRecordingsParams`（→ API クエリ）・`describeRecordingsFilters`
（→ 適用中の条件チップ）・`clearRecordingsFilters` はすべてここに置く。
`lib/program-search.ts`（`/search` の下書き）とは意図的に分離している ---
条件モデルを共有しないので、変換ロジックを共有する理由も無い。

- **チャンネルは `Service.id` で、site は `?site=` で URL と API に載せる。**
  `serviceId` は network をまたいで一意でないが、合成 id なら BS 101 と CS 101 を
  独立に選択・解除できる。**site を identity に混ぜない** --- 混ぜると
  「高松の録画を全部」がチャンネルの列挙でしか表せず、`service` だけが
  他の軸と違う意味論（3 つ組の選言）になる。軸内は OR、軸間は AND
- **チャンネル種別（`channelType`）・`qTarget` は UI に出さない。** チャンネルは
  個々のサービスを選べる `<ChannelPicker>`（`Service.id`）の方が細かく絞れ、
  種別だけの選択肢を並列に置く理由が無い。`qTarget` も UI 案に
  無い次元で、出しても検証できないコントロールを増やすだけ（「機能しない
  コントロールは置かない」の逆）。パラメータ自体は `ListRecordingsParams` に
  残るので、共有 URL に手で `qTarget=title` を足す使い方は塞がない
- **キーワードはチップにしない。** 検索欄自体が値を表示しているので、消すのは
  入力欄の編集で足りる。配列条件（ジャンル・チャンネル）は値ごとに 1 チップ
  （個別に外せる）、スカラー条件（状態・種別・期間・ルール）は次元ごとに
  1 チップにする

### ごみ箱タブと検索条件は直交させる

タブ（ライブラリ / ごみ箱）は条件と直交する別の軸なので URL に載せない。
`pages/recordings.tsx` の component state のまま持ち、`buildListRecordingsParams`
も `trash` を検索条件（`RecordingsPageSearch`）とは別の引数で受ける
（`lib/recording-search.ts`）。タブを切り替えても `RecordingsPageSearch` は
そのまま渡るので、条件は自動的に保持される。

### debounce と URL 同期で履歴を汚さない

キーワード入力は 300ms の debounce を挟んでから条件（URL）に確定する
（`components/recording-filters.tsx` の `KeywordField`。`RecordingFilters` 自体は
状態を持たず、debounce 用の下書きだけを例外とする）。条件の URL への書き込みは、
debounce（キーワード）もチップの個別解除も常に `replace` で navigate する
（`pages/recordings.tsx`）--- 1 文字ごと・操作ごとに URL を書き換えて履歴を
汚さないため。

### TanStack Router の `validateSearch` は無効な値を「省略」しても消えない

`parseRecordingsSearch` は非 strict モード（既定）の TanStack Router で使われる。
このモードは実際のルートマッチでも（`matchRoutesInternal`。`@tanstack/router-core`
の `router.js` 内、`preMatchSearch = { ...parentSearch, ...strictSearch }` ---
`parentSearch` は生の未検証の値）、ビルドロケーション用の軽量マッチでも
（`matchRoutesLightweight` の `accumulatedSearch`。`Object.assign(accumulatedSearch,
validateSearch(...))`）、`validateSearch` の戻り値を「生の（未検証の）
`location.search` の上に重ねる」形で合成する。**戻り値からキーを省略すると、その
キーは上書きされない**ので、生の不正な値（`?status=bogus` の文字列そのもの等）が
「検証済みのつもり」の結果へそのまま残って漏れる --- 実機で確認済み（壊れた URL の
不正な値がチップにそのまま出た）。対策は**落とした次元も `undefined` を明示的に
代入する**（キーを省略しない）。`{ ...x, k: undefined }` はどちらの合成方式で見ても
実際に上書きになるため、これで確実に消える。

`/live` の `service` と `/search` の `ruleId` はどちらもこの形に揃えてある
（`routes.tsx`。`routes.test.tsx` が `router.state.matches` の `search` を直接見て
固定している）。

### 一覧は自前で組んだ `useInfiniteQuery`

orval は無限クエリのフックを生成しない（生成されるのは単発の `useQuery` ラッパーの
み）ので、生成された `listRecordings` 関数を `@tanstack/react-query` の
`useInfiniteQuery` に自分で渡す。`queryKey` は `getListRecordingsQueryKey(絞り込み +
limit)`（カーソル `before` / `beforeId` を含めない）にする --- 同じ絞り込みの中で
ページを積んでいくのが `useInfiniteQuery` の前提であり、カーソルを含めるとページ
ごとに別クエリになってしまう。先頭要素が `'/api/recordings'` になる形は保たれるので、
`RecordingActions` の `invalidateQueries({ queryKey: ['/api/recordings'] })`（前方
一致）が変わらず効く。

進行方向の読み込み（番兵 + IntersectionObserver、失敗後はボタンへ落とす
`lib/auto-load.ts` の `shouldAutoLoadNextPage` / `shouldShowLoadMoreButton`、
計測できない環境の判定 `lib/list-virtualization.ts` の `domLayoutMeasurable`）は
`pages/programs.tsx` と同じ部品を再利用する。録画一覧はグリッドのような座標系を
持たないリストなので仮想化はしていない。固定の `limit` は渡さず、既定ページサイズ
（50）で継ぎ足す。

### 0 件の文言は条件の有無で分ける

録画一覧に「未検索」の状態は無い（条件ゼロ = 全件が正しい。`/search` の
「未検索と 0 件を別状態にする」規律はここには持ち込まない）。ただし 0 件の文言は
分ける --- 条件があるとき「条件に一致する録画がありません」+ 条件をクリアする
導線、条件が無いときだけ「録画がありません」（ごみ箱タブでは「ごみ箱は空です」）。
取り違えると「まだ何も録れていない」と誤読させる。

`components/page.tsx` の `EmptyState` は `<p>` ではなく `<div>` で包む。
条件クリアのボタンを子に持つ呼び出し側があるため --- `<p>` の中に
ブロック要素（`<div>` / 別の `<p>`）を置くと無効な HTML になり、React が hydration
エラーの警告を出す。

## 録画 → ルールの導線

`RecordingDetail` は `recording.ruleId` があるときだけ「ルール」セクションを出す
（`ruleId` が無い手動予約由来の録画では**セクションごと出さない** ---
「機能しないコントロールは置かない」の既存規律）。

- **ルール名の解決は `useListRules` のキャッシュから引く。** ルール専用の単体
  取得エンドポイント（`GET /api/rules/{id}` / `useGetRule`）はあるが使わない
  --- `RulesPage` が `useListRules()`（パラメータなし = 常に全件）を引く設計に
  既に乗っているので、録画ごとに個別の 1 件取得を増やす理由がない（`/rules` を
  経由していればキャッシュに乗っており、していなければ詳細を開いたときに引く。
  後者は下記の `#N` → ルール名の差し替えとして見える）
- **`rules.find` で見つからない ruleId は `#N` 表記に落とす。ただしこれは
  「ルールが削除された」ケースではない。** `recordings.rule_id` は `rules`
  への FK が `ON DELETE SET NULL` なので、ルールを削除
  すると `recordings.rule_id` が NULL になり `Recording.ruleId` 自体が省略
  される --- つまりルール削除後は「ルール」セクションごと消え、`#N` へは
  落ちない。`#N` に落ちるのは `rules.find` が空を返す間、つまり一覧クエリが
  未解決 / 失敗（どちらも `query.data` が `undefined`）か、返ってきた一覧に
  その id がまだ無い（新しく作られたルール等）という一時的な状態
  （`recording-detail.test.tsx`「ルール一覧にまだ載っていない ruleId でも
  #N 表記に落ちて壊れない」で固定した）
- **原則「固有名詞はリンク」に従い**、ルールの識別（名前 or `#N`）そのものを
  リンクテキストにする。リンク先は `/search?ruleId=N`（`RuleRow` の
  「検索しながら編集」と同じ着地先、ルールの実質的な編集画面）。もう 1 本の
  リンク「このルールの録画で絞る」は `/recordings?ruleId=N` --- 同一ページ
  （`/recordings`）内の検索条件変更であり、既存の `parseRecordingsSearch`
  （`lib/recording-search.ts`）を通るので条件チップにもそのまま出る

## ストレージ残高と満杯見込み

`/recordings` のヘッダー領域（`RecordingFilters` の下）に「空き X GB / 今後 7 日の
予約で約 +Y GB の見込み」を出す（`components/storage-balance.tsx`）。導出は
`lib/storage-forecast.ts` に純関数として集約し、`StorageBalance` 自身は 3 つの API
（`GET /api/storage` / `GET /api/recordings` / `GET /api/reservations`）の取得と
表示の出し分けだけを持つ --- 「録画一覧・録画検索」と機能的な関係は薄いが、`/recordings`
がストレージを最も直接に消費する画面であるため最初の設置先にした。M8（ホーム）で
移設する前提で `components/` に独立させてある。

**参照する root は `media`（`storage.media_dir`。アーカイブ）だけ。** `scratch`
（ローカルスクラッチ）は録画の最終的な保存先ではないので残高の対象にしない
（[storage/contract.md](../storage/contract.md) §残量の観測、同ファイル §5
「2 階層: 録画バッファとアーカイブ」）。

### 4 つの沈黙

- **ストレージ観測が無い**（`GET /api/storage` の結果に `root === 'media'` の行が
  無い。初回観測前や statfs 失敗の継続で起きうる）ときは部品ごと何も描かない
- **直近の録画実績が 0 件、または予約の取得が未解決/失敗**のときは残高だけ出し、
  「+Y GB の見込み」「満杯見込み」は出さない --- でっち上げの既定値を置かない
- **予約が正当に 0 件**（取得は成功したが今後 7 日の窓に予約が無い）のときも
  「+0 B」は出さない --- 前項と表示結果は同じだが、内部的には別の経路
  （後述）を通る
- **見込み消費が残量に収まる**ときは満杯見込み日を出さない（下界主義。
  `lib/capacity.ts` の「主張は下界に限る」と同じ精神）

**「取得失敗」と「正当な 0 件」を混同しない。** 予約の取得が失敗/未解決のとき
（`lib/storage-forecast.ts` の `upcomingReservationSchedule` の結果が
`undefined`）は `estimateStorageForecast` が `hasEstimate: false` を返し、見込み
そのものを算出しない。予約が正当に 0 件のとき（結果が空配列 `[]`）は見込みを
算出した上で `projectedConsumptionBytes: 0` になる。**この 2 つを区別せず
`undefined` を `[]` にフォールバックすると、取得失敗時に「今後 7 日の予約で
約 +0 B の見込み」という、欠損データから捏造した肯定を描いてしまう**
（実際にこの不具合を実装直後のレビューで指摘された。プローブで実際の描画
「空き 931.3 GB今後7日の予約で約 +0 B の見込み観測: 8/13 15:57」を確認済み）。
録画側（`recordings` が `undefined`）も同じ理由で `averageBitrate` を
`undefined` のまま渡し、`0` にフォールバックしない。

ヘッダーの補助情報 1 つのために専用のエラー表示（`ErrorState`）を割かず、
取得失敗はすべて上記の沈黙（残高だけ出す）に縮退させる。

### 母数: 直近 20 件の finished 録画

`GET /api/recordings?status=finished&limit=20`（`lib/storage-forecast.ts` の
`recentRecordingSampleLimit`）。日数ではなく件数にしたのは、録画頻度が運用ごとに
大きく違うため（固定の日数では少ない側で標本が枯渇し、多い側で 1 回のフェッチの
上限 200 件に収まらなくなる）。`status=finished` に絞るのは、`Recording.durationMs`
が番組の放送時間（全尺）であって実際に録画できた時間ではないため --- `failed`
（途中で終わった録画）を含めると「途中までの `sizeBytes`」を「全尺の `durationMs`」
で割ることになり、ビットレートが実際より低く出る方向に偏る。`sizeBytes` が無い
録画（原本削除済み。`Recording.sizeBytes` の説明「原本の実サイズ。ingest 済みの
場合のみ」参照 --- 未 ingest も含む）も除く。

**`sizeBytes` は原本 TS のみでエンコード派生物を含まない。** エンコードプロファイル
を設定しているルール（既定 `keepOriginal: always`。[storage/retention.md](../storage/retention.md)）
では実消費は原本 + 派生物なので、この見込みは**過小**に振れる。逆に
`keepOriginal: until_encoded` を選んでいるルールでは、エンコード完了後に原本が
削除され実消費が派生物サイズ（原本の 1/4〜1/10、同 doc）へ縮むため、原本サイズを
今後も一定と仮定するこの見込みは**過大**に振れる。どちらの方向にも振れることを
前提にしており、`lib/rule-cost.ts` の「見込み」と同じく一方向の保証は書かない。

### 鮮度: 1 時間を超えたら「古い可能性」

`GET /api/storage` の `observedAt` は「観測ループが止まっていても行は消えない」
契約（[storage/contract.md](../storage/contract.md) §残量の観測）なので、鮮度は
UI 側で必ず判定する。worker の観測間隔は現在 **5 分固定**
（`internal/worker/storage.go` の `defaultStorageSyncInterval`）。
`worker.Config.StorageSyncInterval` というフィールド自体はあるが、
`cmd/rokuban/server.go` がどの設定キーからも埋めていないため実質デッドで、
「設定で変更可能」という記述は誤り（以前の版はここで存在しない設定キーを根拠に
していた）。

それでも間隔の値をフロントへ輸入して「5 分の N 倍」を定義しないのは、この値が
worker 側の実装詳細であって `GET /api/storage` の契約に含まれないため。1 時間
（`lib/storage-forecast.ts` の `observationStaleAfterMs`）は代わりに置いた
**独立した固定の安全マージン**で、現在の 5 分間隔に対して 12 倍の余裕があり、
1 回の失敗パスや再起動直後の遅延では誤って「古い」と出ない一方、観測ループが
本当に止まっていれば 1 時間以内に検知できる。将来この間隔が実際に設定可能になり
既定より大きく延ばす運用が出てきた場合は基準が弱くなるため、そのときに見直す。

この行自体は 5 分周期の定期 invalidate で取り直している（[docs/api/sse.md](../api/sse.md)
の「ストレージ」グループ）。取り直す周期（5 分）と古さの判定基準（1 時間）は
独立に選んだ別の値で、前者は「得るものが無い頻度で REST を引かない」ため、
後者は「1 回の失敗パスで誤検知しない」ための安全マージン。

### 消費見込み: 今後 7 日、skip=true は数えない

対象窓は今後 7 日（`forecastWindowDays`）。`lib/rule-cost.ts` の値札・ルールの時間帯
条件と同じ 1 週間周期に揃えている。`GET /api/reservations` は絞り込みパラメータを
持たないため全件を取得し、クライアント側で `[now, now+7日)` に開始する予約だけを
合算する。

**`skip === true` の予約は消費に数えない。** `skip` は `effective.skip`
（[recording/reservation-model.md](../recording/reservation-model.md) §4.3「同期の
可否を決めるのは state ではなく effective.skip である」）で、true の間 reconciler
は mirakc に同期しない --- つまりディスクを消費しない。`state`
（active/detached/orphaned）は表示用の導出値であって同期可否のフィルタに使っては
ならない（同節）ため、フィルタには使わない。

**満杯見込み日は一様分布を仮定しない。** `GET /api/reservations` で各予約の
開始時刻・尺は既に取得済みなので、`upcomingReservationSchedule` が `startMs` 昇順に
整列した消費イベント列を作り、`estimateStorageForecast`（`projectedFullAtMs`）が
先頭から累積消費を積み上げて残量を最初に超える瞬間を報告する。

当初の実装は「見込み消費を 7 日間に均等に分布する」線形外挿だったが、レビューで
2 方向の実害が指摘された:

- 予約が窓の**終盤**に集中している場合、一様分布は実際より**早い**満杯見込み日を
  出す（過大警告。例: 残量 5GB・ビットレート 24Mbps 相当・7 日後に始まる 6 時間の
  予約 1 件だけ ⇒ 一様分布は「明日頃」と出すが、実際は 7 日間なにも消費されず、
  満杯になるとしても 7 日後の予約の中）
- 予約が窓の**冒頭**に集中している場合、一様分布は実際より**遅い**満杯見込み日を
  出す（過小警告 --- 下界主義に反する危険な方向。例: 残量 50GB・同条件で**今日**
  始まる 6 時間の予約 1 件 ⇒ 一様分布は「5.4 日後」と出すが、実際はその予約の
  最中（今夜 ~4.6 時間後）に満杯になる）

実際の開始時刻を辿る現在の実装ではどちらも起きない（`lib/storage-forecast.test.ts`
の「予約が窓の終盤/冒頭 1 件に集中しているとき」の 2 テストがこの 2 方向を固定
している）。

**それでも近似が残る --- この列挙は閉じていない。** 現時点で分かっているのは
少なくとも次の 3 つ（1・2 は上の各節で述べた母数・エンコード派生物の近似の
再掲、3 が今回追加で見つかったもの）:

1. 平均ビットレートが直近の標本の外挿であること（上記「母数」節）
2. `sizeBytes` が原本 TS のみでエンコード派生物を含まないこと（同節）
3. **重なる予約を直列（開始順に 1 本ずつ消費）として扱うこと。** 実際に並行して
   録画される予約（複数チューナー構成では普通）があっても合成レートにはしない。
   この近似も両方向に振れる:
   - 同時に始まる複数予約では、直列近似は実際より**遅い**満杯見込みを出す
     （過小警告・危険な方向。実測: 同時開始の 6 時間予約 2 本が残量を消費する
     とき、直列近似は 2.78 時間後と報告するが、実際の合成レートでは 1.39 時間後
     に満杯になる）
   - 長い予約の途中に短い予約が重なるだけなら、直列近似は実際より**早い**満杯
     見込みを出す（過大警告・安全側だが不正確。実測: 24 時間予約に 30 分予約が
     重なるケースで、直列近似は 10 分後と報告するが、実際は約 23.67 時間後）

   誤差の大きさは重なる予約のうち長い方の尺で頭打ちになる（実測で数時間〜
   約 1 日程度）。上記の一様分布の系統誤差（最大で窓の長さそのもの、7 日）
   より小さいが、ゼロではない。`lib/storage-forecast.test.ts` の「既知の近似」
   2 テストが両方向を固定している。**実装は変えていない** --- 重なりを合成
   レートで扱うには、予約どうしの重なり区間を都度計算する必要があり、この
   タスクのスコープを超えるための判断（値札の精度を上げるコストが、今の
   下界主義的な運用判断への寄与に見合うほど高くない）。

この 3 つ（と、まだ見つかっていない近似があるかもしれないこと）のために
「満杯見込み日」はあくまで目安であって確約ではない --- `lib/rule-cost.ts` の
「見込み」と同じく、一方向の保証（「多めには出ない」等）は書かない。

