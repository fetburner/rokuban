> [web/e2e/README.md](../README.md) の一部。索引から辿る

### ホームの保存位置フレーム（`home-resume-frame.mjs`）

Go の製品ビルダーが作る目印付き MP4 から映像 track を正規化し、非カット版と `keepRanges` で連結したカット版を配る。Chromium と WebKit で、ホームの先頭に出す 1 本が frame 492 の目印を表示すること、動画を再生せず、表示中のコマが出るまで固定サムネイルを残すことを測る。動画取得が失敗したときに固定画像へ戻ることと、絵を押すと録画詳細へ移ることも見る。表示中の frame を canvas の画素で読み、`currentTime` だけを根拠にしない。`keepRanges` の写像を外す変異では、カット版の目印判定が落ちる。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:home-resume-frame
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:home-resume-frame
```

ffmpeg / ffprobe / Go と Chromium / WebKit を使うローカル判定である。Safari / iPhone は確認対象にしない。動く根拠は Chrome と同じ hls.js 経路を通ることで、Playwright WebKit は hls.js 経路の診断に使う。

編集モード（チャプターを直す）の境界操作も、同じ目印フレームで判定する。非カット MP4 と原本 HLS（offset 0 / 10）で次を見る。原本 HLS は押す前に境界の 0.1 秒手前へ置く（MP4 は 0.5 秒手前）。原本 HLS offset 0 は、0.5 秒手前から境界を選んだ後に最初に表示されるフレームと「境界まで」も見る。

- 境界を選ぶと一時停止して、境界の目印フレームが映り、「境界の直後」の説明が出る
- ±1 フレーム: 区間の終端の境界を選んで +1 / -1 し、次に表示されたフレームの `mediaTime` が隣のフレームの PTS に一致する。**開始境界で測らない**。1 フレームの区間の開始境界を進めると区間が空になって消え、選択が別の境界へ移る（目印 9 個のうち消えない目印は 1 個だけだった）
- 「境界まで」が目印のコマで止まる。「境界から」が、`play()` を呼んだ瞬間の `currentTime` が選んだフレームの中に居る状態で始まり、約 3 秒で止まる
- ±1 フレームボタンの長押し 1.5 秒で 5〜8 フレーム動き、離した後に最後の境界のコマ中央へ移る。元に戻す 1 回で押す前の境界へ戻る。送った回数と seek 時間はログに出る
- 400×800 の帯の中で、再生ボタン・時刻・3 つのモードボタン・速度ボタンが映像の枠内に収まり、互いに重ならない

**WebKit の原本 HLS も表示フレームで判定する。** 原本 HLS は WebKit でも hls.js で再生するので、Chrome と同じ判定を通す。2026-10-30 に Playwright WebKit で 1 回回し、NG は 0 件で exit 0 だった。落ちたときは、同じ位置へ `+0.0001` 秒の seek をもう一度行った結果を「診断:」としてログに出す（判定には使わない）。

境界の選択だけを回すなら `E2E_TIMELINE_BOUNDARY_SEEK_ONLY=1` を使う。 ±1 フレームの判定は rVFC の `mediaTime` で行い、ログの `SRC` 行に捕捉経路（`rvfc` / `paint`）を出す。補助経路（seeked 後の二重 rAF）は `mediaTime` を読めないので NaN を返し、時刻の判定は NG になる。

一時停止中の seek の後に映像が前のフレームのまま残るかは、素の `currentTime` 代入でも `E2E_TIMELINE_RAW_SEEK_DIAGNOSTICS=1` で測れる。製品のボタンを通さず、編集モードの同じページで代入だけを行う。診断だけを回すなら `E2E_TIMELINE_RAW_SEEK_ONLY=1` を使う。hls.js 経路では未測定である。

実録画でも測るときは、`E2E_TIMELINE_REAL_HLS_URL` に localhost の HLS playlist を渡す。非 loopback の URL はスクリプトが拒否する。目印が無いので、連続再生で得たフレームの画素 fingerprint と、境界を選んだ後に表示されたフレームの fingerprint を比べる。画像や動画は保存しない。境界は `E2E_TIMELINE_REAL_HLS_BOUNDARY_SECONDS`（既定 1 秒）で選ぶ。

```sh
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit E2E_TIMELINE_RAW_SEEK_ONLY=1 pnpm e2e:recording-playback-timeline
E2E_URL=http://127.0.0.1:4173 E2E_BROWSER=webkit E2E_TIMELINE_RAW_SEEK_ONLY=1 \
  E2E_TIMELINE_REAL_HLS_URL=http://127.0.0.1:<port>/playlist.m3u8 pnpm e2e:recording-playback-timeline
```

**変異で落ちることを確認済み**（Chrome）。WebKit の変異は hls.js 経路では未測定である。

- ±ボタン・`,` / `.` の境界移動からシークを外す（`moveBoundary` が `selectBoundary` の代わりに選択だけを更新する）。
  - Chrome は 6 件落ちる（2026-10-06 に再測定）。非カット MP4 と原本 HLS offset 0 / 10 の +1 フレームと、長押しを離した後の到達位置である。
  - 例は「非カット MP4: +1フレーム後に隣のフレームへ seek しない」で、`mediaTime` は 2.235567、期待は 2.268933 だった。
- 「境界まで」の停止後のシークし直しを外す（`finishPlayAround` の境界変換シークを無効化する）。
  - Chrome は MP4 と原本 HLS offset 0 / 10 の 3 か所で落ちる。
  - 例は「非カット MP4: 「境界まで」が境界のコマで止まらない (目印 frame 181 slot=2 が映らない)」。このとき `t=6.872716` で slot=-1 だった。

### ボトムタブの高さと本文の下パディング（`programs-bottom-nav.mjs`）

`--bottom-nav-height`（`web/src/index.css`）がボトムタブの実際の描画高さと一致しているかを 390×844 の実ブラウザで見る。jsdom は `getBoundingClientRect` を計算しない（常に 0 を返す）ので、ここで見る値はどれもユニットテストでは原理的に取れない。

**タブは `fixed` のオーバーレイのままとし、ここで保証するのは到達可能性だけである**。ページ全体スクロール + `fixed` なタブでは、途中のスクロール位置で行がタブの裏に入ることを仕様として受け入れる。重なり量の観測・判定は行わない（`docs/frontend/scroll.md`「ボトムタブの裏に隠れる行」）。

このスクリプトが見ているのは別の欠陥である。`--bottom-nav-height` から nav の上辺の境界線ぶん（1px）が落ちており、`main` の `padding-bottom`（64px）がタブの実寸（65px）に足りていなかった。**この状態でも最下端での余白はちょうど 0px で、隠れていた画素は無かった**（実測）。落ちていたのは計算の正しさと 1px ぶんの余裕だけで、見た目に現れる症状ではない。

見るのは:

- ① `main` の計算済み `padding-bottom`（= `--bottom-nav-height`）がボトムタブの実際の描画高さ（border 込み）と一致すること。**境界線ぶんを落とすと 64px 対 65px で落ちる**（実測で確認済み）
- ② 最下端までスクロールしたとき、`main` の内容ボックスの下端がタブの上端より下に来ていないこと。①に加えて「`main` が文書の最下端の箱である」ことも見る。同じ変異で 780px 対 779px で落ちる
- ③ 最下端でタブの上端をまたいでいる行が無いこと。**これは余裕ゼロの回帰確認で、境界線落ちに対する検出力は無い**（上記のとおり修正前も余白 0px で通る）。①②が捉えない別種の失敗に対する網として置いてある。`min-h-14` → `min-h-16` の変異では①②③すべてが落ちることを確認済み

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え、時刻は `page.clock.setFixedTime` で固定）。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:programs-bottom-nav
```

### 横向き端末の左右セーフエリア（`safe-area-inline.mjs`）

CDP の `Emulation.setSafeAreaInsetsOverride` で `env(safe-area-inset-*)` に非ゼロの値を入れ、`getBoundingClientRect` の寸法だけで判定する。CSS の文字列一致は見ない。

- 1280×800 と 390×844（inset 0）: inset が 0 のときレイアウトが変わらないこと
- 844×390（左右 59・下 21）: 横向き iPhone。`md` 以上なのでサイドバー配置になり、サイドバー左端が 59、本文・ヘッダー右端が 785（844-59）、ボトムタブは非表示
- 700×390（左右 47）: `md` 未満の横向き。本文・ヘッダーが 47..653、ボトムタブの `ul` も 47..653

override が効かなかった場合（CDP メソッドの欠落など）は、無視して通さず ng を積む。未検証: 実機の Safari が返す inset の実値と、回転時の再計算。横向き iPhone での確認は実機で行う。

```sh
pnpm build && pnpm preview --host 127.0.0.1 --port 4173 --strictPort &
E2E_URL=http://127.0.0.1:4173 pnpm e2e:safe-area-inline
```

### 固定ヘッダーとページ内 z-10 要素の重なり（`header-stacking.mjs`）

録画詳細のポスター上の「視聴済みにする」は `absolute z-10` を持つ。祖先に stacking context が無いので、`PageHeader` が同じ `z-10` だと DOM 順で後ろのボタンがヘッダーの手前に描かれる。録画詳細を 400px / 1280px 幅でスクロールし、ボタンの矩形をヘッダーの中央に重ねた位置で `document.elementFromPoint` がヘッダー内を返すことを実ブラウザで確認する。ボタンの祖先に stacking context が無いこと、判定点がボタン矩形に入っていること、ブレーカー帯とヘッダーが重ならないことは前提として検査し、成立しなければ落とす。DOM は書き換えない。録画一覧も同じ幅でスクロールし、ヘッダーが残りページ内要素と重ならないことを見る。jsdom は描画順と hit testing を測れない。

API をスタブするため mirakc と DB は要らない。①は `PageHeader` を `z-10` に戻すと落ちる。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:header-stacking
```

### 読み込み中のレイアウトシフト（`cls.mjs`）

CLS（Cumulative Layout Shift）はレイアウトそのものの指標なので、jsdom（`getBoundingClientRect()` が常に 0 を返す）では原理的に測れない。Lighthouse で検索に要改善域の CLS が出たことの唯一の判定手段になる。

ブラウザの Layout Instability API（`PerformanceObserver({type: 'layout-shift'})`）で `hadRecentInput === false` の `value` を単純合計する。**これは Lighthouse が実際に報告する CLS の近似であって同一ではない**。Lighthouse は session window でグルーピングしてその最大値を採るが、ここでは windowing をせず全期間の単純合計を見る。単純合計は session window の最大値より大きくなることしかないので、ここで 0.10 以下なら Lighthouse の値も 0.10 以下になる。逆方向の保証はしない、未検証。

見るのは検索（`/search`。`components/condition-fields.tsx`）の 2 点:

- ① モバイル幅（390x844）でサービス一覧の取得を遅延させた状態（Lighthouse のスロットル下を模す）で読み込み中の CLS が 0.10 以下
- ② デスクトップ幅（1280x900）で①と同じ遅延を掛けた状態で 0.10 以下。ラボ計測は検索デスクトップも 0.087（しきい値の一歩手前）を報告しており、`ConditionFields` の対策の根拠はビューポート依存の議論（「押される側が折り目の外に出る」）なので、モバイルだけでは踏んでいない

検索画面の詳細条件は初期状態で閉じているが、`cls.mjs` は開閉ボタンを押してからサービス一覧を待つ。これで、サービス節が表示中の詳細フォームを非同期に押す従来のシナリオを保ったまま、初期表示を短くした変更の影響も測れる。

サービス数は 24 局（地上波 + BS + CS 相当）にしてある --- 2 局だけでは 390px でもチップが 1 行に収まってしまい、直す前の実装でも再現しない。

**サービス一覧の遅延（`NETWORK_DELAY_MS`）は 500ms より確実に大きくすること**。Chrome はクリック等の離散入力から 500ms 以内の layout-shift を全部 `hadRecentInput: true` にし、`installClsObserver` はそれを合計から除く。`measureSearch` は「詳細条件を表示」をクリックしてから遅延後にサービス一覧が届く。そのため遅延が 500ms 未満だと、届いたときのシフトが毎回この入力窓に収まって除外され、残留ノイズしか測れなくなる。

レビュー指摘では、旧 400ms で①の実測が 0.00066 → 0.000008 まで落ち、下記の変異検出が機能しなくなっていた。いまは 1500ms にしてある。直す前の `condition-fields.tsx`（`ServiceFields` が `TextMatchFields` の直後）を当てると①が 0.257（②は 0.039）で、①が実際に落ちることを確認済み。

**詳細節を開いた現在の実測は①が 約 0.03、②が 約 0.016。** 検索・クリア操作を全条件の後ろへ移した状態でも、どちらも 0.10 のしきい値を下回る。

**検索条件にサイトチップ（`SiteFields`）を足した（issue #531）後も測り直したが値は変わらない（今回の実測では①が 約 0.03、②が 約 0.016）**。この `SiteFields` はレジストリの解決した `GET /api/sites` のキャッシュを再利用する同期的な節である。しかも、レジストリと下書きの和集合が 2 つ以上のときしか描画しない。このフィクスチャは単一サイトかつ下書きが空なので DOM に一切増えない。

**ホーム（`/`）はここでは見ない**。ラボ計測はホームのデスクトップでも 0.111（スロットル時のみ）を報告している。原因は「4 セクションの表示順は固定なのに解決順は不定で、後続セクションが先に見えている状態で先行セクションが実データごと上に挿し込まれる」形である。対策には `docs/frontend/home.md`「セクションの可視性は個別に」を変える設計判断が要る。判定手段（この形のフィクスチャ）ごとその判断のあとに足す --- しきい値を超えたまま緑にできない判定を置くと、この受け入れ全体が「常に赤いので誰も見ない」ものになる。

`design.mjs` と同じ手（`/api/**` を `page.route` で丸ごと差し替え）で mirakc も DB も要らない。⓪（配っている bundle と `dist/` の一致）も自分で確認する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:cls
```

### 畳んだサイドバーの項目名（`sidebar-tooltips.mjs`）

1280px の Chromium で畳んだレールをホバーし、初回の表示が 250〜500ms に入ることを確認する。隣接項目は 180ms 以内に出る。Tab フォーカスでも表示し、展開後は両操作で表示しない。リンクの読み上げ名が 1 件で、tooltip が `aria-describedby` に結ばれていないことも調べる。API は Playwright で差し替えるため、サーバーと DB は要らない。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:sidebar-tooltips
```
