# 実ブラウザでの受け入れ確認

**jsdom が測れないもの（レイアウト・スクロール位置・可視判定・色）を判定するための道具。**

`pnpm test`（Vitest + jsdom）はレイアウトを計算しない。`getBoundingClientRect()` は常に 0 を返し、
`IntersectionObserver` も無い。したがって**スクロール位置・要素の可視性・レイアウトシフトに関する
機能は、ユニットテストが全部通っても何の保証にもならない**。

番組リストの遡行（前の時間窓をリスト先頭に差し込んで、見ている位置を保つ）で、実際に
**「テストが通った」を根拠に 3 回リリースして 3 回とも実機で壊れていた**。壊れ方はそれぞれ違った。

1. `document.documentElement.scrollHeight` の差分で補正 → 差し込み直後の高さは見積もりで、
   実測が後から届いて再びずれる
2. DOM のアンカー要素を掴んで位置を合わせる → **仮想化では差し込んだ瞬間にその要素が DOM から
   消える**ので、補正が一度も走らない
3. `scrollToIndex` で復元 → ボタンがリスト最上部（画面外）にあり、押すためのスクロールが
   アンカーの記録より先に走って、記録する行が変わっていた

いずれも jsdom では**原理的に検出できない**。ここに置いた判定はそのためのもの。

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

原因調査には診断のほうを使う（合否は出さず、添字と画素を出す）。

```sh
pnpm e2e:diagnose
```

### 参照バッジの導線（`badge-links.mjs`）

容量不足バッジ（予約一覧）から番組表への導線（issue #233 M6-5）。見るのは主に 2 点
--- ①バッジが行本体の詳細リンクの中に入れ子の `<a>` として置かれておらず、クリック
すると番組表（`/?at=...`）へ飛ぶこと、②`lg` 以上ではグリッド表示に自動で切り替わり、
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

渡すのは **SI の `serviceId`**（ライブの URL に載るのも SI の値そのもの。
issue #217）。`GET /api/capabilities` も
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
    （文字は 4.5、面と線は 3 が下限）
  - **和文が実際に Noto Sans JP、英数字が実際に Geist で描画されているか**
    （CDP `CSS.getPlatformFontsForNode` で番組リストの行（`li[data-program-id]`）
    の実使用フォントを見る --- `main` や `body` のようなブロック要素だけを
    子に持つノードを渡すと常に空配列が返るため使えない。
    `getComputedStyle().fontFamily` は指定文字列を返すだけで実描画の保証には
    ならない）。あわせて**和文まじりの文字列でも tabular-nums が実際に等幅を
    作っているか**を DOM の実測幅で見る（`docs/frontend/stack.md`
    「フォントは英数字と和文で 2 書体を使い分ける」）
- 色以外にも、jsdom では原理的に測れないキーボード到達性を 1 件持つ:
  録画一覧を行本体（Enter）で展開したあと、Tab 2 回以内に `<video>` へ
  到達すること。`<video>` に `tabIndex` を明示すると（jsdom の focus spy
  では検出できない形で）Tab 走査から外れてしまう退行が M5-4（issue #227）
  で実際に一度起きたため、ここで実ブラウザから固定している
- 測ったコントラストの表。**数値の権威はこの出力**で、docs には転記しない
- モバイルの「その他」ポップオーバー（`components/app-shell.tsx` の `MoreMenu`）
  を開いた状態の判定。固定されたボトムバーの上に浮くオーバーレイなので、
  はみ出し・重なりは jsdom（`app-shell.test.tsx`）では原理的に測れない。
  ここで実測するのは 3 点: ボトムタブが常に 4 個か / 開いたポップオーバーが
  ビューポート内に収まるか / ポップオーバーがトリガーの上端より上に出るか
  （バーの下に隠れていないか）。開いた状態のショットも `more-menu-open-*.png`
  として出る

判定の設計で外してはいけない点が 2 つある。

- **半透明の地は合成してから測る。** `text-warning` が乗るのは地ではなく
  `bg-warning/10` の上。地に対する比だけを見ると 0.5〜0.7 甘い数字が出る
- **下限を割ると分かっていて直さないものは `knownGaps` に理由込みで書く。**
  合否からは外れるが「既知の不足」として毎回出力される。閾値を静かに下げない

ダークは `.dark` クラスをスクリプトが直接付けて撮る ---
アプリ側に切り替え手段がまだ無いため（design.md「ダークは実行時にはまだ到達できない」）。

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

### SSE 抜きでの定期再取得（`sse-refresh.mjs`）

SSE の通知を 1 通も届けないまま接続だけ維持したとき、**定めた周期で REST 再取得が
実際に起きるか**を、リクエスト数を数えて判定する（運用状態 60 秒 / EPG 10 分。周期と
対象は [docs/api/sse.md](../../docs/api/sse.md) §レベルトリガーの対称性）。時計は
`page.clock.runFor` で進めるので 10 分待たない。`design.mjs` と同じく `/api/**` を
丸ごと差し替えるため、mirakc も DB も Go サーバーも要らない。

**単体テスト（`src/lib/events.test.tsx`）と重なっていない部分がここの存在理由。**
単体テストはフックとテスト用のクエリキーしか通らないので、**画面が実際に使っている
キーを取りこぼしている**という壊れ方を検出できない。実際、`epg` トピックが番組リスト
（`useInfiniteQuery` の手書きキー `['/api/programs', 'infinite', ...]`）に一度も
届いていなかったのを見つけたのはこの判定（詳細は docs/api.md §SSE「経緯と失敗事例」）。
同じ形の漏れ（予約詳細が生成キーのままで EPG の 10 分側に落ちる）を押さえるため、
**画面を 2 つ開く** --- 番組表（`/programs`）と予約詳細
（`/reservations/$site/$programId`）。ページごとにカウンタを作り直すので、増分は
そのページの回復経路だけを表す。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:sse-refresh
```

`badge-links.mjs` と同じ ⓪（配っている bundle と `dist/` の一致）を自分で確認する。

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
