# 実ブラウザでの受け入れ確認

**jsdom が測れないもの（レイアウト・スクロール位置・可視判定）を判定するための道具。**

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

**この判定手段が実際に本番相当の回帰を 2 件発見した。**

1. `supportsNativeHls` が実 Chrome の `canPlayType` の戻り値 `'maybe'` を誤って
   ネイティブ対応と判定し、Chrome がサイレントに再生できなくなる
2. **その修正（`'probably'` のみを対応と見なす）がどの実ブラウザでも false に
   なり、Safari までが hls.js 経路に落ちる。** この回帰は①〜⑤（Chromium 系
   だけ）では検出できず、**e2e 緑のまま通った** --- 「実ブラウザで測っている」
   ことは「壊れる側のブラウザで測っている」ことを意味しない。⑥（WebKit）を
   足して初めて機械判定できるようになった

詳細は [docs/runbook/live.md](../../docs/runbook/live.md)（実機確認の判定項目と回帰の記録）。

## CI では回さない

実サーバーと実 mirakc のデータに依存するため、CI には載せない。**ローカルでの受け入れ確認**の
位置づけ（[docs/frontend.md](../../docs/frontend.md) の「受け入れは実機で行う」に実行可能な形を
与えるもの）。

## 判定を足すときの規律

**足した判定が、直す前の実装で実際に落ちることを確認すること。** 落ちない判定は何も判定して
いない（CLAUDE.md「テスト規律」のユニットテストと同じ）。
