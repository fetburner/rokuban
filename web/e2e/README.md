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

`lib.mjs` は各スクリプトの前置きを共有する。中身はブラウザの起動・終了、`/api/**` の
配線、配っている bundle が `dist/` の現物と一致するかの確認、結果の集計と終了コード
である。あわせてフィクスチャが orval 生成の zod スキーマと一致するかの確認
（`validateFixturesOrExit`。詳細は下記 §デザイン）も持つ。各スクリプト固有の判定
（何が OK/NG かの基準）はここには置かず各 *.mjs 本体にとどめる ---
フィクスチャ自体（どの値を使うか）はスクリプトごとに違うためである。
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

### フィクスチャ契約の CI 検証

各スクリプトが使う API フィクスチャと `web/src/api/zod.ts` の生成スキーマの一致だけは、
実ブラウザを使わずに `pnpm e2e:fixtures` で検証する。これは `E2E_VALIDATE_FIXTURES_ONLY=1`
を付けて契約検証を持つスクリプトを起動し、検証後に各プロセスを終了する経路である。
preview サーバー、`dist/`、Playwright のブラウザ本体は必要ない。

対象スクリプトは `e2e/validate-fixtures.mjs` が手書きの一覧ではなくファイル内容
（`validateFixturesOrExit` の呼び出しの有無）から導出する。手書きだと、並行して
増えたスクリプトの契約検証が一覧への追加漏れで静かに検査対象から外れる。各スクリプトは
このモードで全フィクスチャを検証してから `launchBrowser` や bundle 検証へ進まない。
実ブラウザを使う判定はこのコマンドの対象外である。
CI では browser-e2e ジョブが 4 本だけ回し、残りはローカルの個別 E2E で行う（下記 §CI で回す 4 本とそれ以外）。
子プロセスは順番にすべて実行するので、先のスクリプトが失敗しても後続のフィクスチャ検証を
省略しない。

**`E2E_VALIDATE_FIXTURES_ONLY=1` をシェルに export したまま忘れると、以降の `pnpm e2e:*`
は全部「契約検証だけして exit 0」になる。実判定を 1 つも走らせていない緑になる。**この
コマンドを使うときはコマンドの前にだけ付ける。

### 多 site 番組表（`multi-site.mjs`）

`tokyo` と `takamatsu` に同一 `networkId` / `serviceId` / `Service.id` / 局名 /
`programId` の共有 BS fixture を配る。グリッドとリストで可視の site 名があり、
React key が衝突しないことを確認する。同時刻の容量超過は帯が各 site の列内に
収まり、時間軸列のラベル同士が重ならないことを実ブラウザの矩形で測る。
これは jsdom では列幅・横方向の配置を測れないため、実装より先に追加した判定である。
site 固定撤去前（フロントが単一 site 決め打ちだった頃）はこの判定を通すと列が
1 本で red（`描画された列: 1`）になり、site 和集合化後は green になった。
下記の GR fixture を足した現在、期待する列数は 6 本（`expectedColumnCount`。
共有 BS 1 局 + GR 2 局 × 2 site）。

**GR fixture（両 site で同じリモコン番号を持つが別放送の局。実在の東京・高松の
NHK 総合・NHK E テレと同じ形）も配る**。 `orderServices`（`lib/epg-grid.ts`）が
リモコン番号を site より先に比べる実装だと、この 4 局は 1 列ごとに site が交互
する順になる（レビュー指摘）。左端からの並びで「同じ site が連続する走」を数え、
各 site の走が種別の本数（GR 1 本 + BS 1 本 = 2 本）に収まっていることを実測する
（①）。GR 局を足すと site の列領域が非隣接な複数の走に分かれるため、`ProgramGrid`
は走ごとに `siteOverlay` を呼ぶ --- 対策前はその本数ぶん容量帯の読み上げ文
（sr-only）が重複する。帯（見た目）は走ごとに複数出てよいが、読み上げ文は
1 site につき 1 つだけであることも実測する（②）。

**列ヘッダーの site 名が `overflow-hidden` で視覚的に切れていないことも実測する
（①´）**。`allTextContents()` は切れていても文字列自体は取れてしまう。
そのため要素の矩形が列ヘッダーの矩形（`program-grid-header-cell`）に収まっているかを
`getBoundingClientRect()` で確認する。ヘッダーの高さを意図的に詰めた実装
（`headerHeightPx` を縮める）で実際に落ちることを確認済み。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:multi-site
```

### 参照バッジの導線（`badge-links.mjs`）

容量不足バッジ（予約一覧）から番組表への導線（issue #233 M6-5、`view` の URL 化は
issue #437）。見るのは主に 2 点 --- ①バッジが行本体の詳細リンクの中に入れ子の
`<a>` として置かれておらず、クリックすると番組表（`/programs?view=grid&at=...`）へ
飛ぶこと。②`lg` 以上ではリンクが積んだ `view=grid` どおりグリッド表示になり、
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
自動で確認する**（`verifyBundleMatches`）。不一致なら他の判定をせず即 exit 1 するので、
`curl`/`ls` で手動照合する必要はない。これは実際に踏んだ事故の再発を、人の確認忘れに
頼らず機械で止めるための仕組みである。その事故は `--strictPort` を付けていても
起きた --- 複数の worktree を並行して触っていると、別の worktree の preview が
同じポートに先に居座る。すると自分の起動が黙って失敗し、`E2E_URL` が無関係な古い
ビルドを指したまま判定が進んでしまう。ポート自体は空いているものを選ぶこと
（`--strictPort` が起動を失敗させるので、選び間違えればここで気付ける）。

### シリーズ一覧の分割（`shelves-split.mjs`）

**この判定だけは `/api/**` を差し替えない。** 確かめるのは次のことである。
「`/rules` で分類ルールを 1 本作ると、全件再評価の後に `/series` の棚が割れる」。
つまり API、worker の再評価ジョブ、SSE、棚の再取得という非同期の連鎖の実物である。
実バイナリ（`--roles api,worker,notifier`）と専用の DB が要る。SSE は notifier ロールが配るので、api と worker だけでは棚が再取得されない。
`worker.queues: [cleanup]` で足りる（再評価ジョブは cleanup キュー）。mirakc もチューナーも要らない。

判定は `E2E_DATABASE_URL` の DB を **TRUNCATE して**録画 12 件（数学 6 + 化学 6、自動キーはどちらも `NHK高校講座`）を作る。開発用 DB を指さないこと。
サーバーは起動時に `media_assets` の `rel_path` 名前空間を検査するので、先に同じ形の行を入れてから起動する。

作成と観測は別のページで行う。1 枚目は `/rules` でルールを作り、2 枚目は最初から `/series` を開いたまま操作しない。
読み込み前に通る空虚な成功を避けるため、作成前に 2 枚目で「1 棚 12 件」を見てから始め、作成後は割れるまでポーリングする。
2 枚目は作成のレスポンスで再取得されないので、割れたことを観測できるのは SSE 経由だけである（待ちは 30 秒で、60 秒周期の定期取得では通らない）。
値に空白を含めるとフォームが「この値は棚キー NHK高校講座 として扱われます」と注記することも見る。

確認した壊し方は 3 つある。
再評価ジョブの投入先を、worker が引かないキュー（ruler）へ戻すと「シリーズが割れる」が落ちる。
フォームの注記を消すと「食い違いの注記」が落ちる。
notifier ロールを外して起動すると「シリーズが割れる」が落ちる（割れるのが SSE 経由だけだから）。
実装前の画面（`/series` も `/rules` の分類ルール節も無いビルド）に向けると、作成前の「1 棚 12 件」が成立せず、`分類ルールを作成` ボタンも見つからずに落ちる。

```sh
E2E_URL=http://localhost:40799 \
  E2E_DATABASE_URL='postgres://localhost:5432/<サーバーと同じ DB>?sslmode=disable' \
  pnpm e2e:shelves-split
```

### ライブ視聴（`live.mjs`）

番組リストと違い、**mirakc も実チューナーも要らない** --- HLS プレイリスト/
セグメントは `page.route` でブラウザ側から丸ごと差し替える。動的 import
（hls.js のバンドル分割）・MSE への実再生・チャンネル切替時の cleanup は
jsdom で原理的に測れない。`vi.mock` によるフェイクの配線検査（Vitest）でも
「配線が呼ばれること」までしか見えない。手順は
[docs/runbook/live.md](../../docs/runbook/live.md) §②。

```sh
E2E_LIVE_SERVICE_A=9001 E2E_LIVE_SERVICE_B=9002 pnpm e2e:live
```

`E2E_LIVE_SERVICE_A` / `_B` に渡すのは **SI の `serviceId`** である。これは
`E2E_LIVE_NETWORK_ID` と組で、DB へ投入した行の `(network_id, service_id)` そのもの。
既定は `network_id=1`。
`/live` のページ URL は他画面と同じ `?service=<Service.id>`（合成 id）である。
スクリプトは起動時に `GET /api/sites/{site}/services` から対応する `Service.id` を
引く（`resolveServiceId`）--- 合成規則をスクリプト側に複製しない。セグメント/
プレイリスト/離脱ヒントの URL には従来どおり SI の `serviceId` がそのまま載る
（issue #217。streamer 側の資源同定は変えていない）。したがって env の投入例
（DB の `network_id` / `service_id` 列）も変わらない。`GET /api/capabilities` も
`page.route` で `{live: true}` に差し替えるので、サーバー側の `live.enabled` は
false（既定）のままでよい。差し替えないと画面が「無効です」になって
①〜⑦が全滅する（issue #209）。①〜⑦は「再生」ボタンを押した後の挙動を見るもの
なので、`page.goto` の直後に `clickPlay` でボタンを押す手順が入っている
（issue #234 M7-1。下記⓪参照）。

**⓪ 選択と視聴開始の分離（issue #234 M7-1）は ffmpeg フィクスチャに依存せず、
bundled Chromium だけで常に測れる。**チャンネルを開いた直後にプレイリスト/
セグメント要求が飛ばないこと、「再生」ボタンを押した後に初めて飛ぶことを
`page.route` の要求ログで観測する。「タップだけでは要求が飛ばない」ことは
jsdom では判定できないため、ここが唯一の判定手段になる。たとえば `fetch` を丸ごと
モックする Vitest のテストは mock 自体を呼ぶかどうかしか見られず、`<video src>` への
直接代入のように `fetch` を経由しない実ブラウザの資源取得は原理的に検出できない。
この判定を足す前の実装（チャンネルをタップした瞬間に probe する版）で実際に
落ちることを確認済み（詳細は issue #234 の実装 PR #259 の変異リスト）。

**この判定手段（①〜⑦）が実際に本番相当の回帰を 2 件発見した。**

1. `supportsNativeHls` が実 Chrome の `canPlayType` の戻り値 `'maybe'` を誤って
   ネイティブ対応と判定し、Chrome がサイレントに再生できなくなる
2. **その修正（`'probably'` のみを対応と見なす）がどの実ブラウザでも false に
   なり、Safari までが hls.js 経路に落ちる。**この回帰は①〜⑤（Chromium 系
   だけ）では検出できず、**e2e 緑のまま通った**。つまり「実ブラウザで測っている」
   ことは「壊れる側のブラウザで測っている」ことを意味しない。⑥（WebKit）を
   足して初めて機械判定できるようになった

詳細は [docs/runbook/live.md](../../docs/runbook/live.md)（実機確認の判定項目と回帰の記録）。

**⑨〜⑪ は画質まわりである**（画質の切替が #869、停滞時の自動降格が #871）。
⑨ は一覧が遅れて届いてもプレイリストを取り直さないことを見る。
切替で `<video>` を作り直さないこと（音量とミュートが保たれること）も見る。
切替で離脱ヒントを送らないことも見る。
⑩ は切替を跨いで字幕の表示状態が保たれることを見る。
⑪ は実再生させてから配信を止め、**1 段下がり、復旧後にそのまま再生が続く**ことを
両方向で見る（復旧は配信側が実際にセグメントを配ったことでも確かめる）。
**⑪ の閾値は実時間（12 秒）なので、この判定だけで 1 分半ほどかかる。**
時計を止めると意味が変わる種類の判定であり、`page.clock` を使わない
（末尾「判定を足すときの規律」の逆向きの例である）。
**⑪ は VOD 形のフィクスチャでは判定にならない** --- hls.js は VOD では 30 秒先読み
するので、セグメントを止めてもバッファを食い切るまで再生が進む。そこだけ
ライブ形のプレイリスト（`#EXT-X-ENDLIST` を付けず、載せる本数を絞る）を配る。

### 録画中の追っかけ再生（`chase.mjs`）

録画詳細を `#chase` で開き、実 H.264/AAC セグメントを返す成長中の EVENT
playlist を Chromium の hls.js（`E2E_BROWSER=webkit` ならネイティブ HLS）で再生する。録画 API・追っかけ HLS・離脱 API は
`page.route` で差し替えるため mirakc と実録画は要らない。次を実ブラウザで見る。

- 録画中の詳細ページから追っかけプレイヤーが開き、`<video>` に `controls` が無く、時間軸は操作バーの
  シークバー 1 本である
- EVENT playlist がセグメントを増やしても再生位置は録画先頭から始まる
- セッションの範囲内へのシークは playlist を取り直さない。ドラッグ中は張り直さない（要求回数で見る）
- 変換済みの端より先・開始 offset より前へのシークは、その秒の offset で張り直す。先頭セグメントが
  その秒のものであること、直前のセッションの leave ヒント（`/offset/{前の offset}/leave` まで）を見る
- **offset のセッションから 0 秒へ戻すと、offset 無しの playlist に張り直し、保存位置を復元しない**。
  パネルで 0 秒を「未選択」に潰す変異で、保存位置の 5 秒へ戻って落ちる
- 録画の先端の印と End キーで先端へ移る。範囲内か張り直しかは実装から予測しない。最後に要求した
  playlist の offset + `currentTime` が選んだ位置に落ち着くことを見る。予測すると、変換済みの端が
  読み込みに応じて伸びるため 10 回中 7 回落ちた
- 再生中に張り直しても再生を続け、選んだ位置から進む
- バー下の目盛り: 延長中は「予定 … ¦」の ¦ が予定終端の印を、録画済みが短いときは先端の時刻の中心が
  先端の印を指す（許容 3px）。印が左端に近いときはラベルが行の中に寄り、ラベル同士が重ならない。
  先端の印の下は時刻だけで、「録画の先端 …（押すと先端へ）」は印の title にある
- 保存済みの VOD 共通速度が追っかけの `<video>` に適用され、`ratechange` が同じ
  `localStorage` キーへ保存される
- 成長後の playlist が `ENDLIST` になり、完了した録画を追える
- **画質（プロファイル）の切替で再生位置が続く**（issue #874）。先頭再生と
  offset 付き再生の 2 通りを見る。**`LivePlayer` を `key={profile}` で作り直す変異で
  offset 付きが 1.5 秒 → 0 秒に巻き戻って落ちる**（先頭再生は保存位置が近いので
  差が出にくい）。あわせて切替が離脱ヒントを送らないこと・`?profile=` が要求に
  載ることも見る
- **画質の切替の途中で「続きから」を別の位置で上書きしない**。途中で書かれた値を
  すべて記録して見る（最終値は切替後の seek で正しい値に戻るため）。
  2 秒未満は `removeItem` になるので、それも 0 として記録する。
  持ち越し中の保存ガードを外すと、WebKit では先頭再生で `0`、offset 4 秒の切替で
  `4` が書かれて落ちる。Chromium + hls.js では `timeupdate` が来ないので落ちない
- **追っかけで見た位置から、完了後の VOD を開くと同じ位置 ± 3 秒から始まる**（⑧、
  issue #975 受け入れ 2）。追っかけの保存位置（スタブ API が保持）を読み、録画を完了に
  切り替えて `/recordings/1` を開き直し、原本 VOD の `currentTime` を比べる。
  **原本 VOD の `LivePlayer` に `resumePositionMs` を渡さない変異で
  `5.52 秒 → 0.00 秒` になって落ちる**（Chromium で確認。WebKit は未実施）
- **追っかけ範囲外 seek で原本 HLS に移った後も再生が続く**（⑩）。最後に進んだ `currentTime` から
  次に進んだ時刻までの最大停止時間を記録し、2 秒以下を判定する。再生を引き継がない変異は、範囲外 seek の
  `reselectPlaybackSource` へ `playing && false` を渡すものである。**停止 10073ms・`advances=0` になって落ちる**
  （Chromium で確認）。**再生元を替えない変異（`selected !== current.source`
  を `false &&` にする）で、offset を要求せず ⑩ の 4 判定が落ちる**（Chromium で確認）
- **追っかけの終端（`ended`）では、別の再生元を作らず終了状態のまま止まる**（⑪・⑫）。追っかけの
  `ENDLIST` は mirakc の録画が終わった後にだけ付くので、終端は常に録画ファイルの終端である
  （`docs/api/media.md`）。fixture は伸びる EVENT playlist を 6 segment（12 秒）で止め、そこへ
  `ENDLIST` を付ける。壁時計の録画時間は 20 秒、原本 HLS fixture は 20 秒以上ある。⑪ は完了の取得の後に、
  ⑫ は前に `ENDLIST` を付ける。**main の実装（壁時計の終端の 1.5 秒手前より前なら移る）で ⑪ が
  `sameVideo:false`・原本 HLS 要求 2 件になって落ちる**。**前回の実装（5 秒の許容と、完了の取得を待つ保留）
  では ⑪ と ⑫ の両方が同じ形で落ちる**（以上 Chromium で確認）。終端の 1 秒後の `ended` は判定に使わない。
  Playwright の WebKit は `ended` の約 0.9 秒後に `durationchange` だけを出して `currentTime` を 0 に戻すことがある
  （13 回の実行の 26 判定中 4 回。`seeking` / `loadstart` / `emptied` は出ない）。次のポスター判定は⑬

`E2E_BROWSER=webkit` で同じ判定を Safari 相当のネイティブ HLS 経路で回す。
画質切替の位置の持ち越しは hls.js（`startPosition`）とネイティブ（要素への代入）で
経路が別なので、両方で回す。

フィクスチャ生成に `ffmpeg` を使う。無い環境ではこの判定だけを skip として終了し、
他の CI 判定を失敗扱いにしない。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:chase
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:chase
```

### 録画原本 HLS と encoded の再生時刻（`recording-playback-timeline.mjs`）

Go テストで放送 TS に似せた MPEG-2 29.97 fps の fixture を作る。
HLS は製品の `BuildOriginalVODFFmpegArgs`（offset 0 / 10 秒）で生成する。
MP4 は製品の `BuildFFmpegArgs` と `config.example.yml` の h264 例で生成する。
入力は先頭 PTS が 0 でない。音声を映像より約 700 ms 先に置く。
不規則な間隔の9フレームに白い目印を入れ、位置で各フレームの ID を表す。
期待時刻は、チャプターの軸である非カット MP4 を ffprobe して得た目印フレームの PTS とする。
原本 TS の「目印 PTS - 最早 `start_time`」との差は参考値としてログにだけ出す。

Chrome の hls.js と WebKit のネイティブ HLS の両方で、原本 HLS offset 0、シークで
張り直した offset 10 秒、非カット MP4 を再生する。各目印の表示を画素で検出し、
その時点の `requestVideoFrameCallback` の `mediaTime` を読む。offset 付き HLS は
セッションの offset を足して原本時間軸へ戻し、フレーム番号の順序と半フレーム以内の
差を記録する。`E2E_TIMELINE_EXPECTED_SHIFT_FRAMES=1` を付けると期待値を 1 フレーム
ずらす変異確認になる。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-playback-timeline
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit E2E_TIMELINE_EXPECTED_SHIFT_FRAMES=1 pnpm e2e:recording-playback-timeline
```

この判定は ffmpeg / ffprobe / Go と Chromium / WebKit を使うため、これらが必要である。
非カット MP4 基準の測定（HLS offset 0 / 10、括弧内は参考の原本 PTS 基準）では、
Chrome の HLS が +66.73ms / +90.10ms（+56.71 / +80.08）だった。
WebKit の HLS は 0.00ms / +23.37ms（-10.02 / +13.34）だった。
半フレーム許容差 16.68ms を超えるため exit 1 になる。
非カット MP4 は両ブラウザで 0.00ms（-10.02）だった。1 フレーム変異は WebKit で 13 件の NG
になり、ずれを検出することを確認した。測定値と未測定の範囲は
[`docs/frontend/recordings.md`](../../docs/frontend/recordings.md) に記録している。

### 原本 HLS VOD の再生開始（`recording-original-vod.mjs`）

FFmpeg で MPEG-2 TS と H.264/AAC HLS の fixture を作り、録画 API と streamer の URL を
`page.route` で差し替える。mirakc と実録画は要らない。⑤-manual は自動再生を意図的に
`NotAllowedError` で拒否し、0 / 1 / 3 秒待ってから操作バーの ▶ を押す。fixture は EVENT
playlist の先頭 6 segment から始まり、待ち時間中に1秒ごとに segment を増やす。

⑦ は原本 HLS と encoded の両方について 1280px / 400px で再生開始と枠の寸法を測る。
クリック後 15 秒以内に video が一時停止状態を抜けて `currentTime > 0.5` になることを待つ。
原本 HLS は先頭 4 segment の EVENT playlist から始め、2 秒ごとに segment を追加して最大 8 segment まで配る。

原本 HLS は開始位置も測る。クリック後に最初に取得した映像 segment が `0_seg00000.ts` で、最初の
`playing` の `currentTime` が 1 未満でなければ NG にする。`currentTime > 0.5` だけでは、最新端から
始まって進む再生も通ってしまうためである。診断には `playing` / `seeking` と `currentTime` への代入、
詳細 API が返した再開位置を載せる。

前のページの再開位置 PUT は遷移の後に届くことがある。⑦ と ④ は書き込みの反映を止めて（`applyPositionWrites`）
から `resumePositionMs` を消す。止めないと製品は残った位置から再開し、8 秒分の fixture の端で止まる。

⑦が失敗した場合は待機エラー、`paused`・`currentTime`・`readyState`・`seekable`・`buffered` と
メディアエラーを記録する。`play()` の呼び出し・成否、playlist / segment の要求と HTTP 応答も記録する。
再生中の操作バーは 3 秒で隠れ `aria-hidden` / `inert` になる。手動でバーの操作を調べるときは、
枠の上でマウスを動かして表示を待つ。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:recording-original-vod
```

### 原本 HLS から encoded への切替（`recording-original-vod.mjs`）

⑥ は原本 HLS 再生中に encoded が追加されても現在の HLS を保ち、次の範囲外 seek で encoded MP4 へ
位置を渡す。切替後に `currentTime` が進むことと、最後に進んだ時刻からの最大停止時間が 2 秒以下であることも、
Chromium と WebKit で確認する。再生を引き継がない変異は、⑩ と同じ
`playing && false` である。**切替後が `paused:true`・停止 10077ms・`advances=0` になって落ちる**（Chromium で確認）。

切替の停止時間（上限 2000ms）の実測は、Chromium で ⑩ 88〜177ms（5 回）・⑥ 113〜165ms（3 回）、
WebKit で ⑩ 202〜281ms（13 回）・⑥ 241〜310ms（3 回）だった。

```sh
E2E_URL=http://localhost:4173 E2E_BROWSER=chromium pnpm e2e:recording-original-vod
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-original-vod
```

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

**フィクスチャは契約で検証される。**「唯一の視覚オラクル」が欠損データのまま
撮れていても、契約（`openapi.yaml`）が動くたびに誰も気付かない、という壊れ方が
実際にあった。ルールの `textMatches` が旧形 `{ field, kind }` のまま
`{ target, mode }` に追従しておらず、ルール一覧に「undefinedに…を含む」が
描かれたまま exit 0 していた。判定本体は `validateFixturesOrExit`
（`e2e/lib.mjs`）である。これは `verifyBundleMatchesOrExit` と同じ**前提条件**チェックで、
スクリプト固有の OK/NG 判定ではないため、共有ハーネス側に置いてある。フィクスチャを
orval 生成の zod スキーマ（`web/src/api/zod.ts` の `List*ResponseItem`）で
`parse` し、1 件でも不一致なら他の判定を一切せず exit 1 する。これは ⓪ の
`verifyBundleMatchesOrExit` と同じ「前提が崩れていたら打ち切る」扱いである。
`design.mjs` に加えて `badge-links.mjs` / `sse-refresh.mjs` /
`grid-reserved.mjs` / `reservations-mobile.mjs` も自分のフィクスチャで呼ぶ。
**契約を変えたら、フィクスチャを持つ各スクリプトも同じ PR で直す。**

`zod.ts` は `import.meta.env` 等 Vite 依存を持たない素の TypeScript なので、
Node の ESM スクリプトから `../src/api/zod.ts` を直接 import できる。
追加のローダー（tsx・vite-node）は要らない。Node は型注釈だけを消す型
ストリッピングを既定で持つ（`.node-version` の 24.20.0 で実際に import
できることを確認済み）。`zod.ts` は enum・namespace・parameter properties の
ような変換が要る構文を持たないため、これだけで通る。`package.json` の
`e2e:design` 等の各スクリプトもそのままで変更していない。

`design.mjs` は画面のテキストに欠損文字列が混ざっていないかも全ショットに
掛ける ---「安いので全画面に掛ける」。見るのは `undefined` / `NaN`
（単語境界 `\b`）と `[object`（前方一致）。**`null` は対象にしない** ---
番組名・ルール名に偶然「null」を含む文字列が来ると単語境界だけでは区別できず
偽陽性になりうるため。

出るもの:

- `e2e/screenshots/*.png`（追跡しない）。主要 8 画面（ホーム・シリーズ一覧・番組ハブを含む）×
  ライト / ダーク × デスクトップ / モバイル。加えて番組表グリッド・
  サーキットブレーカー発動中・モバイルの「その他」を開いた状態・読み込み中
  （Skeleton の走査線を撮るため録画一覧の応答を遅延させたもの）を撮る。
  空状態（EmptyState の走査線。既定のショットでは折り返しの下に隠れて
  文字が写らないので、スクロールしてから撮る）とホームの全セクション空状態
  （`home-empty-*`）を足す。
  さらにシリーズ一覧の格子とリスト（`series-card-*` / `series-list-*`）を 360px と 2560px で撮る。
  番組ハブは 400px とデスクトップ幅で配置とメニューを撮る（`series-hub-400-*` / `series-hub-desktop-*`）。
  **人が見て判断するための成果物**で、機械が比較するものではない
- 合否（exit code）。以下をすべて実画素・実描画で判定する:
  - 状態色（塗りか文字か / 赤か琥珀か）・地の無彩性・**WCAG コントラスト**
    （文字は 4.5、面と線は 3 が下限）。`bg-muted` 系の淡い面に乗る文字は、
    塗り・`/80` の sticky 日付見出し・`/30` の録画詳細パネルに加えて
    **hover 中の面**（一覧の行の `hover:bg-muted/40`）まで測る --- Lighthouse は
    hover を測らないので、監査に出ない面はここでしか押さえられない
    （下限を割ったものは除外せず、通常の失敗として扱う）。選択モードで
    選んだ行も同じ `bg-muted/40` を敷くが、こちらは常時見えるので測る
  - **和文が実際に Noto Sans JP、英数字が実際に Geist で描画されているか**を
    見る。CDP `CSS.getPlatformFontsForNode` に番組リストの行
    （`li[data-program-id]`）を渡し、その実使用フォントを読む。
    `main` や `body` のようなブロック要素だけを子に持つノードを渡すと
    常に空配列が返るため使えない。`getComputedStyle().fontFamily` は
    指定文字列を返すだけで実描画の保証にはならない。
    あわせて**和文まじりの文字列でも tabular-nums が実際に等幅を
    作っているか**を DOM の実測幅で見る（`docs/frontend/stack.md`
    「フォントは英数字と和文で 2 書体を使い分ける」）
- シリーズ一覧（`/series`）の格子とリスト（①-B）。360px のモバイルと 2560px のデスクトップで実測する。
  格子が 2 列 / 4 列・リストが 1 列になること、サムネイルが 16:9（誤差 0.01）であること、横にはみ出さないこと。
  画像が無いときの代替表示が `bg-muted` の塗りで走査線でないこと、メタが 14px であること。
  サムネイルを 16:9 でなくすると（`h-16 w-28` に戻す）リストの 2 判定が落ちることを確認した。
- 色以外にも、jsdom では原理的に測れないキーボード到達性を 1 件持つ。
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
- `④-A` の操作標的計測では、fine のデスクトップと coarse の 360px モバイルを使う。
  主要 6 画面の
  `button, a[href], [role="button"], [role="switch"], input, select, summary` を
  実際の描画矩形から列挙する。各標的の visual / hit 寸法、最小エッジ間隔、
  意図的な重なり件数を出力し、
  実効 hit 寸法が 24×24 CSS px 未満なら exit 1 にする。`::before` / `::after` による
  当たり判定の拡張と祖先の overflow によるクリップを含めるので、クラス名だけの
  推測にならない。未フォーカスの sr-only スキップリンクは既存の Tab 後計測で扱う。
  日付セル・チャンネル候補・行主操作・ライブチャンネルは別途 44px 高、モバイルナビは
  幅44px・高さ56pxを、#729 のトースト action は 32px、close は 28px を個別に
  固定する。全面リンク等の意図的な重なりがあるため、間隔に全画面一律の閾値は置かず、
  配置の意味は `docs/frontend/reservations.md` に従う
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
- **下限を割るものは通常の失敗にする。** 既知の不足を理由に合否から外したり、
  閾値を静かに下げたりしない

ダークは Playwright context の `colorScheme` で OS 設定をエミュレートする。
`design.mjs` はクラスを直接付けず、アプリが初回描画前に `html.dark` へ同期する到達経路を判定する。

**`getComputedStyle()` の戻り値を正規表現で読んではいけない**。
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
実際に起きるか**を、リクエスト数を数えて判定する。周期と対象は運用状態 60 秒 /
ストレージ残高 5 分 / EPG 10 分で、[docs/api/sse.md](../../docs/api/sse.md)
§レベルトリガーの対称性に書いてある。時計は `page.clock.runFor` で進めるので 10 分待たない。
`design.mjs` と同じく `/api/**` を丸ごと差し替えるため、mirakc も DB も Go サーバーも
要らない。

**接続断バナー（`components/connection-banner.tsx`、issue #456）もここで見る**。
`/api/events` を `page.route` で abort し、`disconnectedBannerDelayMs`（10 秒。
`page.clock` の仮想時計で進める）後に帯が出る。そこから route を復旧させ、
帯が消えるまでを実ブラウザで確認する。復旧の再接続はブラウザ実装側のタイマー
（実時間、`page.clock` は進めない）に依るため実時間でポーリングする。加えて、
帯と `CircuitBreakerBanner`（`/api/breakers` を 1 件返して同時に出す）が
`PageHeader` と `getBoundingClientRect` で交差しないことも見る。スクロールして
両方を sticky の「張り付いた」状態にしてから測る。未スクロールでは top のずらしを
外しても重ならずに通ってしまうためだ。

**単体テスト（`src/lib/events.test.tsx`）と重なっていない部分がここの存在理由。**
単体テストはフックとテスト用のクエリキーしか通らないので、**画面が実際に使っている
キーを取りこぼしている**という壊れ方を検出できない。実際、`epg` トピックが番組リスト
（`useInfiniteQuery` の手書きキー `['/api/programs', 'infinite', ...]`）に一度も
届いていなかった。これを見つけたのはこの判定である（詳細は docs/api/sse.md §レベルトリガーの対称性）。
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

### 番組リストの操作列（`reserve-visibility.mjs`）

番組リストの予約 / 取消 / ライブボタンを含む操作列を「ホバー / フォーカスした行・
展開中の行」だけ立てる（issue #310 / #755）。判断は
[docs/frontend/reservations.md](../../docs/frontend/reservations.md) §番組リストの操作列は
ホバー / フォーカスした行だけ立てるに従う。**この開閉は
`:hover` / `:focus-visible` / `pointer:` メディア特性で駆動するので jsdom では
原理的に測れない**。jsdom はレイアウトを持たず、
`getBoundingClientRect().width` は常に 0 になるためだ。`pnpm test` は「常時開いたまま」
というクラス名の変異を検出できない。単体側
（`program-row.test.tsx`）が見るのは `group` / `peer` マーカーと `data-testid` の
配線だけで、可視性そのものはここが唯一の判定手段。

見るのは 4 状態である（すべて操作列の実描画幅 `getBoundingClientRect().width` を
直接読む）。畳は約 0px、通常行の開は 81px、放送中行はライブボタン分を足した
125px である。どちらも `box-content` でボタン合計幅を content box として確保した
上に `border-l` の 1px が外側に乗った外寸:

- ① 細ポインタ（既定の Chromium = hover:hover + pointer:fine）: ホバーも
  フォーカスもしていない通常行と放送中行は畳む。ホバー・`:focus-visible` で
  それぞれ 81px / 125px まで開く（両方向）。あわせてホバー前後で行の
  高さが変わらない（CLS 無し）こと、開いている列の中でボタンが
  `overflow-hidden` に切られていない（`scrollWidth <= clientWidth`）こと、
  開いている通常行でのワンタップ予約が実際に `PUT .../intent` を飛ばすことを測る
- ② 細ポインタで展開すると、行ヘッダから hover / focus が外れても見えたまま
  （展開パネルは `.group` の外の兄弟なので `peer-aria-expanded` を pointer 種別で
  縛らないことで担保）。通常行の展開幅が 81px のまま、折りたたみ直すと消える
- ③ タッチ / 粗いポインタ（hasTouch + isMobile = hover:none + pointer:coarse）:
  通常行と放送中行の折りたたみ行は畳み、展開行はそれぞれ 81px / 125px
  まで開く。加えて外付けキーボード想定で `:focus-visible` だけでも開く
  （WCAG 2.4.7 / 2.4.11）
- ④ 折りたたみ行の操作列を実座標へ `page.touchscreen.tap()` で生タップ
  （ロケータのアクショナビリティ判定を迂回する）しても PUT が飛ばない・
  トーストも出ない。**これがレビューで見つかった欠陥そのもの**である。`opacity-0`
  では見えない 80×56px がヒットテストに残って予約が成立していた。幅 0 +
  `overflow-hidden` は列そのものを畳むので飛ばない。放送中行は操作列に
  遷移する `<a>`（ライブボタン）が入っているため、同じ生タップの後も
  URL が `/programs` のままで `/live` へ遷移していないことも測る

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）で mirakc も DB も要らない。⓪（配っている
bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reserve-visibility
```

### 番組表グリッドの空間ナビゲーション（`grid-navigation.mjs`）

番組表のセルにフォーカスを置いたとき、矢印キーで空間的に移動できることを実ブラウザ
で確認する。`role="region"` とセルの Tab 停止点は維持したまま、次の 2 点を見る:

- ① `ArrowRight` で、フォーカス中の番組の開始時刻を含む隣列の番組へ移る
- ② 仮想化で最初は DOM に無い同列の遠い番組へ `ArrowDown` で移り、スクロール後に
  そのセルへフォーカスが移る

jsdom ではレイアウト・スクロール位置・仮想化後のフォーカスを同時に測れない。API は
`page.route` で差し替え、固定時刻と 2 列の番組だけを使うので mirakc も DB も要らない。
フィクスチャは `validateFixturesOrExit` で生成スキーマと一致することも確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:grid-navigation
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
- ③ 未予約の `skip` は、セルの高さが 24px 以上なら「スキップ中」バッジ、24px 未満なら
  高さいっぱいの状態マーカーだけを表示する。短いセルに切れる文字バッジを描かない
- ④ 予約済み（未選択）と選択中（未予約）は別の形。予約済みだけに見える
  「予約」がある
- ⑤ 印の色はタリー / 琥珀 / destructive ではない（色は信号のみ）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:grid-reserved
```

### 番組表セルの操作モーダル（`programs-dialog.mjs`）

番組表のセルをクリックしたとき、選択した番組が番組表の最上部へ移動するのではなく
ダイアログで開くことを実ブラウザで見る。jsdom では CSS による可視性、フォーカストラップ、
overlay / Escape による閉鎖後のフォーカス復帰、スクロール中の要素の可視性を測れないため、
次の 6 点を判定する:

- ① セルのクリックで番組名を `aria-labelledby` に持つダイアログが開く
- ② ダイアログ内に `ProgramRow` をマウントせず、予約パネルの「予約」が hover なしで
  可視・44px 以上・操作可能で、1 回のクリックで `PUT .../intent` が 1 回だけ飛ぶ。
  操作列は要約行の右端に揃い、通常 81px / 放送中 125px の実幅になる。通常 / 放送中の
  2 状態のダイアログは `e2e/screenshots/program-dialog-*.png` に保存する
- ③ 予約ボタンと閉じるボタンが重ならず、番組概要が長くて本文がスクロールしても、
  閉じるボタンが画面外へ出ない
- ④ Tab 走査がダイアログの外へ出ない
- ⑤ Escape で閉じ、クリック元セルへフォーカスが戻る
- ⑥ overlay と「閉じる」ボタンでも閉じ、クリック元セルへフォーカスが戻る

**フォーカス復帰は base-ui の `Dialog.Popup` の既定（`finalFocus` 省略時の
「trigger or previously focused element」）に任せている。**controlled Dialog
（`Dialog.Trigger` が無い）でも既定はクリック直前のフォーカス要素へ戻るため、
このページ側で `finalFocus` を自作する必要は無い。自作すると、ボタンの
クリックで focus を移さない macOS/iOS Safari で `document.body` を
掴んでしまい、既定より悪化する。

API は `page.route` で丸ごと差し替えるので mirakc・実チューナー・DB は要らない。
⓪（配っている bundle と `dist/` の一致）とフィクスチャの zod 契約検証も行う。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-dialog
```

### ボトムタブの高さと本文の下パディング（`programs-bottom-nav.mjs`）

`--bottom-nav-height`（`web/src/index.css`）がボトムタブの実際の描画高さと一致して
いるかを 390×844 の実ブラウザで見る。jsdom は `getBoundingClientRect` を計算しない
（常に 0 を返す）ので、ここで見る値はどれもユニットテストでは原理的に取れない。

**タブは `fixed` のオーバーレイのままとし、ここで保証するのは到達可能性だけである**。
ページ全体スクロール + `fixed` なタブでは、途中のスクロール位置で行がタブの裏に
入ることを仕様として受け入れる。重なり量の観測・判定は行わない（
`docs/frontend/scroll.md`「ボトムタブの裏に隠れる行」）。

このスクリプトが見ているのは別の欠陥である。`--bottom-nav-height` から nav の
上辺の境界線ぶん（1px）が落ちており、`main` の `padding-bottom`（64px）がタブの
実寸（65px）に足りていなかった。**この状態でも最下端での余白はちょうど 0px で、
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

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-bottom-nav
```

### 番組リストの空時間窓（`programs-empty-window.mjs`）

番組 API が最初または途中の 6 時間窓を空で返しても、後続窓へ進む導線が消えない
ことを 390px 幅の Chromium で見る。空窓を 2 回進め、後続取得の失敗後に同じ窓を
手動再試行して番組へ到達すること、番組表示後の空窓で自動取得が連鎖しないこと、
最終窓ではボタンと追加要求が消えることを判定する。通常の番組あり窓の日付移動と
自動読み込みは `checks.mjs` が引き続き判定する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-empty-window
```

### 検索フォームの初画面と詳細条件（`search-mobile.mjs`）

`/search` は条件フォームの大半をチップ列（サービス・チャンネル種別・ジャンル・
時間帯）が占める（issue #305）。issue #685 では、キーワード入力を先に使えることと、
詳細条件を必要なときだけ編集できることを両立させる。レイアウト・可視性・スクロールは
jsdom の `getBoundingClientRect()`（常に 0 を返す）では測れないため、360/390px の
Chromium で次を確認する。

①②は `page.goto` 直後に**一切スクロールも操作もせず**測る:

- ① 「検索」ボタンの矩形がビューポート内に収まり、モバイルのボトムタブと
  重なっていないこと
- ② テキスト条件 1 行目の対象とモードが同じ行にあり、値入力がその下の専用行で
  フォーム幅をほぼ使うこと。「条件を追加」を押さずに直接入力できることも確認する。
  360px では実測 302px（フォーム内 328px）、390px では 332px（同 358px）であり、
  旧一行レイアウト相当の実ブラウザ測定（360px: 62px、390px: 92px）から大きく改善
  している。対象・モード・値を `w-28 shrink-0` の一行へ戻す変異は失敗する。

③は URL (`cond`) からジャンルとチャンネルを復元して測る:

- 詳細条件が初期状態で閉じ、「設定中の詳細条件: 2件」と種別ごとの要約が見えること
- 詳細条件の表示ボタンをキーボードの Enter で開き、日本語の入力を編集できること
- ふたたび閉じ、要約の解除ボタンでジャンルだけを下書きから外せること

④だけは定義上「押した後」なので、①②を測り終えてから操作する:

- ④ テキスト条件に打って「検索」を押した後、件数行（`N 件`）が
  折り目の中にあり、`sticky` なページヘッダの下にも潜っておらず、結果の 1 件目も
  折り目の中にあること。変更前も結果には届いたが、実測（390px）は
  `scrollY=1138` まで必要だった（1280px では 948）。変更後は 390px で
  `scrollY=502`（1280px では 426）、件数行 y=49、1 件目 y=81 になった。
  主操作を上端へ動かすだけでは総スクロール量は変わらず、
  「押しても画面が変わらず、
  結果を見るために下までスクロールする」状態が①②とも OK のまま成立するため、
  送信後に結果の先頭へスクロール・フォーカスする。
- ⑥ 長いサービス名と有料番組の `ProgramRow` で、メタ行が 1 行に収まり、行が不要に
  2 行ぶん高くならないこと。メタ行・行の高さは固定値で判定する（実測は
  360/390/1280px いずれも `ProgramRow` 65px・メタ行 20px、しきい値は行 72px・
  メタ行 24px）。`flex-wrap` を戻した変異、`py-2.5`→`py-5` の変異、名前カラムに
  2 行目を足す変異のいずれでも実際に落ちる。ただし `flex-wrap` を捕まえるのは
  computed style の判定であって高さではない（`ProgramRow` のサービス名は
  `truncate` なので、折り返しを許してもメタ行は 20px のまま）。

1280px では対象・モード・値の top がほぼ一致すること（= 同じ行にある）を見て
従来のデスクトップの一行レイアウトを確認し、④と⑥も同じ条件で実行する。`sm:flex-row` を
落として一行を崩す変異で実際に落ちることを確認済み。
`page.route` のスタブは mirakc も DB も使わず、検索 API と番組詳細の 2 本を差し替える。

**ボトムタブは `nav[aria-label="主ナビゲーション"].fixed` で指す。**この
`aria-label` の `<nav>` はサイドバーとボトムタブの 2 本あり、`.last()` で当てると
`AppShell` の DOM 順に依存する。順が入れ替わると 390px でも `hidden md:flex` の
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
組み合わせがシェブロンに重なる。折り返し・
overflow・要素間の重なりは jsdom（`getBoundingClientRect()` が常に 0 を返す）
では原理的に測れない。既存の単体テスト（`pages/reservations.test.tsx`）は
「局名の文字列が行の中に居る」ことしか見ておらず、この壊れ方を検出できない。

見るのは 360px 幅（レビューの実測条件）で:

- ① 副情報コンテナ（`[data-testid="reservation-secondary"]`）が横方向に
  オーバーフローしていない（`scrollWidth <= clientWidth`）
- ② 副情報の各子要素の右端がシェブロン（`[data-testid="reservation-chevron"]`）
  の左端を超えていない。①はコンテナが中身を外に漏らしていないことしか見ないので、
  コンテナの外形自体が食い込む場合を捕まえるにはこちらが要る
- ③ ページ全体が横スクロールしない（退行の網。単体では①②の代わりにならない）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も
DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

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

### 固定ヘッダーとページ内 z-10 要素の重なり（`header-stacking.mjs`）

録画詳細のポスター上の「視聴済みにする」は `absolute z-10` を持つ。祖先に stacking context が無いので、
`PageHeader` が同じ `z-10` だと DOM 順で後ろのボタンがヘッダーの手前に描かれる。録画詳細を 400px / 1280px 幅で
スクロールし、ボタンの矩形をヘッダーの中央に重ねた位置で `document.elementFromPoint` がヘッダー内を返すことを
実ブラウザで確認する。ボタンの祖先に stacking context が無いこと、判定点がボタン矩形に入っていること、
ブレーカー帯とヘッダーが重ならないことは前提として検査し、成立しなければ落とす。DOM は書き換えない。
録画一覧も同じ幅でスクロールし、ヘッダーが残ることと録画中の行に追っかけリンクが無いことを見る。
jsdom は描画順と hit testing を測れない。

API をスタブするため mirakc と DB は要らない。①は `PageHeader` を `z-10` に戻すと落ちる。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:header-stacking
```

### 予約一覧の日付・出自・ルール絞り込み（`reservations-rule-filter.mjs`）

予約一覧の時間順ビューで、ローカル日付の見出しがスクロール後もページヘッダーの下に留まること、ルール出自リンクが行全面リンクより手前でクリックできることを測る。360 / 390 / 1280px で日付見出しと出自リンクを確認し、同じ幅でルールメニューが画面内に収まり、各項目が 24 × 24px 以上であることを確認する。360px ではページの横スクロールが無いことも判定する。7px 刻みでスクロールし、日付見出しとヘッダーのチップの中心が行の中身に遮られないこと、時刻欄のクリックで詳細へ遷移することも測る。light / dark・各幅のスクリーンショット（時刻順・スクロール後・ルールメニュー・ルール絞り込み）を `E2E_SHOT_DIR` へ保存する。

`/api/**` をブラウザー側でスタブするので mirakc・DB は要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/reservation-rule-shots \
  pnpm e2e:reservations-rule-filter
```

### 予約一覧の容量確認失敗（`reservations-capacity-error.mjs`）

予約一覧の容量超過 API が失敗すると、失敗を空配列として扱った「確認が要る予約は
ありません」を表示してしまうことがある。実ブラウザで API の全リトライを失敗させ、
容量を確認できない理由と再試行を表示すること、attention フィルタの空状態を確定しない
こと、同じ予約に正常な容量応答を返したときバッジと要確認チップが復旧することを見る。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:reservations-capacity-error
```

### 予約一覧取得失敗時の番組表レイアウト（`programs-reservation-error.mjs`）

`pages/programs.tsx` のグリッドの高さ予算は次の式で決まる。
`height: calc(100dvh - var(--page-header-height, 0px) - var(--sticky-banners-height, 0px))`
である。`reservations.isPending` / `isError` のバナーを
`PageHeader` の**外**（通常フローの兄弟）に置くと、バナーの高さがどちらの
CSS 変数にも入らない。すると 100dvh で組んだ画面なのに文書がビューポートを超え、
ページ全体がスクロールする（実測: 外に置くと 1440x900 で 949px / 900px、
`PageHeader` の中なら 900px / 900px）。この壊れ方はレイアウトそのものなので
jsdom（`pnpm test`）では原理的に検出できない。

`pages/programs.tsx` のコメントが挙げる「グリッドの sticky ヘッダが画面外へ
出る」という症状そのものは、**この構造では再現しなかった**。外に置いた
状態で文書を最後までスクロールしても、グリッド内の `GenreLegend`（見出し行の
上にある帯）が緩衝になって 39px の余裕が残るためだ。下の判定 B はその症状の再現では
なく、緩衝が無くなったときに気付くための網である。

`GET /api/reservations` だけを常に 500 で返し、次を見る:

- デスクトップ（1440x900、`/programs?view=grid`）
  - A. `document.documentElement.scrollHeight` が `window.innerHeight` を
    超えない（文書がはみ出さない）
  - B. グリッドのサービス列見出し（`program-grid-header-cell`）の上端が
    `PageHeader` の下端より下にあり、ビューポート内に見えている
  - C. グリッドのセルを選ぶと出る選択済み番組の行の「予約」ボタンが
    disabled であること。予約状態が不明なまま record intent を送らないことの
    実ブラウザ確認である。表示形式ごとに boolean prop を渡す実装ではグリッドだけが
    渡し忘れており、実際に `PUT .../intent` が飛ぶことを測った
- モバイル（390x844、`/programs` リスト）
  - D. 失敗バナーを飲み込んだ `<header>` の実測高さ（`offsetHeight`）。
    合否ではなく測定値（sticky に入れた代償の定量化）。緩い上限
    （viewport 高さの半分未満）だけ置く。実測は平常 121px / 1 行の帯 170px
    （`ErrorState` を使っていた案では 281px）
  - E. その状態でも番組リストの先頭行がヘッダの下に見えていること

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は
`page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）
も自分で確認する。

**`pnpm e2e`（`checks.mjs`）には足さない。** `checks.mjs` は判定を集約する
ハブではなく、番組リストの日付ジャンプだけを見る独立したスクリプト
（既存の他スクリプトも `pnpm e2e` からは呼ばれていない）。したがって
このスクリプトも他と同じく独立した `package.json` スクリプトのままにする。

```sh
pnpm build && go build -o /tmp/rokuban ../cmd/rokuban && /tmp/rokuban server --roles api --config ../dev.local.yml &
pnpm e2e:programs-reservation-error
```

### 読み込み中のレイアウトシフト（`cls.mjs`）

CLS（Cumulative Layout Shift）はレイアウトそのものの指標なので、jsdom
（`getBoundingClientRect()` が常に 0 を返す）では原理的に測れない。
Lighthouse で検索に要改善域の CLS が出たことの唯一の判定手段になる。

ブラウザの Layout Instability API（`PerformanceObserver({type: 'layout-shift'})`）で
`hadRecentInput === false` の `value` を単純合計する。**これは Lighthouse が実際に
報告する CLS の近似であって同一ではない**。Lighthouse は session window で
グルーピングしてその最大値を採るが、ここでは windowing をせず全期間の単純合計を
見る。単純合計は session window の最大値より大きくなることしかないので、ここで
0.10 以下なら Lighthouse の値も 0.10 以下になる。逆方向の保証はしない、未検証。

見るのは検索（`/search`。`components/condition-fields.tsx`）の 2 点:

- ① モバイル幅（390x844）でサービス一覧の取得を遅延させた状態（Lighthouse の
  スロットル下を模す）で読み込み中の CLS が 0.10 以下
- ② デスクトップ幅（1280x900）で①と同じ遅延を掛けた状態で 0.10 以下。ラボ計測は
  検索デスクトップも 0.087（しきい値の一歩手前）を報告しており、`ConditionFields` の
  対策の根拠はビューポート依存の議論（「押される側が折り目の外に出る」）なので、
  モバイルだけでは踏んでいない

検索画面の詳細条件は初期状態で閉じているが、`cls.mjs` は開閉ボタンを押してから
サービス一覧を待つ。これで、サービス節が表示中の詳細フォームを非同期に押す従来の
シナリオを保ったまま、初期表示を短くした変更の影響も測れる。

サービス数は 24 局（地上波 + BS + CS 相当）にしてある --- 2 局だけでは 390px でも
チップが 1 行に収まってしまい、直す前の実装でも再現しない。

**サービス一覧の遅延（`NETWORK_DELAY_MS`）は 500ms より確実に大きくすること**。
Chrome はクリック等の離散入力から 500ms 以内の layout-shift を全部
`hadRecentInput: true` にし、`installClsObserver` はそれを合計から除く。
`measureSearch` は「詳細条件を表示」をクリックしてから遅延後にサービス一覧が届く。
そのため遅延が 500ms 未満だと、届いたときのシフトが毎回この入力窓に収まって
除外され、残留ノイズしか測れなくなる。レビュー指摘では、旧 400ms で①の実測が
0.00066 → 0.000008 まで落ち、下記の変異検出が効かなくなっていた。いまは
1500ms にしてある。直す前の
`condition-fields.tsx`（`ServiceFields` が `TextMatchFields` の直後）を当てると
①が 0.257（②は 0.039）で、①が実際に落ちることを確認済み。

**issue #685 の詳細節を開いた現在の実測は①が 0.024、②が 0.014。**
`ServiceFields` の位置と非同期／同期の境界（上記）は変えていないため、しきい値
0.10 に対して十分小さいまま。

**検索条件にサイトチップ（`SiteFields`）を足した（issue #531）後も測り直したが
値は変わらない（今回の実測では①が 0.024、②が 0.014）。**この `SiteFields` は
レジストリの解決した `GET /api/sites` のキャッシュを再利用する同期的な節である。
しかも、レジストリと下書きの和集合が 2 つ以上のときしか描画しない。この
フィクスチャは単一サイトかつ下書きが空なので DOM に一切増えない。

**ホーム（`/`）はここでは見ない。**ラボ計測はホームのデスクトップでも 0.111
（スロットル時のみ）を報告している。原因は「4 セクションの表示順は固定なのに解決順は
不定で、後続セクションが先に見えている状態で先行セクションが実データごと上に挿し込まれる」
形である。対策には `docs/frontend/home.md`「セクションの可視性は個別に」を変える
設計判断が要る。判定手段（この形のフィクスチャ）ごとその判断のあとに足す --- しきい値を超えたまま
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
  を外すと落ちる**（実測: 有り 320 / 320、無し 462 / 320）
- ② 解き方が「チップの中で折り返す」であること（切り落としでも隠しでもない）。
  チップの箱がビューポートに収まり、箱の中で内容があふれておらず、実際に 2 行に
  なっている。`max-w-full` を外すと①と一緒に落ちる
- ③ `Chip` は共有プリミティブなので、録画一覧の絞り込みにある短いピル
  （状態 5 件 / 種別 3 件 / ジャンル 16 件）が丸ピルのまま 1 行であること。
  ①の対策が他画面の見た目を変えていないことを逆方向から見る。ポップオーバーが
  ビューポートに収まることも合わせて測る。同じ③で録画一覧のサイトチップも見る
  ---レジストリに無い site を `?site=` に載せて開き、和集合で増えたチップが
  320px に収まることを確認する

**レジストリは 2 サイトにしてある（issue #531）。**`<ConditionFields>` の
サイトチップはレジストリと下書きの和集合が 2 つ以上のときしか描画しない。
そのため単一サイトかつ下書きが空のスタブでは、判定対象自体が存在しなかった。
片方を長い site 名にして①のサービスチップと同じ `Chip` を通し、
「サイト」のチップ列がちょうど 2 件描かれることと、①の横スクロール判定に
一緒に含めている。

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
- ⑤ 録画詳細で実 `<video>.playbackRate` を 1.5 に変え、標準 controls と同じ
  `ratechange` 経路で保存されること、一覧を経由して別の録画の詳細へ移っても
  実 `<video>.playbackRate` が 1.5 のままなこと。**測っているのは
  「速度変更イベントが端末に保存され、録画をまたいで実 `<video>` に載る」こと**で、
  `savePlaybackRate` を no-op にする変異では保存待ちが失敗し、保存値の適用を外す変異では
  次の録画の `<video>.playbackRate` 判定が失敗する。
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

### シークバーのプレビュー（`seek-tiles.mjs`）

動画の下のスクラブ帯でポインタを動かしたとき、その位置に対応するタイル画像が出ることを実
ブラウザで見る。位置は「ポインタの x → 帯の中の割合 → 再生位置 → タイルの格子位置」で
決まるが、**jsdom の `getBoundingClientRect()` は常に 0 を返すので、この経路は単体テスト
では 1 歩も進まない**。`recording-player.test.tsx` が見るのは「タイルの問い合わせが
始まること」「タッチでは始めないこと」だけで、位置の正しさはここが唯一の判定手段になる。

見るのは 9 点:

- ① ホバー位置に対応するタイルが出る。**列の折り返し（タイル 3 → 9）と行送り
  （タイル 10）を別々の位置で固定する** --- 3 だけだと列数を 10 から 5 にしても
  同じ答えになる（実測で確認済み）
- ② プレビューが帯の幅に収まり、帯そのものを覆わない
- ③ ポインタが帯から離れると消え、映像の上では出ない（両方向を見る）
- ④ タイルが 404 の録画ではプレビューが出ず、再生面（`src` と `duration`）は従来のまま
- ⑤ 帯をクリックすると、その位置でプレビューに出ていたタイルの時刻へ飛ぶ。
  **動画の配信は Range に応じる**（実物の streamer と同じ）。応じないと Chromium は
  動画を seekable にせず、クリックが 0 秒から動かない。比べるのは「見えていたタイルの
  background-position」と「飛んだ先の時刻のタイル」である
- ⑥ 帯が 1 枚ぶん（320px）より狭い画面（幅 340px）でも、プレビューが帯の幅に収まる
- ⑦ タッチ（`hasTouch` / `isMobile`）ではタップで飛ぶだけで、プレビューは出ない。タップが
  帯に届いたことを飛んだ先の時刻で確かめる（届かなければ「出ない」は空虚に通る）
- ⑧ 400px（light / dark）と 600px（light）で、プレビュー面積を映像の 16% 以下に抑え、
  ラベルを 14px 以上で表示して時刻・チャプター行から離す
- ⑨ 原本 120 秒、`keepRanges = [[0, 30000), [40000, 120000)]` の合成カット版（110 秒）を使う。
  cut 軸 25.5 秒は原本 25.5 秒のタイル #2、50.5 秒は原本 60.5 秒のタイル #6 を出す。
  ラベルは cut 軸の `0:25` / `0:50` を出す。クリック後の `video.currentTime` も cut 軸の 50.5 秒である。
  整数秒の境界は CSS ピクセルへの丸めで手前のタイルに入るため、各 10 秒格子の途中を指す。
  cut 版でもタイルを問い合わせ、404 ならプレビューを出さず再生面を保つ

判定用の動画は ffmpeg で作る（動画の長さが要る）。**VP8/WebM を使う** ---
Playwright の Chromium は H.264 を持たない構成があり、コーデックの有無で落ちると
「実装が壊れている」と区別できない。ffmpeg が無い環境ではこの判定だけを skip する。

**変異で落ちることを確認済み**: プレビューを `hidden` にする（①②③が落ちる）。
`SEEK_TILES_COLUMNS` を 5 にする変異も、①の位置判定で落ちる。
実測の文言は「タイル #9 の background-position が -1280px -180px（期待 -2880px 0px）」である。
ホバーの受け口を帯から外枠（映像を含む）へ広げる変異は③で落ちる。
クリック先だけ 0.8 倍にずらす変異は⑤⑦で落ちる（「-960px 0px を見てクリックしたが 28.0s（タイル #2）へ飛んだ」）。
狭い画面で縮めない変異は⑥で落ちる（「preview 32..352 / scrub 32..308」）。
ホバーの受け口を `onMouseMove` / `onMouseLeave` に戻す変異（修正前の実装）は⑦で落ちる。
追加した cut 判定は、修正前の実装でタイル問い合わせが始まらず、25.5 秒・50.5 秒のプレビューも出ずに落ちた。
タイル位置への cut → 原本写像を外し、cut 秒をそのまま格子に使う変異では⑨が落ちる。
実測は「cut 50.5s のタイル位置が -1600px 0px（原本 60.5s の期待 -1920px 0px）」で、
表示した tile #5 と期待する tile #6 を区別する。これは合成動画でクライアントの写像を測る判定で、
実録画のタイル生成と keep 区間の時刻軸が一致することは保証しない。

**Go 側と TS 側の定数が揃っていること自体は、ここでは測れない。**
`internal/worker/seek_tiles.go` と `src/lib/seek-tiles.ts` に同じ値が 2 つある。
この判定のフィクスチャは手書きで、Go が作る格子を再現していない。
揃っていることは両側の単体テスト（`TestSeekTileCount` /
`TestSeekTilesWorker_ComposeSheetArgs` と `seek-tiles.test.ts`）がリテラルで固定している。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:seek-tiles
```

### CM 検出のロゴ位置（`cm-logo-area.mjs`）

CM 検出のロゴ画面は `/cm-logos/{networkId}/{serviceId}` を局の資源として開く。
旧い query 形式は画面側のリダイレクト契約であり、この判定は新しい局ルートを使う。
API はブラウザ側で差し替え、1x1 PNG と `X-Coded-Width: 1440`、
`X-Coded-Height: 1080`、`X-Sample-Aspect-Ratio: 4:3` を返す。
画像の画素数ではなく coded size と SAR を使えていることを測る。
ロゴ候補のプレビューは幅 100px 超の実寸（今のロゴ 240×120、候補 200×100）にする。1x1 では狭い列での縮みと見切れが測れない。枠の PUT は実バックエンドと同じく候補を作らない（`running` 行は worker が後から作る）。

`data-testid` と `aria-label` は画面の契約である。

- `cm-logo-frame` / `cm-logo-frame-image` / `cm-logo-rect`
- `cm-logo-handle-nw` / `-ne` / `-sw` / `-se`
- `cm-logo-time`（`input[type=range]`）
- 数値入力の `aria-label`: `X` / `Y` / `幅` / `高さ`

判定する項目は次の 16 項目である。

- ① SAR を掛けたコマの表示比が 16:9（±1%）である
- ② `cm-logo-time` に範囲があり、トラックの 37% 位置を実際にクリックして初期値と違う値にすると、
  `/frame?at=` がその値で呼ばれる。録画 7（尺と原本つき）を詳細と一覧の両方で返す。
  要求の発行だけで③へ進むと旧画像の上でドラッグが始まり、途中で frame が null になって最小枠が残るので、新しい `img` の `src` が変わり `complete` / `naturalWidth` が成立するまで待つ
- ③ 右下ハンドルの CSS px の移動が coded size の `w` / `h` になる。分母は ① と同じ
  `cm-logo-frame-image` の描画寸法（外枠の border を含む寸法ではない）。
  PUT が届き、body の `w` / `h` が有限数であることも先に確かめる
- ④ 数値入力 `X` と `幅` が枠の位置と大きさを `120 × 描画幅 / 1440` の式どおりに動かし、
  枠のドラッグが `X` を移動量の式どおりに変える（±1）
- ⑤ スライダー / 枠外のコマ / 枠の中心 / 4 ハンドルの中心へポインタを動かし、
  `document.elementFromPoint` が返した最前面の要素の computed cursor が契約どおりである（透明な覆いを見逃さない）
- ⑥ 局名が描かれたことを確かめてから、4KB 相当の失敗ログが `textContent`
  （閉じた `<details>` も含む）に現れないことを見る
- ⑦ 旧形式 `/cm-logos?network=&service=&recording=` が局のルートへ `recording` を保ったまま飛ぶ
- ⑧ 数値入力へ `keyboard.type` で打てる（幅に 400 が 400 のまま、X の Backspace が 50）
- ⑨ 角より約 18px 外側でも、`elementFromPoint` が右下ハンドルで、掴むと X / Y を変えず幅が増える
- ⑩ 枠に寄った状態で枠を 20px ドラッグすると、枠が 20px（±2）動く
- ⑪ ドラッグ後の X / Y / 幅 / 高さが整数である
- ⑫ 720x480 SAR 8:9 の枠箱が 4:3 で、画像が枠箱を埋める（①の 1440x1080 でも画像 = 枠箱）
- ⑬ 400x860 で横スクロールが無く、`keyboard.type` で X / Y / 幅 / 高さを入れ、Tab も Enter も押さず
  「ロゴを解析」を押すと、PUT body が入力値どおりになる。
  ③ と ⑬ は `recordingId` が表示中の録画（7）で `atMs` を含まないことも見る。⑫ は録画 8 を表示して解析し、`recordingId` が 8 になる
- ⑭ 枠があり candidate の行が無い間も（メモリ state の無い開き直したページで）解析中が出る。
  その間もポーリングして、worker が作った `failed` の工程文が再読み込みなしで届く
- ⑮ `ready` は今のロゴと候補の両方で、描画幅が画素数×SAR 4:3（240→320、200→266.7）、描画高さが画素数と一致し、
  各 img が欄の矩形内に収まりスクロールを要さない。幅 1280 と 1024 で測る。N/M の入れ替えなど文言は単体テストで固定する
- ⑯ 採用前チェックを外すと `POST .../candidate/adopt` の body が `{ redetect: false }` になり、採用後は検出待ち件数を表示する

表示寸法を実測し、横は `css_x × 1440 / 描画幅`、縦は
`css_y × 1080 / 描画高さ` の式を判定側にリテラルで書く。
画面の純関数を import して比較すると、同じ実装を二度呼ぶだけになる。
実装が無い状態でこの判定を回すと、局のルートが Not Found になり各項目が
「要素が無い」で落ちる。実装後は全項目が通ることを確かめる。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:cm-logo-area
```

### チャプターの目盛りと自動スキップ（`chapters.mjs`）

録画の詳細で、CM の目盛りが帯の正しい位置に出ること・通常の再生で `cut` 区間の先頭に
差し掛かると終端へ飛ぶこと・手動のシークで区間の中に入ったときは飛ばないこと・境界の
「前後 3 秒」が境界の手前から始まることを実ブラウザで見る。**目盛りの位置は帯の実寸に
対する割合で決まり（jsdom の `getBoundingClientRect()` は常に 0）、スキップは実再生の
`currentTime` の推移でしか観測できない。** 判定はここが唯一の手段になる。

フィクスチャは ffmpeg で作った 120 秒の WebM に、`[30, 40)` の `cut` な CM と
`[60, 70)` の `cut` でない OP を重ねる。**再生位置は localStorage に残る**ので、
判定の前に `localStorage.clear()` する（前回の続きから始まると位置の判定が揺れる）。
音声トラックが無くても Chromium の自動再生ポリシーは掛かるので、
`--autoplay-policy=no-user-gesture-required` を付けて起動する。

- ① 目盛りの数と左端の座標・幅が区間の割合と一致し、`data-cut` が区間の `cut` と一致する
- ② 区間の手前 2 秒から再生して 4 秒後、位置が区間の終端を越えている（飛ばなければ
  32 秒付近にとどまる）
- ③ 区間の中へシークしてから再生しても追い出されない。**cut でない OP を通り抜けても
  飛ばない**（両方を見る）。再生が進んでいることも確かめる（空虚な成功を防ぐ）
- ④ 編集の `<details>` は閉じているので、先に `summary` を開いてから見る。境界の「前後 3 秒」が 1 秒後に境界の手前 3 秒付近に居る（境界そのものから始める
  実装と区別できる）。そのまま境界を跨いでも飛ばされない（前後再生の間は自動スキップを
  止める）

**変異で落ちることを確認済み**: 目盛りの `left` を区間の終端にする変異は①
（「実際 x=480.0 w=64.0 / 期待 x=416.0 w=64.0」）。飛び先を `currentTime` に代入しない
変異は②（「4 秒後の位置 32.0 秒」）。直前位置を常に 0 にする変異は③
（「シークで入った cut 区間から追い出された（位置 42.7 秒）」）。前後再生でスキップ抑制を
外す変異は④（「前後 3 秒の再生中に境界で自動スキップが働いた（位置 41.38 秒）」）。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:chapters
```

### 次のエピソード・終端カード・棚（`recording-next-episode.mjs`）

録画詳細の「次のエピソード」まわりを、ffmpeg 製の 120 秒 WebM（`chapters.mjs` と同じ。Range に応じる）で
実ブラウザに通す。**次のものは jsdom では測れない。** 終端カードが出るか・読めるか、移った先が再生を始めるか、
全画面が保たれるか、番組外区間が破線で描かれるか、である。 フィクスチャは同じシリーズの録画 5 件
（過去の回・開いている回・次の回・原本のみ・録画中）で、開いている回だけ前後 6 秒長く録っている。

- ① 番組外区間の位置が割合（6/132 と 126/132）と一致し、中央の 1 行に明暗が交互に出る（破線）
- ② 終端カードの文言・次の回の放送日時・輪・サムネイル、ボタン 3 つが同じ行にあり、
  文字と地のコントラスト比が 4.5 以上（デスクトップとスマホ。映像の枠からはみ出さない）
- ③ 「取り消す」の後 4 秒待っても移動せず、カードも消えている
- ④ 取り消さなければ次の回へ移り、再生が進み、全画面要素が同じ枠のまま保たれ、「戻る」で戻れる
- ⑤ 次が再生できない最後の回のカードは「もう一度見る」と「この回をごみ箱へ」だけ
- ⑥ 棚の見出し・新しい順（過去の回を含む）・行を押すと移る・スマホでは出ない
- ⑦ シリーズへのリンクが各状態（エンコード版・原本のみ・録画中・ごみ箱）でちょうど 1 つ見える。
  1280 では棚の見出し（「›」と下線付き）で、ごみ箱だけはタイトル下。400px ではタイトル下にあり、
  「シリーズ」の見出し語と下線を持つ
- ⑧ バーの次のエピソード（デスクトップは「次: 9/30(水)」、スマホはアイコンだけ）
- ⑨ 次の回へ移ると、前の回のチャプター編集の下書きと開閉が残らない（移動先のチャプターは取得済みにしておく）。
  選んだ画質も持ち越さず、映像の `src` と版タブの「再生中」が既定の画質を指す
- ⑩ 棚のサムネイルの下端の進み線が、視聴済みで全幅・途中で保存位置の割合・未視聴で無し

**変異で落ちることを確認済み**: カードのボタンを白地に白文字にする変異は②
（「『取り消す』の文字と地のコントラスト比が 1.00」）。破線を両端の縦線に戻す変異は①
（「中央の 1 行の明暗の切り替わり 0 回」）。移動先の自動再生を外す変異は④
（「自動で移った先が再生を始めない」）。棚の行を押したときの先読みを外す変異は⑥
（「プレイヤーの枠が作り直された」）。スマホで棚を出す変異も⑥ で落ちる。
ページの `key` に録画 id を戻す変異は⑥ と④（「自動で次の回へ移ると全画面が解除された」）。
取り消しを効かなくする変異は③（「取り消したのに /recordings/2 へ移った」）。
自動遷移を 3 秒待たずに起こす変異は、カードを観測する前の待ち受けが TimeoutError で落ちる。
チャプター編集を録画 id で作り直さない変異は⑨（「1 話の下書きが 2 話に漏れた」）。
プレイヤーが録画 id の変化で画質の選択を捨てない変異も⑨（「移った先の映像が既定の画質でない」）。
`RecordingDetail` の id 変化での戻しを消す変異も⑨（「版タブの『再生中』が映像の画質と一致しない」）。
棚の進み線を描かない変異と、視聴済みを全幅にしない変異は⑩ で落ちる。
タイトル下のシリーズの行を lg 以上でも出す変異は⑦（「エンコード版（1280）で見えるシリーズへのリンクが 2 個」）。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:recording-next-episode
```

### 番組表の表示形式（`programs-view.mjs`）

番組表で選んだ表示形式が端末に保存され、URL に `view` が無いリロードでも復元されることを
確認する（issue #722）。加えて、リロード後に `list` の DOM が先に現れず、最初の表示分岐が
`grid` になることを実ブラウザの `MutationObserver` で判定する。これは `useMediaQuery` の
初回値を固定の `false` に戻す変異で `list → grid` として失敗する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-view
```

### 番組表の短い番組選択（`programs-grid-zoom.mjs`）

短い番組の選択は、セルの視覚的な高さに下限を入れず、時間軸全体を 120 / 240 /
480 px/時で拡大することで解決する（issue #724）。jsdom ではセルの実矩形や
`scrollTop`、隣接セルの境界付近を実際に押した結果を測れないため、実ブラウザで
次を確認する。

- 既定の 5 分 / 10 分 / 30 分セルが 10 / 20 / 60px、480px/時では 40 / 80 /
  240px になり、高さ = 放送時間の比例が保たれる
- ズーム前後でグリッドの可視起点の時刻がずれない
- 隣接する 5 分・10 分・30 分番組を境界付近で押しても、別セルが選択されない

実装前は時間軸ズームの操作点が存在せず、5 分セルは 10px のままなので、①と
③の判定が落ちることを確認できる。API は `page.route` で差し替えるため mirakc・
実チューナー・DB は不要で、フィクスチャは `validateFixturesOrExit` で契約検証する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-grid-zoom
```

### 録画一覧の絞り込みパネルのルール節（`recordings-rule-filter.mjs`）

絞り込みパネルは `max-h-[min(34rem,80vh)]` の中をスクロールさせるので、節を 1 つ
足したときに高さ予算を超えないか・末尾の節まで届くかは jsdom では測れない。選択肢
に無い `ruleId`（削除済みルール・古い共有リンク）を渡したときの `<select>` の挙動
も、HTML の ask-for-a-reset に従うかどうかは実装依存なので実ブラウザで測る。

- 390px / 1280px でパネルの実測高さが 544px（34rem）以内に収まり、ルール節の
  `<select>` と末尾の「種別」節がスクロールで到達でき（中心のヒットテストが
  その要素に当たる）、ページが横に溢れない
- `?ruleId=99`（一覧に無い）で開くと `<select>` がフォールバック option
  `ルール #99` を選択状態にする（先頭の「問わない」に落ちない）
- `?ruleId=8`（一覧にある）では名前の option が選ばれ、チップが `ルール: <名前>`
  で出る
- ルールが 0 件なら節ごと出さない

直す前の実装（フォールバック option・チップの接頭辞・節のゲートが無い版）では
実測で③④⑤が落ち、`<select>` は実 Chrome でも `value=""` / 表示「問わない」に
なる。①②は節が増える前から予算内なので落ちない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:recordings-rule-filter
```

## CI で回す 4 本とそれ以外

CI の `browser-e2e` ジョブは、実バイナリが `go:embed` した `dist/` を配るサーバーへ Chromium を向ける。
回すのは `cls` / `chip-overflow` / `recordings-selection` / `recording-detail-layout` の 4 本だけである。
4 本は 1 本が落ちても残りを走らせる。
選定基準は次の 3 つを全部満たすことである。

- jsdom が原理的に測れない（レイアウトシフト・幅の溢れ・スクロール余白・viewport への収まり）
- `/api/**` を Playwright 内でスタブし、mirakc・チューナー・実データに依存しない
- ffmpeg・webkit・DB への直接書き込みを要らず、Chromium だけで軽く終わる

それ以外の 31 ファイル（判定スクリプトは 29 本。`lib.mjs` と `validate-fixtures.mjs` は共有部品）は
**ローカルでの受け入れ確認**の位置づけである。
[docs/frontend.md](../../docs/frontend.md) の「受け入れは実機で行う」に実行可能な形を与えるものだ。
回さない理由は 3 類型ある。

- 実メディアが要る: `chapters` / `seek-tiles` / `subtitles` / `recording-next-episode` / `chase` / `live` / `live-audio` は
  ffmpeg でフィクスチャを作る。`live` と `live-audio` は webkit も要る
- 実 DB の状態が要る: `checks`（既定の `pnpm e2e`）は API をスタブせず実 EPG の番組行の描画を待つ。
  `live` は `epg_services` に実サービスの行が要り、`shelves-split` は `E2E_DATABASE_URL` の DB を TRUNCATE する
- 残りの画面別判定（番組表・予約・検索など）は API スタブで技術的には載せられる。ただし
  全体の所要時間を測っておらず、毎 PR に払う価値をまだ判断していない。`design.mjs` は
  63 枚のショットを撮るぶん重い

判定スクリプト全 33 本を回す定期ジョブは作らない。回す主体と失敗の受け手が決まっておらず、誰も見ない
赤い定期ジョブは PR ごとに回す 4 本より信号として弱いためである。対象を増やすときは
`.github/workflows/ci.yml` のコメントとこの節の本数・類型を同じ PR で直す。
実ブラウザ不要の `pnpm check:colors` は lint job に入っている。

## 判定を足すときの規律

**足した判定が、直す前の実装で実際に落ちることを確認すること。** 落ちない判定は何も判定して
いない（CLAUDE.md「テスト規律」のユニットテストと同じ）。

**時計を固定した判定は、時計が動くことに起因する欠陥を検出できない。** `page.clock.setFixedTime`
（このファイルの各判定）も jsdom の `vi.setSystemTime`（`pnpm test`）も時計を止めている。
その代償として、時計が動くことで露呈する欠陥はどちらの経路でも原理的に見えない。たとえば
「レンダーのたびに変わる値（生の `Date.now()` 等）をキャッシュキーに載せて無限再取得になる」
欠陥は、時計を止めた構成では再現しない（`pages/home.tsx` の容量超過クエリで実際に踏んだ）。
時計に起因する挙動を判定したいときは、時計を止めない経路を別に用意すること
（`design.mjs`・`pages/home.test.tsx` の「実時計でのクエリキー安定性」参照）。

**`waitFor` の失敗を `.catch(() => {})` で飲むときは、後段が飲んで良い理由になっているか
を確かめる。**後段が「待っていた条件そのもの」を直接読み直して合否を出す形（例:
`document.activeElement === el` を評価し直す）なら、待ちの成否を経由しないので飲んでよい。
そうでない形（後段が別の要素・別の状態を測る）で飲むと、「待ちが失敗した」ことが
その別の判定の NG 文言に化けて出る --- スタイル回帰と区別が付かず、しかもタイミング
依存で再実行すると消える。この規律自体はまだこのリポジトリで踏み抜いたことを実測で
示せていない（予防的なもの）--- 下記 ChannelOption の一件は、調べる過程でこの形の
壊れ方ではないと分かった。

**before/after diff を取る共有ヘルパーは、before の基準を自分で制御しないと壊れる**。
`design.mjs` の `checkExplicitFocusRing`（ChannelOption 呼び出し）で実際に踏んだ。
base-ui の Popover は開いた直後、先頭候補へ非同期に既定フォーカスを当てる
（queueMicrotask → requestAnimationFrame 1 回）。この既定フォーカスが「まだ来ていない」
か「もう来た」かで before の box-shadow が実行のたびに割れ、before/after の差分判定
（before と after が同じなら NG）が偽陽性を出していた。ポップアップの開閉待ち自体は
5000ms の予算に対し 1〜3ms で毎回成功しており、swallow とは無関係だった（4 回計測、
8/8 で `beforeShadow !== 'none'` と NG が一致）。直すには before を測る前に明示的に
`blur()` して基準を確定させ、かつ相手の既定フォーカスが済むのを待ってから呼ぶ
（rAF 2 回。フレーム数は負荷で増えないので固定回数で足りる）。
