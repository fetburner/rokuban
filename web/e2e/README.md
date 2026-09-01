# 実ブラウザでの受け入れ確認

**jsdom が測れないもの（レイアウト・スクロール位置・可視判定・色）を判定するための道具。**

`pnpm test`（Vitest + jsdom）はレイアウトを計算しない。`getBoundingClientRect()` は常に 0 を返し、
`IntersectionObserver` も無い。したがって**スクロール位置・要素の可視性・レイアウトシフトに関する
機能は、ユニットテストが全部通っても何の保証にもならない**。

この規律は番組リストの遡行（前の時間窓をリスト先頭に差し込んで、見ている位置を保つ機能。
**現在は廃止済みで存在しない**）で生まれた。実際に**「テストが通った」を根拠に 3 回
リリースして 3 回とも実機で壊れていた**。壊れ方はそれぞれ違った。

1. `document.documentElement.scrollHeight` の差分で補正 → 差し込み直後の高さは見積もりで、
   実測が後から届いて再びずれる
2. DOM のアンカー要素を掴んで位置を合わせる → **仮想化では差し込んだ瞬間にその要素が DOM から
   消える**ので、補正が一度も走らない
3. `scrollToIndex` で復元 → ボタンがリスト最上部（画面外）にあり、押すためのスクロールが
   アンカーの記録より先に走って、記録する行が変わっていた

いずれも jsdom では**原理的に検出できない**。ここに置いた判定はそのためのもの。

`lib.mjs` は各スクリプトの前置き（ブラウザの起動・終了、`/api/**` の配線、配って
いる bundle が `dist/` の現物と一致するかの確認、フィクスチャが orval 生成の
zod スキーマと一致するかの確認（`validateFixturesOrExit`。詳細は下記
§デザイン）、結果の集計と終了コード）を共有する。各スクリプト固有の判定
（何が OK/NG かの基準）はここには置かず各 *.mjs 本体にとどめる ---
フィクスチャ自体（どの値を使うか）はスクリプトごとに違うので、
`validateFixturesOrExit` は「フィクスチャ配列 → 呼び出し側が組む」形にして
判定ロジックだけを共有する。

## 使い方

ブラウザは初回だけ取得する。

```sh
pnpm install
pnpm exec playwright install chromium webkit  # webkit は live.mjs の⑥に要る
```

サーバーを起動しておく（`go:embed` なので **web を変更したらバイナリを作り直す**こと。
`docs/runbook.md` 参照）。

```sh
cd web && pnpm build
go build -o /tmp/rokuban ./cmd/rokuban
/tmp/rokuban server --roles api --config dev.local.yml
```

判定する。

```sh
pnpm e2e                              # 既定で http://localhost:40773
E2E_URL=http://localhost:40775 pnpm e2e
```

### 参照バッジの導線（`badge-links.mjs`）

容量不足バッジ（予約一覧）から番組表への導線（issue #233 M6-5、`view` の URL 化は
issue #437）。見るのは主に 2 点 --- ①バッジが行本体の詳細リンクの中に入れ子の
`<a>` として置かれておらず、クリックすると番組表（`/programs?view=grid&at=...`）へ
飛ぶこと、②`lg` 以上ではリンクが積んだ `view=grid` どおりグリッド表示になり、
不足区間の帯がスクロール後に可視範囲へ入っていること（②' として「今日」ボタンを
押した後 `at` の位置ではなく現在時刻へスクロールし直すことも見る）。加えて③として
`lg` 未満（グリッドが出ずリスト表示のまま）でもクリックが機能しエラーにならないこと
まで見る --- ①②は 1280px でしか開かないため、この経路を通さないと隠れたバグに
気付けない。②・②' はスクロール位置そのものの判定なので、jsdom（`pnpm test`）では
原理的に確認できない。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）で、mirakc も実チューナーも DB も要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:badge-links
```

**配っている bundle が `dist/` の現物と一致するかは、スクリプト自身が毎回 ⓪ として
自動で確認する**（`verifyBundleMatches`。不一致なら他の判定をせず即 exit 1 する）ので、
`curl`/`ls` で手動照合する必要はない。これは実際に踏んだ事故（`--strictPort` を
付けていても、複数の worktree を並行して触っていると別の worktree の preview が
同じポートに先に居座っており、自分の起動が黙って失敗して `E2E_URL` が無関係な
古いビルドを指したまま判定が進んでしまった）の再発を、人の確認忘れに頼らず機械で
止めるための仕組み。ポート自体は空いているものを選ぶこと（`--strictPort` が起動を
失敗させるので、選び間違えればここで気付ける）。

### ライブ視聴（`live.mjs`）

番組リストと違い、**mirakc も実チューナーも要らない** --- HLS プレイリスト/
セグメントは `page.route` でブラウザ側から丸ごと差し替える。動的 import
（hls.js のバンドル分割）・MSE への実再生・チャンネル切替時の cleanup は
jsdom で原理的に測れず、`vi.mock` によるフェイクの配線検査（Vitest）だけでは
「配線が呼ばれること」までしか見えない。手順は
[docs/runbook/live.md](../../docs/runbook/live.md) §②。

```sh
E2E_LIVE_SERVICE_A=9001 E2E_LIVE_SERVICE_B=9002 pnpm e2e:live
```

`E2E_LIVE_SERVICE_A` / `_B` に渡すのは **SI の `serviceId`**（`E2E_LIVE_NETWORK_ID`
と組で、DB へ投入した行の `(network_id, service_id)` そのもの。既定は `network_id=1`）。
`/live` のページ URL は他画面と同じ `?service=<Service.id>`（合成 id）で、
スクリプトは起動時に `GET /api/sites/{site}/services` から対応する `Service.id` を
引く（`resolveServiceId`）--- 合成規則をスクリプト側に複製しない。セグメント/
プレイリスト/離脱ヒントの URL には従来どおり SI の `serviceId` がそのまま載る
（issue #217。streamer 側の資源同定は変えていない）ので、env の投入例
（DB の `network_id` / `service_id` 列）は変わらない。`GET /api/capabilities` も
`page.route` で `{live: true}` に差し替えるので、サーバー側の `live.enabled` は
false（既定）のままでよい --- 差し替えないと画面が「無効です」になって
①〜⑦が全滅する（issue #209）。①〜⑦は「再生」ボタンを押した後の挙動を見るもの
なので、`page.goto` の直後に `clickPlay` でボタンを押す手順が入っている
（issue #234 M7-1。下記⓪参照）。

**⓪ 選択と視聴開始の分離（issue #234 M7-1）は ffmpeg フィクスチャに依存せず、
bundled Chromium だけで常に測れる。** チャンネルを開いた直後にプレイリスト/
セグメント要求が飛ばないこと、「再生」ボタンを押した後に初めて飛ぶことを
`page.route` の要求ログで観測する --- 「タップだけでは要求が飛ばない」ことは
jsdom では判定できない（`fetch` を丸ごとモックする Vitest のテストは、mock 自体を
呼ぶかどうかしか見られず、`<video src>` への直接代入のように `fetch` を経由しない
実ブラウザの資源取得は原理的に検出できない）ため、ここが唯一の判定手段になる。
この判定を足す前の実装（チャンネルをタップした瞬間に probe する版）で実際に
落ちることを確認済み（詳細は issue #234 の実装 PR #259 の変異リスト）。

**この判定手段（①〜⑦）が実際に本番相当の回帰を 2 件発見した。**

1. `supportsNativeHls` が実 Chrome の `canPlayType` の戻り値 `'maybe'` を誤って
   ネイティブ対応と判定し、Chrome がサイレントに再生できなくなる
2. **その修正（`'probably'` のみを対応と見なす）がどの実ブラウザでも false に
   なり、Safari までが hls.js 経路に落ちる。** この回帰は①〜⑤（Chromium 系
   だけ）では検出できず、**e2e 緑のまま通った** --- 「実ブラウザで測っている」
   ことは「壊れる側のブラウザで測っている」ことを意味しない。⑥（WebKit）を
   足して初めて機械判定できるようになった

詳細は [docs/runbook/live.md](../../docs/runbook/live.md)（実機確認の判定項目と回帰の記録）。

### デザイン（`design.mjs`）

**色は jsdom では測れない。** Tailwind のクラスは解決されず、oklch も計算されない。
`pnpm test` が全部通っても、色については何の保証にもならない ---
[docs/frontend/design.md](../../docs/frontend/design.md) の「合否は画素で測る」に
実行可能な形を与えるのがこれ。

```sh
# 1) SPA を配れるサーバーを 1 つ立てる（API は下記のとおり全部差し替えるので何でもよい）
pnpm build && pnpm preview --port 4173 --strictPort &

# 2) 撮る + 判定する
E2E_URL=http://localhost:4173 pnpm e2e:design
```

`go:embed` 経路（`rokuban server --roles api`）に `E2E_URL` を向けても動くはずだが
**未検証**。API は全部差し替わるのでサーバーは静的配信しかしないという理屈だけで、
実際に回してはいない。

**mirakc も実チューナーも DB も要らない。** `/api/**` は `page.route` でブラウザ側から
丸ごと差し替える（`live.mjs` が HLS でやっているのと同じ手）。時刻も
`page.clock.setFixedTime` で固定してあるので、ショットの差分は実装の差分だけになる。

**フィクスチャは契約で検証される。** 「唯一の視覚オラクル」が欠損データのまま
撮れていても、契約（`openapi.yaml`）が動くたびに誰も気付かない、という壊れ方が
実際にあった（ルールの `textMatches` が旧形 `{ field, kind }` のまま
`{ target, mode }` に追従しておらず、ルール一覧に「undefinedに…を含む」が
描かれたまま exit 0 していた）。判定本体は `validateFixturesOrExit`
（`e2e/lib.mjs`）--- `verifyBundleMatchesOrExit` と同じ**前提条件**チェックで、
スクリプト固有の OK/NG 判定ではないため共有ハーネス側に置いてある。フィクスチャを
orval 生成の zod スキーマ（`web/src/api/zod.ts` の `List*ResponseItem`）で
`parse` し、1 件でも不一致なら他の判定を一切せず exit 1 する（⓪ の
`verifyBundleMatchesOrExit` と同じ「前提が崩れていたら打ち切る」扱い）。
`design.mjs` に加えて `badge-links.mjs` / `sse-refresh.mjs` /
`grid-reserved.mjs` / `reservations-mobile.mjs` も自分のフィクスチャで呼ぶ。
**契約を変えたら、フィクスチャを持つ各スクリプトも同じ PR で直す。**

`zod.ts` は `import.meta.env` 等 Vite 依存を持たない素の TypeScript なので、
Node の ESM スクリプトから `../src/api/zod.ts` を直接 import できる ---
追加のローダー（tsx・vite-node）は要らない。Node 22 は型注釈だけを消す型
ストリッピングを既定で持ち（`.node-version` の 22.23.1 で実際に import
できることを確認済み）、`zod.ts` は enum・namespace・parameter properties の
ような変換が要る構文を持たないため、これだけで通る。`package.json` の
`e2e:design` 等の各スクリプトもそのままで変更していない。

`design.mjs` は画面のテキストに欠損文字列が混ざっていないかも全 40 ショットに
掛ける ---「安いので全画面に掛ける」。見るのは `undefined` / `NaN`
（単語境界 `\b`）と `[object`（前方一致）。**`null` は対象にしない** ---
番組名・ルール名に偶然「null」を含む文字列が来ると単語境界だけでは区別できず
偽陽性になりうるため。

出るもの:

- `e2e/screenshots/*.png`（追跡しない）。主要 7 画面（M8-3 でホームを追加）×
  ライト / ダーク × デスクトップ / モバイル、加えて番組表グリッド・
  サーキットブレーカー発動中・モバイルの「その他」を開いた状態・
  読み込み中（Skeleton の走査線を撮るため録画一覧の応答を遅延させたもの）・
  空状態（EmptyState の走査線。既定のショットでは折り返しの下に隠れて
  文字が写らないので、スクロールしてから撮る）・
  ホームの全セクション空状態（`home-empty-*`）の 40 枚。
  **人が見て判断するための成果物**で、機械が比較するものではない
- 合否（exit code）。以下をすべて実画素・実描画で判定する:
  - 状態色（塗りか文字か / 赤か琥珀か）・地の無彩性・**WCAG コントラスト**
    （文字は 4.5、面と線は 3 が下限）。`bg-muted` 系の淡い面に乗る文字は、
    塗り・`/80` の sticky 日付見出し・`/30` の録画詳細パネルに加えて
    **hover 中の面**（一覧の行の `hover:bg-muted/50`）まで測る --- Lighthouse は
    hover を測らないので、監査に出ない面はここでしか押さえられない
    （下限を割っているものは `knownGaps` に理由込みで載る）
  - **和文が実際に Noto Sans JP、英数字が実際に Geist で描画されているか**
    （CDP `CSS.getPlatformFontsForNode` で番組リストの行（`li[data-program-id]`）
    の実使用フォントを見る --- `main` や `body` のようなブロック要素だけを
    子に持つノードを渡すと常に空配列が返るため使えない。
    `getComputedStyle().fontFamily` は指定文字列を返すだけで実描画の保証には
    ならない）。あわせて**和文まじりの文字列でも tabular-nums が実際に等幅を
    作っているか**を DOM の実測幅で見る（`docs/frontend/stack.md`
    「フォントは英数字と和文で 2 書体を使い分ける」）
- 色以外にも、jsdom では原理的に測れないキーボード到達性を 1 件持つ:
  録画一覧の行リンクを Enter で開いて詳細（`/recordings/$id`）へ遷移し、
  詳細で Tab 走査だけで `<video>` へ到達すること（視聴は詳細ページに寄せる）。
  `<video>` に `tabIndex` を明示すると（jsdom の focus spy
  では検出できない形で）Tab 走査から外れてしまう退行が M5-4（issue #227）
  で実際に一度起きたため、ここで実ブラウザから固定している
- 共通 `Button` のフォーカスリング（`:focus-visible` の `ring-3` = box-shadow、
  および `border-ring` = 1px 罫線の border-color）が遷移**しない**こと。
  `transition-all` は CSS が実際に発火するかどうかまでは jsdom はもちろん
  `getComputedStyle` の 1 回読みでも確認できないため、ボタン要素に張った
  `transitionstart` イベント（プロパティ名つき）を実ブラウザで観測する ---
  box-shadow / outline* / border-*-color のいずれかが遷移対象に上がったら
  NG（border-color だけを見落としたレビュー指摘が実際にあったため、
  ロングハンド込みで前方一致を掛けている）。同じ手で両方向を確認する:
  hover の背景色の遷移は従来どおり起きること、`active:...:translate-y-px`
  の押下フィードバック（Tailwind v4 では `translate` プロパティにコンパイル
  される）は遷移し**続ける**こと
- 接続断バナー（`components/connection-banner.tsx`、issue #456）の地が無彩か。
  `/api/events` は明示のスタブを持たず catch-all（200 json）に落ちるので、
  Content-Type 不一致で SSE が即座に失敗し、追加の配線なしで「切断中」を作れる。
  `disconnectedBannerDelayMs`（10 秒）分は実時間で待つ
- 測ったコントラストの表。**数値の権威はこの出力**で、docs には転記しない
- モバイルの「その他」ポップオーバー（`components/app-shell.tsx` の `MoreMenu`）
  を開いた状態の判定。固定されたボトムバーの上に浮くオーバーレイなので、
  はみ出し・重なりは jsdom（`app-shell.test.tsx`）では原理的に測れない。
  ここで実測するのは 3 点: ボトムタブが常に 4 個か / 開いたポップオーバーが
  ビューポート内に収まるか / ポップオーバーがトリガーの上端より上に出るか
  （バーの下に隠れていないか）。開いた状態のショットも `more-menu-open-*.png`
  として出る
- `prefers-reduced-motion: reduce` で主要な動き（Skeleton の
  `animate-pulse`・ポップオーバーの
  `slide-in-from-*`/`zoom-in-95`・共通 `Button` の `translate` 遷移）が
  縮退し、既定（`no-preference`）では従来どおり動くこと。**両方向**を見る
  --- 縮退側だけの判定は、動きを恒久的に殺した実装も通してしまう。
  Playwright の `reducedMotion` コンテキストオプションで OS 設定を
  エミュレートし、実要素の `getComputedStyle().animationDuration` /
  `transitionDuration` を実測する（jsdom は matchMedia も CSS の適用も
  測れない）

判定の設計で外してはいけない点が 2 つある。

- **半透明の地は合成してから測る。** `text-warning` が乗るのは地ではなく
  `bg-warning/10` の上。地に対する比だけを見ると 0.5〜0.7 甘い数字が出る
- **下限を割ると分かっていて直さないものは `knownGaps` に理由込みで書く。**
  合否からは外れるが「既知の不足」として毎回出力される。閾値を静かに下げない

ダークは Playwright context の `colorScheme` で OS 設定をエミュレートする。
`design.mjs` はクラスを直接付けず、アプリが初回描画前に `html.dark` へ同期する到達経路を判定する。

**`getComputedStyle()` の戻り値を正規表現で読んではいけない。**
トークンが oklch なので Chromium は計算値も `oklch(...)` のまま返し、
`rgb(...)` を期待した実装は全部の判定を「読めない」で素通りさせる。
`design.mjs` は 1px 塗って `getImageData` で実画素を採っている。

トークン外の生の色値（`bg-amber-700` / `bg-black` / `#rrggbb`）の検出は実ブラウザが
要らないので、そちらは別のコマンドにしてあり **CI の lint job で回る**。

```sh
pnpm check:colors
```

検査が見ていない書き方（動的なクラス名の合成・CSS の名前付き色・3 桁の 16 進・
`public/` の資産）は `scripts/check-colors.mjs` に書き出してあり、実行のたびに出力する。

### SSE 抜きでの定期再取得・接続断バナー（`sse-refresh.mjs`）

SSE の通知を 1 通も届けないまま接続だけ維持したとき、**定めた周期で REST 再取得が
実際に起きるか**を、リクエスト数を数えて判定する（運用状態 60 秒 / ストレージ残高
5 分 / EPG 10 分。周期と対象は [docs/api/sse.md](../../docs/api/sse.md)
§レベルトリガーの対称性）。時計は `page.clock.runFor` で進めるので 10 分待たない。
`design.mjs` と同じく `/api/**` を丸ごと差し替えるため、mirakc も DB も Go サーバーも
要らない。

**接続断バナー（`components/connection-banner.tsx`、issue #456）もここで見る。**
`/api/events` を `page.route` で abort し、`disconnectedBannerDelayMs`（10 秒。
`page.clock` の仮想時計で進める）後に帯が出る → route を復旧させる → 帯が消える、
までを実ブラウザで確認する。復旧の再接続はブラウザ実装側のタイマー（実時間、
`page.clock` は進めない）に依るため実時間でポーリングする。加えて、帯と
`CircuitBreakerBanner`（`/api/breakers` を 1 件返して同時に出す）が `PageHeader`
と `getBoundingClientRect` で交差しないことも見る --- スクロールして両方を
sticky の「張り付いた」状態にしてから測る（未スクロールでは top のずらしを
外しても重ならずに通ってしまう）。

**単体テスト（`src/lib/events.test.tsx`）と重なっていない部分がここの存在理由。**
単体テストはフックとテスト用のクエリキーしか通らないので、**画面が実際に使っている
キーを取りこぼしている**という壊れ方を検出できない。実際、`epg` トピックが番組リスト
（`useInfiniteQuery` の手書きキー `['/api/programs', 'infinite', ...]`）に一度も
届いていなかったのを見つけたのはこの判定（詳細は docs/api.md §SSE「経緯と失敗事例」）。
同じ形の漏れを押さえるため、**画面を 3 つ開く**。ページごとにカウンタを作り直すので、
増分はそのページの回復経路だけを表す。

- 番組表（`/programs`）--- 番組リストの手書きキーが EPG の 10 分側で回るか
- 予約詳細（`/reservations/$site/$programId`）--- 生成キーのままで EPG の 10 分側に
  落ちていないか（キーの先頭要素が所属を決める）
- 録画一覧（`/recordings`）--- `StorageBalance` の設置先。生成キー
  `['/api/storage']` が実画面の上で 5 分周期に乗るか。単体テストは生成キーを
  import して押さえられるが、**その画面がそのコンポーネントを本当に載せているか**は
  ここでしか出ない

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:sse-refresh
```

`badge-links.mjs` と同じ ⓪（配っている bundle と `dist/` の一致）を自分で確認する。

### 予約ボタンの可視性（`reserve-visibility.mjs`）

番組リストの予約 / 取消ボタンを「ホバー / フォーカスした行・展開中の行」だけ
立てる（issue #310。判断は [docs/frontend/reservations.md](../../docs/frontend/reservations.md)
§番組リストの予約ボタンはホバー / フォーカスした行だけ立てる）。**この可視性は
`:hover` / `:focus-visible` / `pointer:` メディア特性で駆動するので jsdom では
原理的に測れない** --- `getComputedStyle().visibility` にクラス由来の値が乗らず、
`pnpm test` は「常時見えたまま」というクラス名の変異を検出できない。単体側
（`program-row.test.tsx`）が見るのは `group` / `peer` マーカーと `data-testid` の
配線だけで、可視性そのものはここが唯一の判定手段。

見るのは 4 状態（すべて実描画の `visibility` を `getComputedStyle` で直接読む。
Playwright の `.isVisible()` は使わない --- opacity を見ないうえ、判定理由を
残せない）:

- ① 細ポインタ（既定の Chromium = hover:hover + pointer:fine）: ホバーも
  フォーカスもしていない行は見えない / ホバー・`:focus-visible` で見える
  （両方向）。あわせてホバー前後で行・予約列（`w-20`）の bounding box が
  変わらない（CLS 無し）ことと、見えているときのワンタップ予約が実際に
  `PUT .../intent` を飛ばすこと（可視性だけ切り替え、pointer-events を殺して
  いない）を測る
- ② 細ポインタで展開すると、行ヘッダから hover / focus が外れても見えたまま
  （展開パネルは `.group` の外の兄弟なので `peer-aria-expanded` を pointer 種別で
  縛らないことで担保）。折りたたみ直すと消える
- ③ タッチ / 粗いポインタ（hasTouch + isMobile = hover:none + pointer:coarse）:
  折りたたみ行は見えない・展開行だけ見える。加えて外付けキーボード想定で
  `:focus-visible` だけでも見える（WCAG 2.4.7 / 2.4.11）
- ④ 折りたたみ行の予約列を実座標へ `page.touchscreen.tap()` で生タップ
  （ロケータのアクショナビリティ判定を迂回する）しても PUT が飛ばない・
  トーストも出ない --- **これがレビューで見つかった欠陥そのもの**。`opacity-0`
  では見えない 80×56px がヒットテストに残って予約が成立していた。`visibility`
  はヒットテストと tab 順序から要素を外すので飛ばない

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）で mirakc も DB も要らない。⓪（配っている
bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reserve-visibility
```

### 番組表グリッドの予約済み印（`grid-reserved.mjs`）

番組表グリッドで予約済みがジャンルの淡い塗りに埋もれる（issue #307）。
見える印が `ring-1`（1px）と `size-1.5`（6px）の点だけで、「予約済み」は
`aria-label` にしか無い。選択中は同じ `ring-primary` の `ring-2` なので、
差は太さだけ。`pnpm test` の既存判定は `data-reserved` と `aria-label` だけを
見ており、見た目の差は見ていない。

jsdom は色も要素の大きさも測れないので、ここが唯一の判定手段。見るのは:

- ① 予約済みセルに、aria ではない見える「予約」がある。箱が 6px の点より
  大きい（幅 16px 以上）。同じジャンルの未予約セルには無い
- ② 5 分（10px）の予約済みセルでも印が消えない --- 見える「予約」、または
  セルの高さの 8 割以上を覆う縦の帯
- ③ 予約済み（未選択）と選択中（未予約）は別の形。予約済みだけに見える
  「予約」がある
- ④ 印の色はタリー / 琥珀 / destructive ではない（色は信号のみ）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:grid-reserved
```

### ボトムタブの高さと本文の下パディング（`programs-bottom-nav.mjs`）

`--bottom-nav-height`（`web/src/index.css`）がボトムタブの実際の描画高さと一致して
いるかを 390×844 の実ブラウザで見る。jsdom は `getBoundingClientRect` を計算しない
（常に 0 を返す）ので、ここで見る値はどれもユニットテストでは原理的に取れない。

**「行がボトムタブに隠れる」（issue #303）はこのスクリプトでは直っていないし、
判定もしていない。** issue #303 の症状は初回表示や任意のスクロール位置で行がタブの裏に
半分入るもので、ページ全体スクロール + `fixed` なタブという前提を変えないと消えない
（`docs/frontend/scroll.md`「ボトムタブの裏に隠れる行」）。下の④がその量を毎回実測して
**表示するだけ**（合否には影響しない。実測 29px）。

このスクリプトが見ているのは別の欠陥 --- `--bottom-nav-height` から nav の上辺の
境界線ぶん（1px）が落ちていて、`main` の `padding-bottom`（64px）がタブの実寸
（65px）に足りていなかったこと。**この状態でも最下端での余白はちょうど 0px で、
隠れていた画素は無かった**（実測）。落ちていたのは計算の正しさと 1px ぶんの余裕だけで、
見た目に現れる症状ではない。

見るのは:

- ① `main` の計算済み `padding-bottom`（= `--bottom-nav-height`）がボトムタブの
  実際の描画高さ（border 込み）と一致すること。**境界線ぶんを落とすと 64px 対 65px
  で落ちる**（実測で確認済み）
- ② 最下端までスクロールしたとき、`main` の内容ボックスの下端がタブの上端より下に
  来ていないこと。①に加えて「`main` が文書の最下端の箱である」ことも見る。
  同じ変異で 780px 対 779px で落ちる
- ③ 最下端でタブの上端をまたいでいる行が無いこと。**これは余裕ゼロの回帰確認で、
  境界線落ちに対する検出力は無い**（上記のとおり修正前も余白 0px で通る）。①②が
  捉えない別種の壊れ方に対する網として置いてある。`min-h-14` → `min-h-16` の変異では
  ①②③すべてが落ちることを確認済み
- ④ 初回表示でタブの裏に入っている画素数の実測値（**表示のみ**）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-bottom-nav
```

### 検索の主操作がモバイル初画面に届くか（`search-mobile.mjs`）

`/search` は条件フォームの大半をチップ列（サービス・チャンネル種別・ジャンル・
時間帯）が占める（issue #305）。「390px 幅の初画面で『検索』ボタンがボトムタブに
隠れない」「テキスト条件の入力がサービスチップより先に届く」はどちらも実レイアウトの
上下関係そのもので、jsdom の `getBoundingClientRect()`（常に 0 を返す）では測れない。

①②は `page.goto` 直後に**一切スクロールも操作もせず**測る:

- ① 「検索」ボタンの矩形がビューポート内に収まり、モバイルのボトムタブと
  重なっていないこと
- ② テキスト条件 1 行目の値入力が「条件を追加」を押さずに直接見えており、
  サービスのチップ列より前（画面の上）にあること

④だけは定義上「押した後」なので、①②を測り終えてから操作する:

- ④ テキスト条件に打って「検索」を押した後、件数行（`N 件（番組 ID 順）`）が
  折り目の中にあり、`sticky` なページヘッダの下にも潜っておらず、結果の 1 件目も
  折り目の中にあること。**①②だけでは足りない** --- 主操作を上端へ動かしても総
  スクロール量は変わらないので、「押しても画面が変わらず、結果を見るために
  下までスクロールする」状態が①②とも OK のまま成立する（レビューで実測:
  クリック後も `scrollY = 0`、件数行は y=1179 で折り目 844 の 335px 下）。
  検索 API は `programId` の配列だけを返すので、`page.route` のスタブは
  `POST …/programs/search` と `GET …/programs/{id}` の 2 本で足りる

③として①②④はデスクトップ（1280px）でも走る（`checkViewport` はビューポートを
変えて呼ぶだけ）--- issue 本文が「デスクトップではボタンは見えるが、キーワードは
同じく一段奥」と言っているため。

**ボトムタブは `nav[aria-label="主ナビゲーション"].fixed` で指す。** この
`aria-label` の `<nav>` はサイドバーとボトムタブの 2 本あり、`.last()` で当てると
`AppShell` の DOM 順に依存する --- 順が入れ替わると 390px でも `hidden md:flex` の
サイドバー側を掴んで矩形が null になり、**重なり判定が黙って消えて全体は green の
まま**になる。`md` 未満で矩形が取れないことは NG として報告する（スキップしない）。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も
DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:search-mobile
```

### 予約一覧の副情報がシェブロンに重ならないか（`reservations-mobile.mjs`）

予約一覧の行の副情報（局名・日時・尺・状態バッジ）が折り返さないコンテナに
`shrink-0` の可変幅要素を並べていると、モバイル幅で長い局名 + 状態バッジの
組み合わせがシェブロンに重なる（issue #302 のレビュー指摘）。折り返し・
overflow・要素間の重なりは jsdom（`getBoundingClientRect()` が常に 0 を返す）
では原理的に測れないので、既存の単体テスト（`pages/reservations.test.tsx`）は
「局名の文字列が行の中に居る」ことしか見ておらず、この壊れ方を検出できない。

見るのは 360px 幅（レビューの実測条件）で:

- ① 副情報コンテナ（`[data-testid="reservation-secondary"]`）が横方向に
  オーバーフローしていない（`scrollWidth <= clientWidth`）
- ② 副情報の各子要素の右端がシェブロン（`[data-testid="reservation-chevron"]`）
  の左端を超えていない --- ①はコンテナが中身を外に漏らしていないことしか見ないので、
  コンテナの外形自体が食い込む場合を捕まえるにはこちらが要る
- ③ ページ全体が横スクロールしない（退行の網。単体では①②の代わりにならない）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も
DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reservations-mobile
```

### 読み込み中のレイアウトシフト（`cls.mjs`）

CLS（Cumulative Layout Shift）はレイアウトそのものの指標なので、jsdom
（`getBoundingClientRect()` が常に 0 を返す）では原理的に測れない --- Lighthouse で
検索に要改善域の CLS が出たことの唯一の判定手段になる。

ブラウザの Layout Instability API（`PerformanceObserver({type: 'layout-shift'})`）で
`hadRecentInput === false` の `value` を単純合計する。**これは Lighthouse が実際に
報告する CLS の近似であって同一ではない**（Lighthouse は session window でグルーピング
してその最大値を採るが、ここでは windowing をせず全期間の単純合計を見る --- 単純合計は
session window の最大値より大きくなることしかないので、ここで 0.10 以下なら Lighthouse
の値も 0.10 以下になる。逆方向の保証はしない、未検証）。

見るのは検索（`/search`。`components/condition-fields.tsx`）の 2 点:

- ① モバイル幅（390x844）でサービス一覧の取得を遅延させた状態（Lighthouse の
  スロットル下を模す）で読み込み中の CLS が 0.10 以下
- ② デスクトップ幅（1280x900）で①と同じ遅延を掛けた状態で 0.10 以下 --- ラボ計測は
  検索デスクトップも 0.087（しきい値の一歩手前）を報告しており、`ConditionFields` の
  対策の根拠はビューポート依存の議論（「押される側が折り目の外に出る」）なので、
  モバイルだけでは踏んでいない

サービス数は 24 局（地上波 + BS + CS 相当）にしてある --- 2 局だけでは 390px でも
チップが 1 行に収まってしまい、直す前の実装でも再現しない。**直す前の
`condition-fields.tsx`（`ServiceFields` が `TextMatchFields` の直後）では①が 0.165、
②が 0.033 で、①が実際に落ちる。**

**ホーム（`/`）はここでは見ない。** ラボ計測はホームのデスクトップでも 0.111
（スロットル時のみ）を報告しているが、原因は「4 セクションの表示順は固定なのに解決順は
不定で、後続セクションが先に見えている状態で先行セクションが実データごと上に挿し込まれる」
形で、対策には `docs/frontend/home.md`「セクションの可視性は個別に」を変える設計判断が
要る。判定手段（この形のフィクスチャ）ごとその判断のあとに足す --- しきい値を超えたまま
緑にできない判定を置くと、この受け入れ全体が「常に赤いので誰も見ない」ものになる。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も
DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:cls
```

### チップの横あふれ（`chip-overflow.mjs`）

サービスチップに補助ラベル（同名のワンセグ / サブサービスを見分けるための
リモコン番号・物理チャンネル・serviceId。issue #306）を足すと、名前だけなら
収まっていたチップが親の幅を超える。`Chip` は `shrink-0` を持つので
**flex-basis が内容の最大幅を要求し、ページ全体が横に伸びる**。jsdom は
`scrollWidth` / `clientWidth` を計算しない（常に 0）ので、この壊れ方は
`pnpm test` が全部通っても検出できない。

見るのは 320px 幅（実機で最も狭い層）で 3 点:

- ① `/search` の条件フォームに長い局名 + 補助ラベルのチップを流し込んでも
  `document.documentElement` が横スクロールしない。**`Chip` から `max-w-full`
  を外すと落ちる**（実測: 有り 320 / 320、無し 448 / 320）
- ② 解き方が「チップの中で折り返す」であること（切り落としでも隠しでもない）。
  チップの箱がビューポートに収まり、箱の中で内容があふれておらず、実際に 2 行に
  なっている。`max-w-full` を外すと①と一緒に落ちる
- ③ `Chip` は共有プリミティブなので、録画一覧の絞り込みにある短いピル
  （状態 5 件 / 種別 3 件 / ジャンル 16 件）が丸ピルのまま 1 行であること。
  ①の対策が他画面の見た目を変えていないことを逆方向から見る。ポップオーバーが
  ビューポートに収まることも合わせて測る

**`break-words` は入れていない。** ①②を `break-words` 無しでも測ったが差が出な
かった（和文は文字間で折り返せる）。長い ASCII 1 語での挙動は未検証。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:chip-overflow
```

### 個人化の持続（`personalization.mjs`）

個人設定の置き場所（[docs/frontend/design.md](../../docs/frontend/design.md)
§個人化）が実ブラウザで効いていることを見る。**jsdom では原理的に測れないもの
だけ**を対象にする --- Vitest の「再マウント」はリロードではないので、
`localStorage` が本当にページの読み込みをまたいで効くかは測れない。

見るのは 5 点:

- ① 録画一覧をカード表示に切り替えてリロードしても、カード表示のまま
  （`aria-pressed=true`）で、URL には何も載っていない
- ② カード表示が本当に段組みになり（1 行目に 2 枚以上）、サムネイルの枠が
  行表示より広い。**列数もサムネイルの実寸も jsdom では 0 のまま**なので、
  グリッドのクラスが当たっていない退行はここでしか出ない
- ③ 検索を押すと条件が `?cond=` に載り、リロードで条件と結果の両方が戻る。
  リロード後に走る検索が **1 回だけ**であること（URL のハイドレーションと
  フォームの初期化で二重に叩く退行の検出）
- ④ 条件なしで `/search` を開くと、前回の条件はフォームに戻るが**検索は
  走らない**（未検索の案内が出たまま）
- ⑤ 録画詳細で再生速度を 1.5× にし、一覧を経由して別の録画の詳細へ移っても、
  select と実 `<video>.playbackRate` の両方が 1.5 のまま。**測っているのは
  「選んだ速度が実 `<video>` に載り、録画をまたいで端末に残る」こと**で、
  `savePlaybackRate` を no-op にする変異で 2 行とも落ちることを確認してある。
  **同一インスタンスのまま録画だけ差し替わる経路（`playbackRate` を当てる
  effect の依存漏れ）はここでは踏めない** --- 現在の UI では必ず一覧を経由し、
  `RecordingPlayer` が新規マウントして localStorage から読み直すため。その
  退行を捕まえているのは `src/components/recording-player.test.tsx` の
  `rerender` を使う jsdom テスト（実測で確認済み）

一覧の `<ul>` は行のテキストから `ancestor::ul[1]` で辿る。素の `ul li` は
サイドバーのナビゲーションにも当たり、「1 行目に 1 枚」という無関係な値を
測ってしまう（実際に踏んだ）。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:personalization
```

## CI では回さない

実サーバーと実 mirakc のデータに依存するため、CI には載せない。**ローカルでの受け入れ確認**の
位置づけ（[docs/frontend.md](../../docs/frontend.md) の「受け入れは実機で行う」に実行可能な形を
与えるもの）。

`design.mjs` だけは実データに依存しない（API を丸ごと差し替える）ので技術的には
CI に載せられるが、いまは他と同じくローカル実行のままにしてある。実ブラウザの
取得と 40 枚のショットぶんの時間を毎 PR に払う価値があるかを、まだ測っていない。
実ブラウザ不要の `pnpm check:colors` は lint job に入っている。

## 判定を足すときの規律

**足した判定が、直す前の実装で実際に落ちることを確認すること。** 落ちない判定は何も判定して
いない（CLAUDE.md「テスト規律」のユニットテストと同じ）。

**時計を固定した判定は、時計が動くことに起因する欠陥を検出できない。** `page.clock.setFixedTime`
（このファイルの各判定）も jsdom の `vi.setSystemTime`（`pnpm test`）も時計を止めることで
ショットとアサーションの再現性を得ているが、その代償として「レンダーのたびに変わる値
（生の `Date.now()` 等）をキャッシュキーに載せて無限再取得になる」類の欠陥は、時計を止めた
構成だと `Date.now()` も毎回同じ値を返すため原理的に再現しない（`pages/home.tsx` の容量超過
クエリで実際に踏んだ。詳細は [docs/frontend/home.md](../../docs/frontend/home.md) §経緯と
失敗事例）。時計に起因する挙動を判定したいときは、時計を止めない経路を別に用意すること
（`design.mjs`・`pages/home.test.tsx` の「実時計でのクエリキー安定性」参照）。
