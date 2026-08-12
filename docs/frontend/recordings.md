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
「ドロップ統計」節が対象とするもの）は一覧の行展開（`RecordingDetail`）と
単体ページ（`pages/recording-detail.tsx`）が同じ部品を共有する ---
実装を 2 系統に分けず「一覧の展開と同等に機能する」を保つため。したがって
以下の各節（ブラウザ再生の出し分け・ごみ箱の非表示規律）は両方の画面に
同時に適用される。

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
（最初の実装がこれで `AddEncodeProfilesAction` の再検証漏れを踏んだ。
下記「経緯と失敗事例」）。

`GET /api/recordings/{id}` はごみ箱の録画も 200 で返す（メタデータの
可視性はメディア配信の 404 契約とは別の判断。[api/rest.md](../api/rest.md)
「録画単体」）。単体ページはこの `deletedAt` の有無で、下記のごみ箱の
出し分け規律をそのまま適用する。

## 録画のブラウザ再生

**VOD は MP4 progressive + Range。** streamer の `GET /api/recordings/{id}/file?profile=<name>` を
ネイティブ `<video controls>` の src に渡す。HLS / hls.js は使わない（家庭 LAN の
オンデマンドではセグメント化のコストに見合わない。決定は [api.md](../api.md)）。

- 利用可能なプロファイルは `Recording.encodedProfiles`（active な encoded のみ）。複数なら
  セレクタ。encoded が無ければプレイヤーは出さず、原本があるときだけ VLC 向けリンクを出す
- **再生位置は localStorage**（キー: 録画 ID + プロファイル）。サーバー側視聴履歴は作らない
  （下記「経緯と失敗事例」）
- 原本 TS はブラウザでは再生せず、ダウンロード / VLC リンクとして残す
- **ごみ箱ビューではサムネイル・プレイヤー・原本リンクを一切出さない。**
  配信側（`GetOriginalMediaAssetForServing` 等）は `recordings.deleted_at IS NOT NULL`
  を 404 にする契約（[api.md](../api.md) §メディア配信）なので、出しても必ず 404 になる。
  復元してから見る運用にする。ごみ箱一覧が `encodedProfiles` を射影しないままなのも
  同じ理由（プレイヤーを出さないので揃える必要がない）

### 再生ボタンは行右端に独立させる

録画一覧の最頻操作は再生だが、以前は行タップ（詳細展開）の 1 段下に埋もれていた。
[reservations.md](reservations.md) の予約ボタンと同じ配置文法 --- 行右端の固定幅
（最小 44px）、行本体（詳細展開）のタップ領域とは分離 --- を再生にも使う。

**ボタンの役目は「展開してプレイヤーへスクロール + フォーカス」までで、`.play()`
は呼ばない。** `<video preload="metadata">` は仕様上メタデータだけを先読みする指定
だが、実際にブラウザがどこまで先読みするか・`.play()` 相当の処理でどれだけ転送量が
増えるかはこの環境で計測していない（一般に本編データの取得はメタデータ取得より
重いはず、という未検証の前提に立っている）。測っていなくても採れる立場は
「`.play()` を呼ばない」という決定自体で、これは実装済みでテストが固定している
（`recording-player.tsx` の `focusToken` 系 --- スクロール/フォーカスはするが
`.play()` は呼ばないことをアサーションで見る）。行のボタンをワンタップしただけで
本編の転送が始まる形は避け、ネイティブ `<video controls>` の再生ボタンをもう一段
挟むことで、実際のデータ転送は利用者の最後の明示的なクリックに紐付ける ---
コストのかかる操作を暗黙に始めないという値札の方針（M7）に合わせた判断。予約の
ワンタップ + トーストとは非対称だが、予約は DB 行を作るだけの操作、再生は帯域を
伴う操作という違いを意図的に残している。

出し分けは 2 条件（両方向でテストする）:

- **ごみ箱では出さない。** 配信側が `deleted_at IS NOT NULL` を 404 にする契約なので、
  出しても必ず失敗する
- **`encodedProfiles` が空なら出さない。** `RecordingPlayer` が実際に `<video>` を
  描くかどうかの条件と一致させる（原本だけがある録画は VLC リンクしか出さないので、
  「再生」ボタンの対象ではない）

**`<video>` に `tabIndex` は明示しない。** 実 Chromium で測った結果、
`<video controls>` は tabindex 無しでもそれ自体が唯一の Tab stop になっており
（native controls の個々のボタンは Tab stop ではない）、`tabIndex={-1}` を付けると
逆に Tab 順から完全に外れてキーボード到達性を落とす退行になった。`.focus()`
自体はこの属性が無くても実 Chromium では効く（同じく実測。詳細は
`recording-player.tsx` のコメント）。この計測は手動（実 Chromium での Tab 到達確認）
で、`web/e2e/` に機械判定はまだ無い --- 44px タップ標的の判定が無いのと同じ既存の
欠落で、この PR 単独の規律違反ではない。

**フォーカス要求のトークンは行を閉じるたびに 0 に戻す。** `RecordingDetail`
（延いては `RecordingPlayer`）は展開の真偽で mount/unmount されるため、0 に戻さず
値を残したまま行本体タップだけで開き直すと、Play を経由していないのに新しく
マウントされたプレイヤーの初回 effect が古いトークンを見てフォーカス/スクロールを
再発火させてしまう。閉じた時点で 0 に戻すことで、非 0 になるのは再び Play を
押したときだけになる。

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

- **チャンネル種別（`channelType`）・`qTarget` は UI に出さない。** チャンネルは
  個々のサービスを選べる `<ChannelPicker>`（`serviceId`）の方が細かく絞れ、
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

`/live` の `serviceId` は `{ serviceId: ... ?? undefined }` の形に揃えてある
（`routes.tsx`。`routes.test.tsx` が `router.state.matches` の `search` を直接見て
固定している）。`/search` の `ruleId` には同じ漏れが残っている
（下記「経緯と失敗事例」）。

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
  取得エンドポイント（`GET /api/rules/{id}`）は無く、`RulesPage` が常に全件を
  引く程度の件数という前提に乗る。同じ `queryKey` の展開行が複数同時にマウント
  されても react-query が 1 回の取得にまとめる
- **参照先のルールが削除済みでも壊れない。** `rules.find` が見つからなければ
  `#N` 表記に落とす --- `recordings.rule_id` は「その時点で使われた事実」として
  残る一方、`rules` 行は削除されうる非対称なので、参照が消えているケースは
  例外ではなく通常の状態として扱う
- **原則「固有名詞はリンク」に従い**、ルールの識別（名前 or `#N`）そのものを
  リンクテキストにする。リンク先は `/search?ruleId=N`（`RuleRow` の
  「検索しながら編集」と同じ着地先、ルールの実質的な編集画面）。もう 1 本の
  リンク「このルールの録画で絞る」は `/recordings?ruleId=N` --- 同一ページ
  （`/recordings`）内の検索条件変更であり、既存の `parseRecordingsSearch`
  （`lib/recording-search.ts`）を通るので条件チップにもそのまま出る

## 経緯と失敗事例

- ブラウザ再生は M3-5、ごみ箱ビューの非表示は M3-18、録画検索の同居は M3-25 で
  実装した
- 録画単体の着地先（`/recordings/$id`、`GET /api/recordings/{id}`）は
  M6-4（issue #232）。一覧の行展開（`RecordingDetail`）をそのまま単体ページに
  も渡す形にし、実装を 2 系統に分けなかった。最初の実装は単体ページの再検証を
  `RecordingActions` にだけ `onMutated` prop で配線したため、同じ `RecordingDetail`
  配下の別の mutater（`AddEncodeProfilesAction`）が素通しになり、「事後エンコード
  を依頼しても単体ページの『追加済み』表示が更新されない」不具合をレビューで
  実機再現された。配線を prop で持つ形自体が「次に mutater を足す人が通し忘れる」
  穴を残すので、単体ページのクエリキーを一覧の invalidate に前方一致させる形
  （`recordingDetailQueryKey`）に直した
- 再生ボタンを行右端に独立させる決定と「`.play()` を呼ばない」判断は M5-4（issue #227）。
  レビューで一度差し戻された --- `<video>` に `tabIndex={-1}` を付けた最初の版は
  「native controls の個々のボタンにフォーカスが行くのでコンテナは Tab 対象で
  なくてよい」という**測らずに書いた理屈**で正当化していたが、実 Chromium では
  逆に `<video controls>` 自身が唯一の Tab stop であり、`tabIndex={-1}` を付けると
  展開後にキーボードだけではプレイヤーへ一切到達できなくなる退行だった
  （CLAUDE.md「測っていない挙動を断言しない」に当たる実例が自分の PR で起きた）
- 再生位置を localStorage に置きサーバー側視聴履歴を作らない決定は
  issue #14 の論点 7c
- `qTarget` を UI に出さない判断の根拠にした UI 案は issue #137
- M3-24（#136）でつなぎとして固定 `limit: 200` を入れていたが、
  `useInfiniteQuery` への置き換えで外した
- 録画 → ルールの導線（「ルール」セクション）は M6-2（issue #230）で実装した。
  ルール → 録画の逆方向（`RuleRow` の「このルールの録画」）は issue #137 の
  時点で既にあり、逆方向が無い非対称は issue #221 の実態調査で見つかった
- `validateSearch` の「省略では消えない」漏れは実機で確認した（壊れた URL の
  不正な値がチップにそのまま出た）。`/search` の `ruleId` も同じ関数形
  （`Number.isFinite(n) ? { ruleId: n } : {}`）で同じ経路で漏れることを
  PR #193 のレビューで確認した（`{ ruleId: "abc" }` が `useSearch()` に
  そのまま届く）。`/live` の `serviceId` は M4-4（#92）で明示代入の形に揃えた。
  `/search` の `ruleId` と `parseRuleId` の非整数は
  [issue #194](https://github.com/fetburner/rokuban/issues/194) に残っている
