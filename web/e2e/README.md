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

通常の E2E 判定は各スクリプトの `/api/**` をフィクスチャへ差し替えるため、実 EPG の状態は使わない。ただし、⓪で配信中の bundle と `dist/` の一致を確かめるため、`dist/` を埋め込んだ API サーバーが要る。スクリプトごとの既定 URL は揃っていないため、まとめて回すときは `E2E_URL` で接続先を指定する。

```sh
# リポジトリルートで実行。PostgreSQL を起動し、dev.local.yml の DB にマイグレーションを適用する。
(cd web && pnpm build)
go build -o /tmp/rokuban-e2e ./cmd/rokuban
/tmp/rokuban-e2e migrate up --config dev.local.yml
/tmp/rokuban-e2e server --roles api --config dev.local.yml
```

別のターミナルで `E2E_URL` をサーバーに合わせて判定する。CI と同じく、Go バイナリは直前に build した `web/dist/` を配る。**web を変更したらバイナリを作り直す**こと（`docs/runbook.md` 参照）。

```sh
cd web
E2E_URL=http://localhost:40773 node e2e/run.mjs
E2E_URL=http://localhost:40773 node e2e/run.mjs chapters
```

実データに対して `checks.mjs` を回すときは、実 EPG の番組がある同じ API サーバーを使い、次を実行する。

```sh
cd web
node e2e/run.mjs checks
```

録画 HLS の再起動境界は、合成 TS から作った HLS を実ブラウザで再生して判定する。判定の中身と環境変数は [checks/playback.md](checks/playback.md) にある。

```sh
cd web
node e2e/run.mjs recording-segmented-vod                      # Chrome。ffmpeg が要る
E2E_BROWSER=webkit node e2e/run.mjs recording-segmented-vod   # 診断用
```

チャンネル選択と「その他」のモバイルシート、録画・シリーズ絞り込み内のチャンネル選択は次で判定する。デスクトップのチャンネル選択も合わせて確認する。

```sh
cd web && E2E_URL=http://localhost:40773 node e2e/run.mjs channel-and-more-sheet
```

### フィクスチャ契約の CI 検証

各スクリプトが使う API フィクスチャと `web/src/api/zod.ts` の生成スキーマの一致だけは、実ブラウザを使わずに `pnpm e2e:fixtures` で検証する。これは `E2E_VALIDATE_FIXTURES_ONLY=1` を付けて契約検証を持つスクリプトを起動し、検証後に各プロセスを終了する経路である。preview サーバー、`dist/`、Playwright のブラウザ本体は必要ない。

対象スクリプトは `e2e/validate-fixtures.mjs` が手書きの一覧ではなくファイル内容（`validateFixturesOrExit` の呼び出しの有無）から導出する。手書きだと、並行して増えたスクリプトの契約検証が一覧への追加漏れで気付かないうちに検査対象から外れる。各スクリプトはこのモードで全フィクスチャを検証してから `launchBrowser` や bundle 検証へ進まない。実ブラウザを使う判定はこのコマンドの対象外である。ブラウザ判定の CI 対象は `run.mjs` が各スクリプトの要件宣言から導出する（下記 §CI 判定の要件宣言）。子プロセスは順番にすべて実行するので、先のスクリプトが失敗しても後続のフィクスチャ検証を省略しない。

**`E2E_VALIDATE_FIXTURES_ONLY=1` をシェルに export したまま忘れると、以降の判定は全部「契約検証だけして exit 0」になる。実判定を 1 つも走らせていない緑になる**。`run.mjs` はこの変数を子プロセス環境から取り除く。フィクスチャ検証を使うときはコマンドの前にだけ付ける。

## 節 → ファイル

### 番組表・ライブ

番組表・参照バッジ・シリーズ一覧・ライブ視聴の判定と、フィクスチャ契約の検証をまとめる。

| 節 | ファイル |
|---|---|
| フィクスチャ契約の CI 検証 | [README.md](README.md) |
| 多 site 番組表（`multi-site.mjs`） | [checks/programs.md](checks/programs.md) |
| 正時をまたぐグリッド再取得（`programs-grid-hour-boundary.mjs`） | [checks/programs.md](checks/programs.md) |
| 参照バッジのリンク（`badge-links.mjs`） | [checks/programs.md](checks/programs.md) |
| シリーズ一覧の分割（`shelves-split.mjs`） | [checks/programs.md](checks/programs.md) |
| ライブ視聴（`live.mjs`） | [checks/live.md](checks/live.md) |

### 録画再生

録画再生とメディア配信の判定は [checks/playback.md](checks/playback.md) にある。

| 節 | ファイル |
|---|---|
| LivePlayer の 503 文言・離脱ヒント（`live-player-modes.mjs`） | [checks/playback.md](checks/playback.md) |
| 録画 HLS の時間軸・世代切替・再開（`recording-playback-timeline.mjs`） | [checks/playback.md](checks/playback.md) |
| 録画 HLS の再起動境界（`recording-segmented-vod.mjs`） | [checks/playback.md](checks/playback.md) |
| S3 の署名付き URL への 302（`s3-302.mjs`） | [checks/playback.md](checks/playback.md) |

### その他の判定

画面操作やレイアウトなど、再生以外の確認項目をまとめる。

| 節 | ファイル |
|---|---|
| ホームの保存位置フレーム（`home-resume-frame.mjs`） | [checks/layout.md](checks/layout.md) |
| デザイン（`design.mjs`） | [checks/design.md](checks/design.md) |
| README のスクリーンショット（`readme-screenshots.mjs`） | [checks/design.md](checks/design.md) |
| フォントスケール（`font-scale.mjs`） | [checks/design.md](checks/design.md) |
| SSE 抜きでの定期再取得・接続断バナー（`sse-refresh.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 置き場別ブレーカーの再開（`circuit-breaker-store.mjs`） | [checks/circuit-breakers.md](checks/circuit-breakers.md) |
| 番組リストの操作列（`reserve-visibility.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 番組表グリッドの空間ナビゲーション（`grid-navigation.mjs`） | [checks/programs.md](checks/programs.md) |
| 番組表グリッドの予約済み印（`grid-reserved.mjs`） | [checks/programs.md](checks/programs.md) |
| 番組表セルの操作モーダル（`programs-dialog.mjs`） | [checks/programs.md](checks/programs.md) |
| ボトムタブの高さと本文の下パディング（`programs-bottom-nav.mjs`） | [checks/layout.md](checks/layout.md) |
| 横向き端末の左右セーフエリア（`safe-area-inline.mjs`） | [checks/layout.md](checks/layout.md) |
| ライブ画面の番組境界（`live-boundary.mjs`） | [checks/live.md](checks/live.md) |
| 番組リストの空時間窓（`programs-empty-window.mjs`） | [checks/programs.md](checks/programs.md) |
| 検索フォームの初画面と詳細条件（`search-mobile.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 検索画面の操作位置・Enter・空の状態（`search-form-interactions.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
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
| 表示切替の形（`program-view-switches.mjs`） | [checks/programs.md](checks/programs.md) |
| 番組表の短い番組選択（`programs-grid-zoom.mjs`） | [checks/programs.md](checks/programs.md) |
| 録画一覧の絞り込みパネル（`recordings-rule-filter.mjs`） | [checks/recordings.md](checks/recordings.md) |
| 録画一覧のツールバーと期間（`recordings-period-toolbar.mjs`） | [checks/recordings.md](checks/recordings.md) |
| 検索欄の `/` フォーカス（`search-shortcut.mjs`） | [checks/reservations-search.md](checks/reservations-search.md) |
| 畳んだサイドバーの項目名（`sidebar-tooltips.mjs`） | [checks/layout.md](checks/layout.md) |

読む順の目安:

- **判定を足す** → 冒頭・§使い方・§判定を足すときの規律を読んでから、近い画面のファイルを開く
- **番組表（グリッド・番組リストの空時間窓）** → [checks/programs.md](checks/programs.md)（⓪ の説明もここ）
- **再生（録画 HLS・シークバー・チャプター）** → [checks/playback.md](checks/playback.md)
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

## CI 判定の要件宣言

`run.mjs` は `e2e/` の実行スクリプトをファイルから探し、冒頭コメントに `// e2e-needs:` が無いものを CI で回す。新しい判定は既定で対象になる。各スクリプトは独立した Node プロセスで起動し、失敗後も続けて所要時間付きの結果表を出す。CI では `$GITHUB_STEP_SUMMARY` にも同じ表を追記する。スクリプト名を引数に渡すと、その 1 本だけを要件に関係なく実行できる。

全対象を 1 ジョブで回すと CI 全体の待ち時間が延びるため、`--shard i/n` で対象を分ける。判定時間には偏りがあるので、重い判定が同じ shard に寄らないようソース byte 数を実行時間の代理指標として決定的に割り当てる。shard 数は各 shard の step summary に出る実測表を見て、判定時間が CI の他の最長経路を超えないように決める。

`package.json` に個々のファイルを指す `e2e:*` 別名は置かない。別名の並びは対象一覧を重複して持ち、追加漏れも起こしていたため、個別実行はこの runner に寄せる。`e2e:fixtures` は browser 判定とは別の契約検証をまとめるため残す。

追加要件の宣言は、browser-e2e が用意しない環境だけに使う。

- `webkit`: 判定に Playwright WebKit が必要
- `db-state`: 実 DB の既存状態または直接書き込みが必要
- `s3`: RustFS などの S3 サービスが必要。専用ジョブは `node e2e/run.mjs --needs s3` で対象を選ぶ
- `manual`: 通常の CI 判定に入れず、必要なときだけ手動で起動する。宣言の隣に理由を書く

ffmpeg と Go は browser-e2e が既に用意するので宣言しない。`E2E_BROWSER=webkit` など任意の診断モードを持つだけなら追加要件ではない。`lib.mjs` などの共有部品は別スクリプトからの import で除く。`finish(` を呼ばない実行判定があり、実行判定ではない fixture builder のような部品もあるため、`finish(` の有無では判別しない。

CI は `design.mjs` の全体を実行する。部分実行だけでは状態色・画素・タイムラインの判定が欠けるためである。`E2E_RULE_CARD_ONLY=1` はルールカードだけを調べるローカル診断用で、CI では使わない。実ブラウザ不要の `pnpm check:colors` は lint job に入っている。判定スクリプト全部を回す定期ジョブは、回す主体と失敗の受け手が決まっていないため作らない。

## 判定を足すときの規律

**足した判定が、直す前の実装で実際に落ちることを確認すること。** 落ちない判定は何も判定していない（CLAUDE.md「テスト規律」のユニットテストと同じ）。

**時計を固定した判定は、時計が動くことに起因する欠陥を検出できない。** `page.clock.setFixedTime`（`checks/` の各判定）も jsdom の `vi.setSystemTime`（`pnpm test`）も時計を止めている。その代償として、時計が動くことで露呈する欠陥はどちらの経路でも原理的に見えない。たとえば「レンダーのたびに変わる値（生の `Date.now()` 等）をキャッシュキーに載せて無限再取得になる」欠陥は、時計を止めた構成では再現しない（`pages/home.tsx` の容量超過クエリで実際に踏んだ）。時計に起因する挙動を判定したいときは、時計を止めない経路を別に用意すること（`design.mjs`・`pages/home.test.tsx` の「実時計でのクエリキー安定性」参照）。

**`waitFor` の失敗を `.catch(() => {})` で飲むときは、後段が飲んで良い理由になっているかを確かめる**。後段が「待っていた条件そのもの」を直接読み直して合否を出す形（例: `document.activeElement === el` を評価し直す）なら、待ちの成否を経由しないので飲んでよい。

そうでない形（後段が別の要素・別の状態を測る）で飲むと、「待ちが失敗した」ことがその別の判定の NG 文言に化けて出る --- スタイル回帰と区別が付かず、しかもタイミング依存で再実行すると消える。この規律自体はまだこのリポジトリで踏み抜いたことを実測で示せていない（予防的なもの）--- 下記 ChannelOption の一件は、調べる過程でこの形の失敗ではないと分かった。

**before/after diff を取る共有ヘルパーは、before の基準を自分で制御しないと正しく動かない**。`design.mjs` の `checkExplicitFocusRing`（ChannelOption 呼び出し）で実際に起きた。base-ui の Popover は開いた直後、先頭候補へ非同期に既定フォーカスを当てる（queueMicrotask → requestAnimationFrame 1 回）。この既定フォーカスが「まだ来ていない」か「もう来た」かで before の box-shadow が実行のたびに割れ、before/after の差分判定（before と after が同じなら NG）が偽陽性を出していた。

ポップアップの開閉待ち自体は 5000ms の予算に対し 1〜3ms で毎回成功しており、swallow とは無関係だった（4 回計測、8/8 で `beforeShadow !== 'none'` と NG が一致）。直すには before を測る前に明示的に `blur()` して基準を確定させ、かつ相手の既定フォーカスが済むのを待ってから呼ぶ（rAF 2 回。フレーム数は負荷で増えないので固定回数で足りる）。
