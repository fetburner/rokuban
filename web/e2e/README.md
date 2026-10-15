# 実ブラウザでの受け入れ確認

**jsdom が測れないもの（レイアウト・スクロール位置・可視判定・色）を判定するための道具。**

`pnpm test`（Vitest + jsdom）はレイアウトを計算しない。`getBoundingClientRect()` は常に 0 を返し、`IntersectionObserver` も無い。したがって**スクロール位置・要素の可視性・レイアウトシフトに関する機能は、ユニットテストが全部通っても何の保証にもならない**。

この規律は番組リストの遡行（前の時間窓をリスト先頭に差し込んで、見ている位置を保つ機能。**現在は廃止済みで存在しない**）で生まれた。実際に **「テストが通った」を根拠に 3 回リリースして 3 回とも実機で正しく動いていなかった**。失敗の仕方はそれぞれ違った。

1. `document.documentElement.scrollHeight` の差分で補正 → 差し込み直後の高さは見積もりで、実測が後から届いて再びずれる
2. DOM のアンカー要素を取得して位置を合わせる → **仮想化では差し込んだ瞬間にその要素が DOM から消える**ので、補正が一度も走らない
3. `scrollToIndex` で復元 → ボタンがリスト最上部（画面外）にあり、押すためのスクロールがアンカーの記録より先に走って、記録する行が変わっていた

いずれも jsdom では**原理的に検出できない**。ここに置いた判定はそのためのもの。

`lib.mjs` は各スクリプトの前置きを共有する。中身はブラウザの起動・終了、`/api/**` の配線、配っている bundle が `dist/` の現物と一致するかの確認、結果の集計と終了コードである。あわせてフィクスチャが orval 生成の zod スキーマと一致するかの確認（`validateFixturesOrExit`。詳細は [checks/design.md](checks/design.md) §デザイン）も持つ。各スクリプト固有の判定（何が OK/NG かの基準）はここには置かず各 *.mjs 本体にとどめる --- フィクスチャ自体（どの値を使うか）はスクリプトごとに違うためである。`validateFixturesOrExit` は「フィクスチャ配列 → 呼び出し側が組む」形にして判定ロジックだけを共有する。

## 使い方

ブラウザは初回だけ取得する。

```sh
pnpm install
pnpm exec playwright install chromium webkit  # webkit は live.mjs の⑥に要る
```

サーバーを起動しておく（`go:embed` なので **web を変更したらバイナリを作り直す**こと。`docs/runbook.md` 参照）。

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

チャンネル選択と「その他」のモバイルシート、録画・シリーズ絞り込み内のチャンネル選択は次で判定する。デスクトップのチャンネル選択も合わせて確認する。

```sh
cd web && E2E_URL=http://localhost:40773 node e2e/issue-1224-sheet.mjs
```

### フィクスチャ契約の CI 検証

各スクリプトが使う API フィクスチャと `web/src/api/zod.ts` の生成スキーマの一致だけは、実ブラウザを使わずに `pnpm e2e:fixtures` で検証する。これは `E2E_VALIDATE_FIXTURES_ONLY=1` を付けて契約検証を持つスクリプトを起動し、検証後に各プロセスを終了する経路である。preview サーバー、`dist/`、Playwright のブラウザ本体は必要ない。

対象スクリプトは `e2e/validate-fixtures.mjs` が手書きの一覧ではなくファイル内容（`validateFixturesOrExit` の呼び出しの有無）から導出する。手書きだと、並行して増えたスクリプトの契約検証が一覧への追加漏れで気付かないうちに検査対象から外れる。各スクリプトはこのモードで全フィクスチャを検証してから `launchBrowser` や bundle 検証へ進まない。実ブラウザを使う判定はこのコマンドの対象外である。CI では browser-e2e ジョブが一部だけ回し、残りはローカルの個別 E2E で行う（下記 §CI で回す判定とそれ以外）。子プロセスは順番にすべて実行するので、先のスクリプトが失敗しても後続のフィクスチャ検証を省略しない。

**`E2E_VALIDATE_FIXTURES_ONLY=1` をシェルに export したまま忘れると、以降の `pnpm e2e:*` は全部「契約検証だけして exit 0」になる。実判定を 1 つも走らせていない緑になる**。このコマンドを使うときはコマンドの前にだけ付ける。

## 節 → ファイル

| 節 | ファイル |
|---|---|
| フィクスチャ契約の CI 検証 | [README.md](README.md) |
| 多 site 番組表（`multi-site.mjs`） | [checks/programs.md](checks/programs.md) |
| 正時をまたぐグリッド再取得（`programs-grid-hour-boundary.mjs`） | [checks/programs.md](checks/programs.md) |
| 参照バッジのリンク（`badge-links.mjs`） | [checks/programs.md](checks/programs.md) |
| シリーズ一覧の分割（`shelves-split.mjs`） | [checks/programs.md](checks/programs.md) |
| ライブ視聴（`live.mjs`） | [checks/live.md](checks/live.md) |
| 録画中の追っかけ再生（`chase.mjs` / `chase-audio.mjs`） | [checks/playback.md](checks/playback.md) |
| 録画原本 HLS と encoded の再生時刻（`recording-playback-timeline.mjs`） | [checks/playback.md](checks/playback.md) |
| ホームの保存位置フレーム（`home-resume-frame.mjs`） | [checks/layout.md](checks/layout.md) |
| 原本 HLS VOD の再生開始（`recording-original-vod.mjs`） | [checks/playback.md](checks/playback.md) |
| 原本 HLS から encoded への切替（`recording-original-vod.mjs`） | [checks/playback.md](checks/playback.md) |
| デザイン（`design.mjs`） | [checks/design.md](checks/design.md) |
| README のスクリーンショット（`readme-screenshots.mjs`） | [checks/design.md](checks/design.md) |
| フォントスケール（`font-scale.mjs`） | [checks/design.md](checks/design.md) |
| SSE 抜きでの定期再取得・接続断バナー（`sse-refresh.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 番組リストの操作列（`reserve-visibility.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 番組表グリッドの空間ナビゲーション（`grid-navigation.mjs`） | [checks/programs.md](checks/programs.md) |
| 番組表グリッドの予約済み印（`grid-reserved.mjs`） | [checks/programs.md](checks/programs.md) |
| 番組表セルの操作モーダル（`programs-dialog.mjs`） | [checks/programs.md](checks/programs.md) |
| ボトムタブの高さと本文の下パディング（`programs-bottom-nav.mjs`） | [checks/layout.md](checks/layout.md) |
| 横向き端末の左右セーフエリア（`safe-area-inline.mjs`） | [checks/layout.md](checks/layout.md) |
| ライブ画面の番組境界（`live-boundary.mjs`） | [checks/live.md](checks/live.md) |
| 番組リストの空時間窓（`programs-empty-window.mjs`） | [checks/programs.md](checks/programs.md) |
| 検索フォームの初画面と詳細条件（`search-mobile.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 検索画面の操作位置・Enter・空の状態（`issue-1232-search.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 予約一覧の副情報がシェブロンに重ならないか（`reservations-mobile.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 予約一覧のシリーズ表示（`reservations-series.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 固定ヘッダーとページ内 z-10 要素の重なり（`header-stacking.mjs`） | [checks/layout.md](checks/layout.md) |
| 予約一覧の日付・出自・ルール絞り込み（`reservations-rule-filter.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 予約一覧の容量確認失敗（`reservations-capacity-error.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 予約一覧取得失敗時の番組表レイアウト（`programs-reservation-error.mjs`） | [checks/programs.md](checks/programs.md) |
| 読み込み中のレイアウトシフト（`cls.mjs`） | [checks/layout.md](checks/layout.md) |
| チップの横あふれ（`chip-overflow.mjs`） | [checks/design.md](checks/design.md) |
| 個人化の持続（`personalization.mjs`） | [checks/design.md](checks/design.md) |
| シークバーのプレビュー（`seek-tiles.mjs`） | [checks/playback.md](checks/playback.md) |
| CM 検出のロゴ位置（`cm-logo-area.mjs`） | [checks/recordings.md](checks/recordings.md) |
| チャプターの目盛りと自動スキップ（`chapters.mjs`） | [checks/playback.md](checks/playback.md) |
| 次のエピソード・終端カード・棚（`recording-next-episode.mjs`） | [checks/recordings.md](checks/recordings.md) |
| 番組表の表示形式（`programs-view.mjs`） | [checks/programs.md](checks/programs.md) |
| 表示切替の形（`issue-1229-switches.mjs`） | [checks/programs.md](checks/programs.md) |
| 番組表の短い番組選択（`programs-grid-zoom.mjs`） | [checks/programs.md](checks/programs.md) |
| 録画一覧の絞り込みパネル（`recordings-rule-filter.mjs`） | [checks/recordings.md](checks/recordings.md) |
| 録画一覧のツールバーと期間（`recordings-period-toolbar.mjs`） | [checks/recordings.md](checks/recordings.md) |
| 検索欄の `/` フォーカス（`search-shortcut.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 畳んだサイドバーの項目名（`sidebar-tooltips.mjs`） | [checks/layout.md](checks/layout.md) |

読む順の目安:

- **判定を足す** → 冒頭・§使い方・§判定を足すときの規律を読んでから、近い画面のファイルを開く
- **番組表（グリッド・番組リストの空時間窓）** → [checks/programs.md](checks/programs.md)（⓪ の説明もここ）
- **再生（追っかけ・原本 VOD・シークバー・チャプター）** → [checks/playback.md](checks/playback.md)
- **ライブ視聴** → [checks/live.md](checks/live.md)
- **色・トークン・スクリーンショット・フォントスケール** → [checks/design.md](checks/design.md)
- **ヘッダー・ボトムタブ・セーフエリア・CLS** → [checks/layout.md](checks/layout.md)
- **予約・検索・番組リストの操作列** → [checks/reservations-search.md](checks/reservations-search.md)
- **録画一覧・次のエピソード・CM 検出** → [checks/recordings.md](checks/recordings.md)

## 外部参照される節名

| 参照される節名 | 分割後の場所 |
|---|---|
| 冒頭 | [README.md](README.md) |
| jsdom が測れないもの | [README.md](README.md) |
| 判定を足すときの規律 | [README.md](README.md) |
| デザイン | [checks/design.md](checks/design.md) |
| ⓪ 配っている bundle と dist/ の一致（lib.mjs・checks.mjs・live.mjs・design.mjs の「README.md 参照」） | [checks/programs.md](checks/programs.md) §参照バッジのリンク |

## CI で回す判定とそれ以外

CI の `browser-e2e` ジョブは、実バイナリが `go:embed` した `dist/` を配るサーバーへ Chromium を向ける。回す判定の一覧は `.github/workflows/ci.yml` の `browser-e2e` ジョブが権威で、ここには写さない。各判定は 1 本が落ちても残りを走らせる。選定基準は次の 3 つを全部満たすことである。

- jsdom が原理的に測れない（レイアウトシフト・幅の溢れ・スクロール余白・viewport への収まり・実画素コントラスト）
- `/api/**` を Playwright 内でスタブし、mirakc・チューナー・実データに依存しない
- ffmpeg・webkit・DB への直接書き込みを要らず、Chromium だけで軽く終わる

それ以外の判定（`lib.mjs` と `validate-fixtures.mjs` は共有部品）は**ローカルでの受け入れ確認**の位置づけである。[docs/frontend.md](../../docs/frontend.md) の「受け入れは実機で行う」に実行可能な形を与えるものだ。回さない理由は 3 類型ある。

- 実メディアが要る: ffmpeg でフィクスチャを作る。対象は `chapters` / `home-resume-frame` / `seek-tiles` / `subtitles` / `recording-next-episode` / `chase` / `live` / `live-audio`。`home-resume-frame` / `live` / `live-audio` は webkit も要る
- 実 DB の状態が要る: `checks`（既定の `pnpm e2e`）は API をスタブせず実 EPG の番組行の描画を待つ。`live` は `epg_services` に実サービスの行が要り、`shelves-split` は `E2E_DATABASE_URL` の DB を TRUNCATE する
- 残りの画面別判定（番組表・予約・検索など）は API スタブで技術的には載せられる。ただし全体の所要時間を測っておらず、毎 PR に払う価値をまだ判断していない。`design.mjs` は 63 枚のショットを撮るぶん重い

判定スクリプト全部を回す定期ジョブは作らない。回す主体と失敗の受け手が決まっておらず、誰も見ない赤い定期ジョブは PR ごとに回す一部より信号として弱いためである。対象を増やすときは `.github/workflows/ci.yml` のコメントとこの節の類型を同じ PR で直す。実ブラウザ不要の `pnpm check:colors` は lint job に入っている。

## 判定を足すときの規律

**足した判定が、直す前の実装で実際に落ちることを確認すること。** 落ちない判定は何も判定していない（CLAUDE.md「テスト規律」のユニットテストと同じ）。

**時計を固定した判定は、時計が動くことに起因する欠陥を検出できない。** `page.clock.setFixedTime`（`checks/` の各判定）も jsdom の `vi.setSystemTime`（`pnpm test`）も時計を止めている。その代償として、時計が動くことで露呈する欠陥はどちらの経路でも原理的に見えない。たとえば「レンダーのたびに変わる値（生の `Date.now()` 等）をキャッシュキーに載せて無限再取得になる」欠陥は、時計を止めた構成では再現しない（`pages/home.tsx` の容量超過クエリで実際に踏んだ）。時計に起因する挙動を判定したいときは、時計を止めない経路を別に用意すること（`design.mjs`・`pages/home.test.tsx` の「実時計でのクエリキー安定性」参照）。

**`waitFor` の失敗を `.catch(() => {})` で飲むときは、後段が飲んで良い理由になっているかを確かめる**。後段が「待っていた条件そのもの」を直接読み直して合否を出す形（例: `document.activeElement === el` を評価し直す）なら、待ちの成否を経由しないので飲んでよい。

そうでない形（後段が別の要素・別の状態を測る）で飲むと、「待ちが失敗した」ことがその別の判定の NG 文言に化けて出る --- スタイル回帰と区別が付かず、しかもタイミング依存で再実行すると消える。この規律自体はまだこのリポジトリで踏み抜いたことを実測で示せていない（予防的なもの）--- 下記 ChannelOption の一件は、調べる過程でこの形の失敗ではないと分かった。

**before/after diff を取る共有ヘルパーは、before の基準を自分で制御しないと正しく動かない**。`design.mjs` の `checkExplicitFocusRing`（ChannelOption 呼び出し）で実際に起きた。base-ui の Popover は開いた直後、先頭候補へ非同期に既定フォーカスを当てる（queueMicrotask → requestAnimationFrame 1 回）。この既定フォーカスが「まだ来ていない」か「もう来た」かで before の box-shadow が実行のたびに割れ、before/after の差分判定（before と after が同じなら NG）が偽陽性を出していた。

ポップアップの開閉待ち自体は 5000ms の予算に対し 1〜3ms で毎回成功しており、swallow とは無関係だった（4 回計測、8/8 で `beforeShadow !== 'none'` と NG が一致）。直すには before を測る前に明示的に `blur()` して基準を確定させ、かつ相手の既定フォーカスが済むのを待ってから呼ぶ（rAF 2 回。フレーム数は負荷で増えないので固定回数で足りる）。
