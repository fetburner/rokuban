> [web/e2e/README.md](../README.md) の一部。索引から辿る

### SSE 抜きでの定期再取得・接続断バナー（`sse-refresh.mjs`）

SSE の通知を 1 通も届けないまま接続だけ維持したとき、**定めた周期で REST 再取得が実際に起きるか**を、リクエスト数を数えて判定する。周期と対象は運用状態 60 秒 / ストレージ残高 5 分 / EPG 10 分で、[docs/api/sse.md](../../../docs/api/sse.md) §レベルトリガーの対称性に書いてある。時計は `page.clock.runFor` で進めるので 10 分待たない。`design.mjs` と同じく `/api/**` を丸ごと差し替えるため、mirakc も DB も Go サーバーも要らない。

**接続断バナー（`components/connection-banner.tsx`、issue #456）もここで見る**。 `/api/events` を `page.route` で abort し、`disconnectedBannerDelayMs`（10 秒。`page.clock` の仮想時計で進める）後に帯が出る。

そこから route を復旧させ、帯が消えるまでを実ブラウザで確認する。復旧の再接続はブラウザ実装側のタイマー（実時間、`page.clock` は進めない）に依るため実時間でポーリングする。加えて、帯と `CircuitBreakerBanner`（`/api/breakers` を 1 件返して同時に出す）が `PageHeader` と `getBoundingClientRect` で交差しないことも見る。スクロールして両方を sticky の「張り付いた」状態にしてから測る。未スクロールでは top のずらしを外しても重ならずに通ってしまうためだ。

**単体テスト（`src/lib/events.test.tsx`）と重なっていない部分がここの存在理由。** 単体テストはフックとテスト用のクエリキーしか通らないので、**画面が実際に使っているキーを取りこぼしている**という壊れ方を検出できない。実際、`epg` トピックが番組リスト（`useInfiniteQuery` の手書きキー `['/api/programs', 'infinite', ...]`）に一度も届いていなかった。これを見つけたのはこの判定である（詳細は docs/api/sse.md §レベルトリガーの対称性）。同じ形の漏れを押さえるため、**画面を 3 つ開く**。ページごとにカウンタを作り直すので、増分はそのページの回復経路だけを表す。

- 番組表（`/programs`）--- 番組リストの手書きキーが EPG の 10 分側で回るか
- 予約詳細（`/reservations/$site/$programId`）--- 生成キーのままで EPG の 10 分側に落ちていないか（キーの先頭要素が所属を決める）
- 録画一覧（`/recordings`）--- `StorageBalance` の設置先。生成キー `['/api/storage']` が実画面の上で 5 分周期に乗るか。単体テストは生成キーを import して押さえられるが、**その画面がそのコンポーネントを本当に載せているか**はここでしか出ない

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:sse-refresh
```

`badge-links.mjs` と同じ ⓪（配っている bundle と `dist/` の一致）を自分で確認する。

### 番組リストの操作列（`reserve-visibility.mjs`）

番組リストの予約 / 取消 / ライブボタンを含む操作列を「ホバー / フォーカスした行・展開中の行」だけ立てる（issue #310 / #755）。判断は [docs/frontend/reservations.md](../../../docs/frontend/reservations.md) §番組リストの操作列はホバー / フォーカスした行だけ立てるに従う。**この開閉は `:hover` / `:focus-visible` / `pointer:` メディア特性で駆動するので jsdom では原理的に測れない**。jsdom はレイアウトを持たず、`getBoundingClientRect().width` は常に 0 になるためだ。`pnpm test` は「常時開いたまま」というクラス名の変異を検出できない。単体側（`program-row.test.tsx`）が見るのは `group` / `peer` マーカーと `data-testid` の配線だけで、可視性はここが唯一の判定手段。

見るのは 4 状態である（すべて操作列の実描画幅 `getBoundingClientRect().width` を直接読む）。畳は約 0px、通常行の開は 97px、放送中行はライブボタン分を足した 141px である。どちらも `box-content` でボタン合計幅を content box として確保した上に、右 padding 16px と `border-l` の 1px が外側に乗った外寸:

- ① 細ポインタ（既定の Chromium = hover:hover + pointer:fine）: ホバーもフォーカスもしていない通常行と放送中行は畳む。ホバー・`:focus-visible` でそれぞれ 97px / 141px まで開く（両方向）。あわせて次の 3 つを測る。ホバー前後で行の高さが変わらない（CLS 無し）こと。開いている列の中でボタンが `overflow-hidden` に切られていない（`scrollWidth <= clientWidth`）こと。開いている通常行でのワンタップ予約が実際に `PUT .../intent` を飛ばすこと
- ② 細ポインタで展開すると、行ヘッダから hover / focus が外れても見えたまま（展開パネルは `.group` の外の兄弟なので `peer-aria-expanded` を pointer 種別で縛らないことで担保）。通常行の展開幅が 97px のまま、折りたたみ直すと消える
- ③ タッチ / 粗いポインタ（hasTouch + isMobile = hover:none + pointer:coarse）: 通常行と放送中行の折りたたみ行は畳み、展開行はそれぞれ 97px / 141px まで開く。加えて外付けキーボード想定で `:focus-visible` だけでも開く（WCAG 2.4.7 / 2.4.11）
- ④ 折りたたみ行の操作列を実座標へ `page.touchscreen.tap()` で生タップ（ロケータのアクショナビリティ判定を迂回する）しても PUT が飛ばない・トーストも出ない。**これがレビューで見つかった欠陥そのもの**である。`opacity-0` では見えない 80×56px がヒットテストに残って予約が成立していた。幅 0 + `overflow-hidden` は列そのものを畳むので飛ばない。放送中行は操作列に遷移する `<a>`（ライブボタン）が入っているため、同じ生タップの後も URL が `/programs` のままで `/live` へ遷移していないことも測る

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は `page.clock.setFixedTime` で固定）で mirakc も DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reserve-visibility
```

### 検索フォームの初画面と詳細条件（`search-mobile.mjs`）

`/search` は条件フォームの大半をチップ列（サービス・チャンネル種別・ジャンル・時間帯）が占める（issue #305）。issue #685 では、キーワード入力を先に使えることと、詳細条件を必要なときだけ編集できることを両立させる。レイアウト・可視性・スクロールは jsdom の `getBoundingClientRect()`（常に 0 を返す）では測れないため、360/390px の Chromium で次を確認する。

①②は `page.goto` 直後に**一切スクロールも操作もせず**測る:

- ① 「検索」ボタンの矩形がビューポート内に収まり、モバイルのボトムタブと重なっていないこと
- ② テキスト条件 1 行目の対象とモードが同じ行にあり、値入力がその下の専用行でフォーム幅をほぼ使うこと。「条件を追加」を押さずに直接入力できることも確認する。360px では実測 302px（フォーム内 328px）、390px では 332px（同 358px）であり、旧一行レイアウト相当の実ブラウザ測定（360px: 62px、390px: 92px）から大きく改善している。対象・モード・値を `w-28 shrink-0` の一行へ戻す変異は失敗する。

③は URL (`cond`) からジャンルとチャンネルを復元して測る:

- 詳細条件が初期状態で閉じ、「設定中の詳細条件: 2件」と種別ごとの要約が見えること
- 詳細条件の表示ボタンをキーボードの Enter で開き、日本語の入力を編集できること
- ふたたび閉じ、要約の解除ボタンでジャンルだけを下書きから外せること

④だけは定義上「押した後」なので、①②を測り終えてから操作する:

- ④ テキスト条件に打って「検索」を押した後、件数行（`N 件`）が折り目の中にあり、`sticky` なページヘッダの下にも潜っておらず、結果の 1 件目も折り目の中にあること。変更前も結果には届いたが、実測（390px）は `scrollY=1138` まで必要だった（1280px では 948）。変更後は 390px で `scrollY=502`（1280px では 426）、件数行 y=49、1 件目 y=81 になった。検索と条件クリアは条件欄の後ろに置き、固定バーにはしない。値欄の Enter でも検索でき、送信後は結果の先頭へスクロール・フォーカスする。
- ⑥ 長いサービス名と有料番組の `ProgramRow` で、メタ行が 1 行に収まり、行が不要に 2 行ぶん高くならないこと。メタ行・行の高さは固定値で判定する（実測は 360/390/1280px いずれも `ProgramRow` 65px・メタ行 20px、しきい値は行 72px・メタ行 24px）。`flex-wrap` を戻した変異、`py-2.5`→`py-5` の変異、名前カラムに 2 行目を足す変異のいずれでも実際に落ちる。ただし `flex-wrap` を捕まえるのは computed style の判定であって高さではない（`ProgramRow` のサービス名は `truncate` なので、折り返しを許してもメタ行は 20px のまま）。

1280px では対象・モード・値の top がほぼ一致すること（= 同じ行にある）を見て従来のデスクトップの一行レイアウトを確認し、④と⑥も同じ条件で実行する。`sm:flex-row` を落として一行を崩す変異で実際に落ちることを確認済み。`page.route` のスタブは mirakc も DB も使わず、検索 API と番組詳細の 2 本を差し替える。

**ボトムタブは `nav[aria-label="主ナビゲーション"].fixed` で指す。**この `aria-label` の `<nav>` はサイドバーとボトムタブの 2 本あり、`.last()` で当てると `AppShell` の DOM 順に依存する。順が入れ替わると 390px でも `hidden md:flex` のサイドバー側を掴んで矩形が null になり、**重なり判定が黙って消えて全体は green のまま**になる。`md` 未満で矩形が取れないことは NG として報告する（スキップしない）。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:search-mobile
```

### 検索画面の操作位置・Enter・空の状態（`issue-1232-search.mjs`）

390px で検索・クリアのボタンが条件の後ろに続くこと、値欄の Enter が検索だけを送りルール作成を送らないこと（ルール名欄は逆）を判定する。未検索の EmptyState に走査線が無く、読み込み中の Skeleton にだけあることも判定する。`/api/**` は `page.route` で差し替えるので mirakc も DB も要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:issue-1232-search
```

### 予約一覧の副情報がシェブロンに重ならないか（`reservations-mobile.mjs`）

予約一覧の行の副情報（局名・日時・尺・状態バッジ）が折り返さないコンテナに `shrink-0` の可変幅要素を並べていると、モバイル幅で長い局名 + 状態バッジの組み合わせがシェブロンに重なる。折り返し・overflow・要素間の重なりは jsdom（`getBoundingClientRect()` が常に 0 を返す）では原理的に測れない。既存の単体テスト（`pages/reservations.test.tsx`）は「局名の文字列が行の中に居る」ことしか見ておらず、この壊れ方を検出できない。

見るのは 360px 幅（レビューの実測条件）で:

- ① 副情報コンテナ（`[data-testid="reservation-secondary"]`）が横方向にオーバーフローしていない（`scrollWidth <= clientWidth`）
- ② 副情報の各子要素の右端がシェブロン（`[data-testid="reservation-chevron"]`）の左端を超えていない。①はコンテナが中身を外に漏らしていないことしか見ないので、コンテナの外形自体が食い込む場合を捕まえるにはこちらが要る
- ③ ページ全体が横スクロールしない（退行の網。単体では①②の代わりにならない）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reservations-mobile
```

### 予約一覧のシリーズ表示（`reservations-series.mjs`）

予約をシリーズでまとめた行の、位置・重なり・寸法を実ブラウザで測る。jsdom は横スクロール・当たり判定・スクロール位置を測れない。360 / 390 / 1280px で次を判定する。

- ページ本体が横にスクロールしない
- 開閉の標的と 1 本だけの行の詳細リンクが 44 × 44px 以上、ハブへの導線・出自のリンクが 24 × 24px 以上
- ハブ・出自・容量不足バッジの当たり判定を行本体が奪わず、クリックで宛先へ遷移する
- 開閉できる行の右端が ∨、1 本だけの行が ›
- 棚の無いシリーズの「まだ録画なし」が、その行の中に 1 つだけ見える（デスクトップ幅は空枠の中、モバイル幅はメタ行）。リンクにはならない
- 見出しをヘッダーの下へ 24px 潜らせてから開いても、開いた行の見出しがヘッダーの下に戻る（前提が作れなければ落とす）
- 複数サイト構成のときだけ各回に site が出る
- 要確認とルールで絞った後、今後 N 本とバッジが残った予約だけで数えられる

フィクスチャで `value=null` の棚を置き、`series=null` の予約へ結合されないことも見る。`/api/**` はスタブするので mirakc・DB は要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/reservations-series \
  pnpm e2e:reservations-series
```

画像（`E2E_SHOT_DIR`）はリポジトリに入れない。レビュー用は `design-mocks` ブランチへ置く。

### 予約一覧の日付・出自・ルール絞り込み（`reservations-rule-filter.mjs`）

予約一覧の時間順ビューで、ローカル日付の見出しがスクロール後もページヘッダーの下に留まること、ルール出自リンクが行全面リンクより手前でクリックできることを測る。360 / 390 / 1280px で日付見出しと出自リンクを確認し、同じ幅でルールメニューが画面内に収まり、各項目が 24 × 24px 以上であることを確認する。360px ではページの横スクロールが無いことも判定する。7px 刻みでスクロールし、日付見出しとヘッダーのチップの中心が行の中身に遮られないこと、時刻欄のクリックで詳細へ遷移することも測る。light / dark・各幅のスクリーンショット（時刻順・スクロール後・ルールメニュー・ルール絞り込み）を `E2E_SHOT_DIR` へ保存する。

`/api/**` をブラウザー側でスタブするので mirakc・DB は要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/reservation-rule-shots \
  pnpm e2e:reservations-rule-filter
```

### 予約一覧の容量確認失敗（`reservations-capacity-error.mjs`）

予約一覧の容量超過 API が失敗すると、失敗を空配列として扱った「確認が要る予約はありません」を表示してしまうことがある。実ブラウザで API の全リトライを失敗させ、容量を確認できない理由と再試行を表示すること、attention フィルタの空状態を確定しないこと、同じ予約に正常な容量応答を返したときバッジと要確認チップが復旧することを見る。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reservations-capacity-error
```

### 検索欄の `/` フォーカス（`search-shortcut.mjs`）

`/recordings`・`/series`・`/search` で `/` を押したときのフォーカス移動と、入力欄・textarea・contenteditable・IME 変換中で文字入力を妨げないことを実ブラウザで見る。番組表と、プレイヤーが見えている録画詳細ではこのキーを横取りしないことも確かめる。`/search` の詳細条件は閉じたままにする。判定を足した直後に未実装の base で失敗することを確認してから実装する。

```sh
pnpm build && pnpm preview --port 40773 --strictPort &
E2E_URL=http://localhost:40773 pnpm e2e:search-shortcut
```
