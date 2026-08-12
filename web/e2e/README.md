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

渡すのは **SI の `serviceId`**（URL に載る mirakc 合成 id は `live.mjs` が
`GET /api/sites/{site}/services` から解決する）。`GET /api/capabilities` も
`page.route` で `{live: true}` に差し替えるので、サーバー側の `live.enabled` は
false（既定）のままでよい --- 差し替えないと画面が「無効です」になって
①〜⑦が全滅する（issue #209）。

**この判定手段が実際に本番相当の回帰を 2 件発見した。**

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

- `e2e/screenshots/*.png`（追跡しない）。主要 6 画面 × ライト / ダーク ×
  デスクトップ / モバイル、加えて番組表グリッド・サーキットブレーカー発動中・
  モバイルの「その他」を開いた状態・
  読み込み中（Skeleton の走査線を撮るため録画一覧の応答を遅延させたもの）・
  空状態（EmptyState の走査線。既定のショットでは折り返しの下に隠れて
  文字が写らないので、スクロールしてから撮る）の 34 枚。
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

## CI では回さない

実サーバーと実 mirakc のデータに依存するため、CI には載せない。**ローカルでの受け入れ確認**の
位置づけ（[docs/frontend.md](../../docs/frontend.md) の「受け入れは実機で行う」に実行可能な形を
与えるもの）。

`design.mjs` だけは実データに依存しない（API を丸ごと差し替える）ので技術的には
CI に載せられるが、いまは他と同じくローカル実行のままにしてある。実ブラウザの
取得と 34 枚のショットぶんの時間を毎 PR に払う価値があるかを、まだ測っていない。
実ブラウザ不要の `pnpm check:colors` は lint job に入っている。

## 判定を足すときの規律

**足した判定が、直す前の実装で実際に落ちることを確認すること。** 落ちない判定は何も判定して
いない（CLAUDE.md「テスト規律」のユニットテストと同じ）。
