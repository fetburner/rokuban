> [web/e2e/README.md](../README.md) の一部。索引から辿る

### 多 site 番組表（`multi-site.mjs`）

`tokyo` と `takamatsu` に同一 `networkId` / `serviceId` / `Service.id` / 局名 / `programId` の共有 BS fixture を配る。グリッドとリストで可視の site 名があり、React key が衝突しないことを確認する。同時刻の容量超過は帯が各 site の列内に収まり、時間軸列のラベル同士が重ならないことを実ブラウザの矩形で測る。これは jsdom では列幅・横方向の配置を測れないため、実装より先に追加した判定である。

site 固定撤去前（フロントが単一 site 決め打ちだった頃）はこの判定を通すと列が 1 本で red（`描画された列: 1`）になり、site 和集合化後は green になった。下記の GR fixture を足した現在、期待する列数は 6 本（`expectedColumnCount`。共有 BS 1 局 + GR 2 局 × 2 site）。

**GR fixture（両 site で同じリモコン番号を持つが別放送の局。実在の東京・高松の NHK 総合・NHK E テレと同じ形）も配る**。 `orderServices`（`lib/epg-grid.ts`）がリモコン番号を site より先に比べる実装だと、この 4 局は 1 列ごとに site が交互する順になる（レビュー指摘）。

左端からの並びで「同じ site が連続する走」を数え、各 site の走が種別の本数（GR 1 本 + BS 1 本 = 2 本）に収まっていることを実測する（①）。

GR 局を足すと site の列領域が非隣接な複数の走に分かれるため、`ProgramGrid` は走ごとに `siteOverlay` を呼ぶ --- 対策前はその本数ぶん容量帯の読み上げ文（sr-only）が重複する。帯（見た目）は走ごとに複数出てよいが、読み上げ文は 1 site につき 1 つだけであることも実測する（②）。

**列ヘッダーの site 名が `overflow-hidden` で視覚的に切れていないことも実測する（①´）**。`allTextContents()` は切れていても文字列自体は取れてしまう。そのため要素の矩形が列ヘッダーの矩形（`program-grid-header-cell`）に収まっているかを `getBoundingClientRect()` で確認する。ヘッダーの高さを意図的に詰めた実装（`headerHeightPx` を縮める）で実際に落ちることを確認済み。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:multi-site
```

### 正時をまたぐグリッド再取得（`programs-grid-hour-boundary.mjs`）

「今日」のグリッドを開いたまま正時を越え、番組・容量超過の再取得を保留した間に、グリッドがスケルトンへ戻らず、スクロール中の番組位置と表示範囲内の不足帯を保つことを確認する。新しい時間軸の起点・現在時刻線・放送終了判定が進むことと、翌日 0 時の保留中に日付見出しが変わることも見る。時計だけを進めても画面は再レンダーされないため、判定では正時後にジャンルを選び、変わった queryKey の応答を保留する。修正前は番組クエリが pending の間にグリッドがスケルトンへ置き換わり、判定が落ちることを確認した。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-grid-hour-boundary
```

### 参照バッジのリンク（`badge-links.mjs`）

容量不足バッジ（予約一覧）から番組表へのリンク（issue #233 M6-5、`view` の URL 化は issue #437）。見るのは主に 2 点 --- ①バッジが行本体の詳細リンクの中に入れ子の `<a>` として置かれておらず、クリックすると番組表（`/programs?view=grid&at=...`）へ飛ぶこと。②`lg` 以上ではリンクが積んだ `view=grid` どおりグリッド表示になり、不足区間の帯がスクロール後に可視範囲へ入っていること（②' として「今日」ボタンを押した後 `at` の位置ではなく現在時刻へスクロールし直すことも見る）。加えて③として `lg` 未満（グリッドが出ずリスト表示のまま）でもクリックが機能しエラーにならないことまで見る --- ①②は 1280px でしか開かないため、この経路を通さないと隠れたバグに気付けない。

②・②' はスクロール位置そのものの判定なので、jsdom（`pnpm test`）では原理的に確認できない。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は `page.clock.setFixedTime` で固定）で、mirakc も実チューナーも DB も要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:badge-links
```

**配っている bundle が `dist/` の現物と一致するかは、スクリプト自身が毎回 ⓪ として自動で確認する**（`verifyBundleMatches`）。不一致なら他の判定をせず即 exit 1 するので、`curl`/`ls` で手動照合する必要はない。これは実際に起きたトラブルの再発を、人の確認忘れに頼らず機械で止めるための仕組みである。そのトラブルは `--strictPort` を付けていても起きた --- 複数の worktree を並行して触っていると、別の worktree の preview が同じポートに先に居座る。すると自分の起動が何も出さずに失敗し、`E2E_URL` が無関係な古いビルドを指したまま判定が進んでしまう。ポート自体は空いているものを選ぶこと（`--strictPort` が起動を失敗させるので、選び間違えればここで気付ける）。

### シリーズ一覧の分割（`shelves-split.mjs`）

**この判定だけは `/api/**` を差し替えない。** 確かめるのは次のことである。「`/rules` で分類ルールを 1 本作ると、全件再評価の後に `/series` の棚が割れる」。つまり API、worker の再評価ジョブ、SSE、棚の再取得という非同期の連鎖の実物である。実バイナリ（`--roles api,worker,notifier`）と専用の DB が要る。SSE は notifier ロールが配るので、api と worker だけでは棚が再取得されない。`worker.queues: [cleanup]` で足りる（再評価ジョブは cleanup キュー）。mirakc もチューナーも要らない。

判定は `E2E_DATABASE_URL` の DB を **TRUNCATE して**録画 12 件（数学 6 + 化学 6、自動キーはどちらも `NHK高校講座`）を作る。開発用 DB を指さないこと。サーバーは起動時に `media_asset_copies` の `rel_path` 名前空間を検査するので、先に同じ形のコピー行を入れてから起動する。

作成と観測は別のページで行う。1 枚目は `/rules` でルールを作り、2 枚目は最初から `/series` を開いたまま操作しない。読み込み前に通る空虚な成功を避けるため、作成前に 2 枚目で「1 棚 12 件」を見てから始め、作成後は割れるまでポーリングする。2 枚目は作成のレスポンスで再取得されないので、割れたことを観測できるのは SSE 経由だけである（待ちは 30 秒で、60 秒周期の定期取得では通らない）。値に空白を含めるとフォームが「この値は棚キー NHK高校講座 として扱われます」と注記することも見る。

確認した壊し方は 3 つある。再評価ジョブの投入先を、worker が引かないキュー（ruler）へ戻すと「シリーズが割れる」が落ちる。フォームの注記を消すと「食い違いの注記」が落ちる。notifier ロールを外して起動すると「シリーズが割れる」が落ちる（割れるのが SSE 経由だけだから）。実装前の画面（`/series` も `/rules` の分類ルール節も無いビルド）に向けると、作成前の「1 棚 12 件」が成立せず、`分類ルールを作成` ボタンも見つからずに落ちる。

```sh
E2E_URL=http://localhost:40799 \
  E2E_DATABASE_URL='postgres://localhost:5432/<サーバーと同じ DB>?sslmode=disable' \
  pnpm e2e:shelves-split
```

### 番組表グリッドの空間ナビゲーション（`grid-navigation.mjs`）

番組表のセルにフォーカスを置いたとき、矢印キーで空間的に移動できることを実ブラウザで確認する。`role="region"` とセルの Tab 停止点は維持したまま、次の 2 点を見る:

- ① `ArrowRight` で、フォーカス中の番組の開始時刻を含む隣列の番組へ移る
- ② 仮想化で最初は DOM に無い同列の遠い番組へ `ArrowDown` で移り、スクロール後にそのセルへフォーカスが移る

jsdom ではレイアウト・スクロール位置・仮想化後のフォーカスを同時に測れない。API は `page.route` で差し替え、固定時刻と 2 列の番組だけを使うので mirakc も DB も要らない。フィクスチャは `validateFixturesOrExit` で生成スキーマと一致することも確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:grid-navigation
```

### 番組表グリッドの予約済み印（`grid-reserved.mjs`）

番組表グリッドで予約済みがジャンルの淡い塗りに埋もれる（issue #307）。見える印が `ring-1`（1px）と `size-1.5`（6px）の点だけで、「予約済み」は `aria-label` にしか無い。選択中は同じ `ring-primary` の `ring-2` なので、差は太さだけ。`pnpm test` の既存判定は `data-reserved` と `aria-label` だけを見ており、見た目の差は見ていない。

jsdom は色も要素の大きさも測れないので、ここが唯一の判定手段。見るのは:

- ① 予約済みセルに、aria ではない見える「予約」がある。箱が 6px の点より大きい（幅 16px 以上）。同じジャンルの未予約セルには無い
- ② 5 分（10px）の予約済みセルでも印が消えない --- 見える「予約」、またはセルの高さの 8 割以上を覆う縦の帯
- ③ 未予約の `skip` は、セルの高さが 24px 以上なら「スキップ中」バッジ、24px 未満なら高さいっぱいの状態マーカーだけを表示する。短いセルに切れる文字バッジを描かない
- ④ 予約済み（未選択）と選択中（未予約）は別の形。予約済みだけに見える「予約」がある
- ⑤ 印の色はタリー / 琥珀 / destructive ではない（色は信号のみ）

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は `page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:grid-reserved
```

### 番組表セルの操作モーダル（`programs-dialog.mjs`）

番組表のセルをクリックしたとき、選択した番組が番組表の最上部へ移動するのではなくダイアログで開くことを実ブラウザで見る。jsdom では CSS による可視性、フォーカストラップ、overlay / Escape による閉鎖後のフォーカス復帰、スクロール中の要素の可視性を測れないため、次の 6 点を判定する:

- ① セルのクリックで番組名を `aria-labelledby` に持つダイアログが開く
- ② ダイアログ内に `ProgramRow` をマウントせず、予約パネルの「予約」が hover なしで可視・44px 以上・操作可能で、1 回のクリックで `PUT .../intent` が 1 回だけ飛ぶ。操作列は要約行の右端に揃い、通常 81px / 放送中 125px の実幅になる。通常 / 放送中の 2 状態のダイアログは `e2e/screenshots/program-dialog-*.png` に保存する
- ③ 予約ボタンと閉じるボタンが重ならず、番組概要が長くて本文がスクロールしても、閉じるボタンが画面外へ出ない
- ④ Tab 走査がダイアログの外へ出ない
- ⑤ Escape で閉じ、クリック元セルへフォーカスが戻る
- ⑥ overlay と「閉じる」ボタンでも閉じ、クリック元セルへフォーカスが戻る

**フォーカス復帰は base-ui の `Dialog.Popup` の既定（`finalFocus` 省略時の「trigger or previously focused element」）に任せている**。controlled Dialog（`Dialog.Trigger` が無い）でも既定はクリック直前のフォーカス要素へ戻るため、このページ側で `finalFocus` を自作する必要は無い。自作すると、ボタンのクリックで focus を移さない macOS/iOS Safari で `document.body` を対象にしてしまい、既定より悪化する。

API は `page.route` で丸ごと差し替えるので mirakc・実チューナー・DB は要らない。⓪（配っている bundle と `dist/` の一致）とフィクスチャの zod 契約検証も行う。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-dialog
```

### 番組リストの空時間窓（`programs-empty-window.mjs`）

番組 API が最初または途中の 6 時間窓を空で返しても、後続窓へ進む入口が消えないことを 390px 幅の Chromium で見る。空窓を 2 回進め、後続取得の失敗後に同じ窓を手動再試行して番組へ到達すること、番組表示後の空窓で自動取得が連鎖しないこと、最終窓ではボタンと追加要求が消えることを判定する。通常の番組あり窓の日付移動と自動読み込みは `checks.mjs` が引き続き判定する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-empty-window
```

### 予約一覧取得失敗時の番組表レイアウト（`programs-reservation-error.mjs`）

`pages/programs.tsx` のグリッドの高さ予算は次の式で決まる。`height: calc(100dvh - var(--page-header-height, 0px) - var(--sticky-banners-height, 0px))` である。`reservations.isPending` / `isError` のバナーを `PageHeader` の**外**（通常フローの兄弟）に置くと、バナーの高さがどちらの CSS 変数にも入らない。すると 100dvh で組んだ画面なのに文書がビューポートを超え、ページ全体がスクロールする（実測: 外に置くと 1440x900 で 949px / 900px、`PageHeader` の中なら 900px / 900px）。この失敗はレイアウトそのものなので jsdom（`pnpm test`）では原理的に検出できない。

`pages/programs.tsx` のコメントが挙げる「グリッドの sticky ヘッダが画面外へ出る」という症状そのものは、**この構造では再現しなかった**。外に置いた状態で文書を最後までスクロールしても、グリッド内の `GenreLegend`（見出し行の上にある帯）が緩衝になって 39px の余裕が残るためだ。下の判定 B はその症状の再現ではなく、緩衝が無くなったときに気付くための網である。

`GET /api/reservations` だけを常に 500 で返し、次を見る:

- デスクトップ（1440x900、`/programs?view=grid`）
  - A. `document.documentElement.scrollHeight` が `window.innerHeight` を超えない（文書がはみ出さない）
  - B. グリッドのサービス列見出し（`program-grid-header-cell`）の上端が `PageHeader` の下端より下にあり、ビューポート内に見えている
  - C. グリッドのセルを選ぶと出る選択済み番組の行の「予約」ボタンが disabled であること。予約状態が不明なまま record intent を送らないことの実ブラウザ確認である。表示形式ごとに boolean prop を渡す実装ではグリッドだけが渡し忘れており、実際に `PUT .../intent` が飛ぶことを測った
- モバイル（390x844、`/programs` リスト）
  - D. 失敗バナーを飲み込んだ `<header>` の実測高さ（`offsetHeight`）。合否ではなく測定値（sticky に入れた代償の定量化）。緩い上限（viewport 高さの半分未満）だけ置く。実測は平常 121px / 1 行の帯 170px（`ErrorState` を使っていた案では 281px）
  - E. その状態でも番組リストの先頭行がヘッダの下に見えていること

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は `page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

**`pnpm e2e`（`checks.mjs`）には足さない。** `checks.mjs` は判定を集約するハブではなく、番組リストの日付ジャンプだけを見る独立したスクリプト（既存の他スクリプトも `pnpm e2e` からは呼ばれていない）。したがってこのスクリプトも他と同じく独立した `package.json` スクリプトのままにする。

```sh
pnpm build && go build -o /tmp/rokuban ../cmd/rokuban && /tmp/rokuban server --roles api --config ../dev.local.yml &
pnpm e2e:programs-reservation-error
```

### 番組表の表示形式（`programs-view.mjs`）

番組表で選んだ表示形式が端末に保存され、URL に `view` が無いリロードでも復元されることを確認する（issue #722）。加えて、リロード後に `list` の DOM が先に現れず、最初の表示分岐が `grid` になることを実ブラウザの `MutationObserver` で判定する。これは `useMediaQuery` の初回値を固定の `false` に戻す変異で `list → grid` として失敗する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-view
```

### 表示切替の形（`issue-1229-switches.mjs`）

Programs のリスト / 番組表が 1 つの segmented control に見えることと、Home の見る / 管理が無彩の下線付き tabs になることを、Chromium の実画面で確認する。URL と localStorage による両方の選択状態も確認し、ライト / ダークと 1280px / 390px の画面を PNG に保存する。API はブラウザ内スタブで差し替える。

```sh
pnpm build && pnpm preview --host 127.0.0.1 --port 4173 --strictPort &
E2E_URL=http://127.0.0.1:4173 E2E_SHOT_DIR=/tmp/rokuban-switch-shots pnpm e2e:issue-1229-switches
```

### 番組表の短い番組選択（`programs-grid-zoom.mjs`）

短い番組の選択は、セルの視覚的な高さに下限を入れず、時間軸全体を 120 / 240 / 480 px/時で拡大することで解決する（issue #724）。jsdom ではセルの実矩形や `scrollTop`、隣接セルの境界付近を実際に押した結果を測れないため、実ブラウザで次を確認する。

- 既定の 5 分 / 10 分 / 30 分セルが 10 / 20 / 60px、480px/時では 40 / 80 / 240px になり、高さ = 放送時間の比例が保たれる
- ズーム前後でグリッドの可視起点の時刻がずれない
- 隣接する 5 分・10 分・30 分番組を境界付近で押しても、別セルが選択されない

実装前は時間軸ズームの操作点が存在せず、5 分セルは 10px のままなので、①と③の判定が落ちることを確認できる。API は `page.route` で差し替えるため mirakc・実チューナー・DB は不要で、フィクスチャは `validateFixturesOrExit` で契約検証する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-grid-zoom
```
