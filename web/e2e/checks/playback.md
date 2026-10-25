> [web/e2e/README.md](../README.md) の一部。索引から辿る

### 録画中の追っかけ再生（`chase.mjs` / `chase-audio.mjs`）

録画詳細を `#chase` で開き、実 H.264/AAC セグメントを返す成長中の EVENT playlist を Chromium の hls.js（`E2E_BROWSER=webkit` ならネイティブ HLS）で再生する。録画 API・追っかけ HLS・離脱 API は `page.route` で差し替えるため mirakc と実録画は要らない。次を実ブラウザで見る。

`pnpm e2e:chase` は `chase.mjs` の後に `chase-audio.mjs` も実行する。後者は ffmpeg で標準 / 主 / 副の rendition を含む EVENT master を作り、Chromium では WebAudio の左右周波数で切替を測る。各トラックを 15 秒聴いて戻った後も再生が進むこと、同じ master 形式での範囲内 seek・offset 張り直し・保存位置からの開始を確認する。WebKit では native `audioTracks` と再生継続を測る。

- 録画中の詳細ページから追っかけプレイヤーが開き、`<video>` に `controls` が無く、時間軸は操作バーのシークバー 1 本である
- EVENT playlist がセグメントを増やしても再生位置は録画先頭から始まる
- セッションの範囲内へのシークは playlist を取り直さない。ドラッグ中は張り直さない（要求回数で見る）
- 変換済みの端より先・開始 offset より前へのシークは、その秒の offset で張り直す。先頭セグメントがその秒のものであること、直前のセッションの leave ヒント（`/offset/{前の offset}/leave` まで）を見る
- **offset のセッションから 0 秒へ戻すと、offset 無しの playlist に張り直し、保存位置を復元しない**。パネルで 0 秒を「未選択」にしてしまう変異で、保存位置の 5 秒へ戻って落ちる
- 録画の先端の印と End キーで先端へ移る。範囲内か張り直しかは実装から予測しない。最後に要求した playlist の offset + `currentTime` が選んだ位置に落ち着くことを見る。予測すると、変換済みの端が読み込みに応じて伸びるため 10 回中 7 回落ちた
- 再生中に張り直しても再生を続け、選んだ位置から進む
- バー下の目盛り: 延長中は「予定 … ¦」の ¦ が予定終端の印を、録画済みが短いときは先端の時刻の中心が先端の印を指す（許容 3px）。印が左端に近いときはラベルが行の中に寄り、ラベル同士が重ならない。先端の印の下は時刻だけで、「録画の先端 …（押すと先端へ）」は印の title にある
- 保存済みの VOD 共通速度が追っかけの `<video>` に適用され、`ratechange` が同じ `localStorage` キーへ保存される
- 成長後の playlist が `ENDLIST` になり、完了した録画を追える
- **画質（プロファイル）の切替で再生位置が続く**（issue #874）。先頭再生と offset 付き再生の 2 通りを見る。**`LivePlayer` を `key={profile}` で作り直す変異で offset 付きが 1.5 秒 → 0 秒に巻き戻って落ちる**（先頭再生は保存位置が近いので差が出にくい）。あわせて切替が離脱ヒントを送らないこと・`?profile=` が要求に載ることも見る
- **画質の切替の途中で「続きから」を別の位置で上書きしない**。途中で書かれた値をすべて記録して見る（最終値は切替後の seek で正しい値に戻るため）。2 秒未満は `removeItem` になるので、それも 0 として記録する。持ち越し中の保存ガードを外すと、WebKit では先頭再生で `0`、offset 4 秒の切替で `4` が書かれて落ちる。Chromium + hls.js では `timeupdate` が来ないので落ちない
- **追っかけで見た位置から、完了後の VOD を開くと同じ位置 ± 3 秒から始まる**（⑧、issue #975 受け入れ 2）。追っかけの保存位置（スタブ API が保持）を読み、録画を完了に切り替えて `/recordings/1` を開き直し、原本 VOD の `currentTime` を比べる。**原本 VOD の `LivePlayer` に `resumePositionMs` を渡さない変異で `5.52 秒 → 0.00 秒` になって落ちる**（Chromium で確認。WebKit は未実施）
- **追っかけ範囲外 seek で原本 HLS に移った後も再生が続く**（⑩）。最後に進んだ `currentTime` から次に進んだ時刻までの最大停止時間を記録し、2 秒以下を判定する。再生を引き継がない変異は、範囲外 seek の `reselectPlaybackSource` へ `playing && false` を渡すものである。**停止 10073ms・`advances=0` になって落ちる**（Chromium で確認）。**再生元を替えない変異（`selected !== current.source` を `false &&` にする）で、offset を要求せず ⑩ の 4 判定が落ちる**（Chromium で確認）
- **追っかけの終端（`ended`）では、別の再生元を作らず終了状態のまま止まる**（⑪・⑫）。追っかけの `ENDLIST` は mirakc の録画が終わった後にだけ付くので、終端は常に録画ファイルの終端である（`docs/api/media.md`）。fixture は伸びる EVENT playlist を 6 segment（12 秒）で止め、そこへ `ENDLIST` を付ける。壁時計の録画時間は 20 秒、原本 HLS fixture は 20 秒以上ある。⑪ は完了の取得の後に、⑫ は前に `ENDLIST` を付ける。**main の実装（壁時計の終端の 1.5 秒手前より前なら移る）で ⑪ が `sameVideo:false`・原本 HLS 要求 2 件になって落ちる**。**前回の実装（5 秒の許容と、完了の取得を待つ保留）では ⑪ と ⑫ の両方が同じ形で落ちる**（以上 Chromium で確認）。

  終端の 1 秒後の `ended` は判定に使わない。Playwright の WebKit は `ended` の約 0.9 秒後に `durationchange` だけを出し、`currentTime` を 0 に戻すことがある。13 回の実行の 26 判定中 4 回。`seeking` / `loadstart` / `emptied` は出ない。次のポスター判定は⑬

`E2E_BROWSER=webkit` で同じ判定を Safari 相当のネイティブ HLS 経路で回す。画質切替の位置の持ち越しは hls.js（`startPosition`）とネイティブ（要素への代入）で経路が別なので、両方で回す。

フィクスチャ生成に `ffmpeg` を使う。無い環境ではこの判定だけを skip として終了し、他の CI 判定を失敗扱いにしない。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:chase
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:chase
```

### LivePlayer の離脱ヒントと追っかけの表示時刻（`live-player-modes.mjs`）

ライブ・追っかけ・原本 VOD の 3 モードで、短い実メディアを実ブラウザに再生させ、離脱ヒントの送信と追っかけの表示時刻を測る。mirakc と実録画は要らない。

`/api/**` と HLS は `page.route` で差し替える。fixture は ffmpeg が作った 20 秒の HLS で、Chromium の hls.js が再生する。jsdom には `navigator.sendBeacon` が無く fetch に落ちるため、離脱ヒントの実送信は実ブラウザでしか測れない。

- 各モードで `visibilityState` を `visible` にした `visibilitychange` は、leave を 1 件も増やさない。hidden の判定を外す変異（visible でも送る）を落とす
- `visibilityState` を `hidden` にした `visibilitychange` と `pagehide` は、そのモードの宛先へ POST を 1 件ずつ送る。宛先はライブが `/live/leave`、追っかけが `/chase/leave`、原本 VOD が続き位置の `/original-vod/offset/12/leave` である
- 原本 VOD は続き位置 12 秒の offset playlist を要求する
- 追っかけの表示時刻は、録画先頭（番組開始から録画開始までの秒）に実メディアの `currentTime` を足した値と 2 秒以内で一致する。`aria-valuenow` は映像が進む前は props から導いた値で、実装と同じ式を読むので使わない

```sh
E2E_URL=http://localhost:4173 pnpm e2e:live-player-modes
E2E_VALIDATE_FIXTURES_ONLY=1 node e2e/live-player-modes.mjs
```

`E2E_VALIDATE_FIXTURES_ONLY=1` は録画 fixture を Zod 契約で検査するだけでブラウザを起動しない。CI の `browser-e2e` ジョブが実ブラウザ判定を回す。

### 録画原本 HLS と encoded の再生時刻（`recording-playback-timeline.mjs`）

Go テストで放送 TS に似せた MPEG-2 29.97 fps の fixture を作る。HLS は製品の `BuildOriginalVODFFmpegArgs`（offset 0 / 10 秒）で生成する。MP4 は製品の `BuildFFmpegArgs` と `config.example.yml` の h264 例で生成する。入力は先頭 PTS が 0 でない。音声を映像より約 700 ms 先に置く。不規則な間隔の9フレームに白い目印を入れ、位置で各フレームの ID を表す。期待時刻は、チャプターの軸である非カット MP4 を ffprobe して得た目印フレームの PTS とする。原本 TS の「目印 PTS - 最早 `start_time`」との差は参考値としてログにだけ出す。

Chrome の hls.js と WebKit のネイティブ HLS の両方で、原本 HLS offset 0、シークで張り直した offset 10 秒、非カット MP4 を再生する。各目印の表示を画素で検出し、その時点の `requestVideoFrameCallback` の `mediaTime` を読む。offset 付き HLS は入力 seek と同じ 30000/1001 のフレーム格子上の起点を足して原本時間軸へ戻し、フレーム番号の順序と 1ms 以内の差を記録する。`E2E_TIMELINE_EXPECTED_SHIFT_FRAMES=1` を付けると期待値を 1 フレームずらす変異確認になる。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-playback-timeline
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit E2E_TIMELINE_EXPECTED_SHIFT_FRAMES=1 pnpm e2e:recording-playback-timeline
```

この判定は ffmpeg / ffprobe / Go と Chromium / WebKit を使うため、これらが必要である。現在の合成 fixture では、非カット MP4 基準の差は Chromium + hls.js と WebKit native HLS の両方で、HLS offset 0 / 10 と MP4 の全目印が 0.00ms だった。

比較の許容差は半フレームではなく 1ms である。原本 HLS は B フレームを無効にし、`-ss` と再生軸を同じフレーム格子へ揃える。offset 10 秒の起点は 9.976633 秒なので、整数の 10 秒を足すと 23.37ms ずれる。表の再生時刻比較と編集モードの境界操作は別の判定であり、スクリプト全体の終了状態には両方が含まれる。この合成 fixture はソフトウェア libx264 に限る。実録画（GR/BS）とハードウェアエンコーダーは未測定である。測定条件と差の内訳は [`docs/frontend/recordings.md`](../../../docs/frontend/recordings.md) に記録している。

### 原本 HLS VOD の再生開始（`recording-original-vod.mjs`）

FFmpeg で MPEG-2 TS と H.264/AAC HLS の fixture を作り、録画 API と streamer の URL を `page.route` で差し替える。mirakc と実録画は要らない。⑤-manual は自動再生を意図的に `NotAllowedError` で拒否し、0 / 1 / 3 秒待ってから操作バーの ▶ を押す。fixture は EVENT playlist の先頭 6 segment から始まり、待ち時間中に1秒ごとに segment を増やす。

⑦ は原本 HLS と encoded の両方について 1280px / 400px で再生開始と枠の寸法を測る。クリック後 15 秒以内に video が一時停止状態を抜けて `currentTime > 0.5` になることを待つ。原本 HLS は先頭 4 segment の EVENT playlist から始め、2 秒ごとに segment を追加して最大 8 segment まで配る。

原本 HLS は開始位置も測る。クリック後に最初に取得した映像 segment が `0_seg00000.ts` で、最初の `playing` の `currentTime` が 1 未満でなければ NG にする。`currentTime > 0.5` だけでは、最新端から始まって進む再生も通ってしまうためである。診断には `playing` / `seeking` と `currentTime` への代入、詳細 API が返した再開位置を載せる。

前のページの再開位置 PUT は遷移の後に届くことがある。⑦ と ④ は書き込みの反映を止めて（`applyPositionWrites`）から `resumePositionMs` を消す。止めないと製品は残った位置から再開し、8 秒分の fixture の端で止まる。

④ で `video` が表示されない、または metadata が揃わない場合は、NG で終了する前に診断ログを出す。再生ボタンの `aria-label` と文言（`続きから` の有無）はクリック前に控える。クリックでポスターごと消えるからである。ログにはその控えと video の状態、詳細 API の `resumePositionMs`、再開位置 PUT の記録（`playbackPositionWrites`）を載せる。HLS playlist / segment の要求・HTTP 応答・失敗も同じログに含める。DOM の取得に失敗した場合も、その理由を記録して残りの診断を出す。

⑦が失敗した場合は待機エラー、`paused`・`currentTime`・`readyState`・`seekable`・`buffered` とメディアエラーを記録する。`play()` の呼び出し・成否、playlist / segment の要求と HTTP 応答も記録する。再生中の操作バーは 3 秒で隠れ `aria-hidden` / `inert` になる。手動でバーの操作を調べるときは、枠の上でマウスを動かして表示を待つ。

⑧〜⑪ は続きから再生を測る。到達位置は `offset の起点 + currentTime` で読む。起点は fixture の `-ss` と同じ `streamerSeekSeconds` で、製品の `originalVODSessionOriginSeconds` は使わない（同じ式で測ると、式が間違っていても通る）。aria-valuenow も使わない。最初の timeupdate までは props から導いた値だからである。

- ⑧: 変換済みが 2 秒しかない新しいセッションで、保存位置 4.5 秒から 1 秒以内の位置に到達する。
- ⑨: 保存位置 12 秒が 8 秒の EVENT playlist の先端より先にあっても、最初に offset 12 を開いて約 12 秒の位置に到達する。
- ⑩: 保存位置 61 秒が映像の終端（58.5 秒）より先でも、416 の後退丸めで端の近くから読み込む。
- ⑪: 保存位置より前への巻き戻しは、その秒の offset で張り直す。

直す前の実装（offset 0 に保存位置を渡す）では、⑨ ⑩ ⑪（Chromium・WebKit）と ⑧（WebKit）が落ちる。

```sh
E2E_URL=http://localhost:4173 pnpm e2e:recording-original-vod
```

### 原本 HLS から encoded への切替（`recording-original-vod.mjs`）

⑥ は原本 HLS 再生中に encoded が追加されても現在の HLS を保ち、次の範囲外 seek で encoded MP4 へ位置を渡す。切替後に `currentTime` が進むことと、最後に進んだ時刻からの最大停止時間が 2 秒以下であることも、Chromium と WebKit で確認する。再生を引き継がない変異は、⑩ と同じ `playing && false` である。**切替後が `paused:true`・停止 10077ms・`advances=0` になって落ちる**（Chromium で確認）。

切替の停止時間（上限 2000ms）の実測は、Chromium で ⑩ 88〜177ms（5 回）・⑥ 113〜165ms（3 回）、WebKit で ⑩ 202〜281ms（13 回）・⑥ 241〜310ms（3 回）だった。

⑬ は、カットなし encoded がある録画で版タブから原本 HLS を選び、保存位置より前へ巻き戻す。新しい offset の HLS playlist が要求され、encoded MP4 を要求しないことを確認する。

```sh
E2E_URL=http://localhost:4173 E2E_BROWSER=chromium pnpm e2e:recording-original-vod
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-original-vod
```

### 録画 HLS の再起動境界（`recording-segmented-vod.mjs`）

録画再生は、録画時刻上の固定長セグメントを合成 playlist で配り、位置が離れたら ffmpeg を 1 つ前の格子点から起動し直す方式である。この方式の合否は jsdom では測れない。止まらずに再生できるか、継ぎ目で音が切れないかを実ブラウザで判定する。アプリの実装より先に判定手段を置いている。

アプリは playlist を格子から先に合成するので、ffmpeg をどこで再起動したかを知らない。そのため再起動境界に `#EXT-X-DISCONTINUITY` を付けられない。内蔵フィクスチャも付けない。

`recording-segmented-fixture.mjs` が `os.tmpdir()` にフィクスチャを作る。素材の `source.ts` は 60 秒の合成 TS である。映像にフレーム番号を 11 bit の箱で焼き、音声は左 440 Hz・右 880 Hz の AAC とする。セグメントは 2 秒で、標準・主・副の 3 rendition を持つ。`master.m3u8` の BANDWIDTH は 20 Mbps にしてある。先読みの範囲は hls.js の設定 `maxBufferLength`（秒）・`maxBufferSize`（バイト）と BANDWIDTH で決まる。BANDWIDTH が 1.8 Mbps のときは 60 秒がすべて先に要求され、遠距離シークがバッファの外にならなかった。

- `direct` は ffmpeg 1 本で通して作る
- `restarted` は 12 秒の格子点まで作り、10 秒から起動し直した出力を `-output_ts_offset` 付きでつなぐ。重複する最初のセグメントは捨てる
- `off-grid`（負例）は、格子点ではない 11 秒から起動し直す
- `no-offset`（負例）は、正しい 10 秒から起動し直すが `-output_ts_offset` を付けない
- `audio-gap`（負例）は、再起動直後の標準音声へ AAC の 1 フレーム分（1024 samples、約 21 ms）の無音を入れる
- 映像セグメントの遅延（負例）は、境界の映像セグメントの応答を、再生位置が境界の手前に来るまで保留して作る。`stall` と呼ぶ

#### 判定

判定は 6 つある。

- 再生が止まらない。再生中に `currentTime` が 500 ms 以上進まない回数を数える
- 境界の ±0.5 秒で、焼いたフレーム番号が `mediaTime` と、`mediaTime` が `currentTime` と、それぞれ ±1 フレームで一致する。不一致は 1 枚でも落とす。ネイティブ HLS だけ、`mediaTime` と `currentTime` の許容を 2 フレームにする
- 内容が壁時計に対して飛ばず、戻らず、止まらない。連続する frame callback の間で、フレーム番号の進みが表示時刻の差 × 30 + 3 を超えない。callback の間隔が 250 ms を超えない。境界の ±0.5 秒に 20 枚以上が描かれる。`currentTime` ごと内容が飛ぶブラウザでは、marker・`mediaTime`・`currentTime` が一致したままになるので、この判定でしか見えない
- 境界の ±0.8 秒で、AudioWorklet が 128 サンプル（約 2.7 ms）ごとの左右 RMS を全サンプルについて求め、無音が 2 回続かない。標準音声の周波数が 440 Hz / 880 Hz のままである
- 主・副・標準の切替で、WebAudio の左右周波数が変わる
- 遠距離シークで、seeked の位置がシーク先 ±1 フレームである。再生を始めて、シーク先 ±0.5 秒のフレームが 3 枚目までに描かれる。そのうち最初のフレームの番号がシーク先 ±1 フレームで、先頭 5 枚のフレーム番号が自分の `mediaTime` と ±1 フレームで一致する。シーク先のセグメントをシーク前に要求していない（サーバーの要求ログで見る）

hls.js が non-fatal で報告する次の 4 つは、内容の欠けか停止の現れとして失敗にする。`bufferSeekOverHole`・`bufferNudgeOnStall`・`bufferStalledError`・`bufferAppendNoProgress` である。

負例は、期待する名前の失敗で落ちたときだけ「期待どおり」とする。名前は `[timecode]`・`[content-jump]`・`[frame-freeze]`・`[seek-frame]`・`[seek-buffered]`・`[audio-gap]`・`[stall]`・`[hls-gap]` のように、各失敗の先頭に付く。待ちが尽きて例外になったものは `[progress]` であり、検出とは数えない。期待する名前は次のとおりである。

- `off-grid` は `[timecode]`・`[content-jump]`・`[frame-freeze]` のどれか
- `no-offset` は `[seek-frame]`・`[content-jump]`・`[frame-freeze]` のどれか。ネイティブ HLS では正例として扱う（下の「ブラウザごとの違い」）
- `audio-gap` は `[audio-gap]`
- `stall` は `[stall]`

`no-offset` は、Chrome の hls.js では境界を通して再生するだけでは検出できなかった。`debug: true` のログに「AVC: 10000 ms overlapping between fragments」が出ており、hls.js が PTS の跳びを補正しているとみられる（内部の挙動であり契約ではない）。遠距離シークの後は、着地が 50 秒になり `bufferAppendNoProgress` が出た。ほかの条件での挙動は未検証である。

再生経路は、アプリと同じ問いで選ぶ。`web/src/lib/live.ts` の `supportsNativeHls` が真ならネイティブ HLS とする。この関数は、プレイリストの MIME と `video/mp2t` の `canPlayType` がどちらも空でないかを問う。偽で `Hls.isSupported()` が真なら hls.js とする。どちらを使ったかは実行ごとに出力する。hls.js の設定もアプリと同じである。

```sh
cd web
corepack pnpm e2e:recording-segmented-fixture   # フィクスチャだけ作る
corepack pnpm e2e:recording-segmented-vod       # E2E_BROWSER=chrome（既定）
E2E_FORMATS=ts,fmp4 corepack pnpm e2e:recording-segmented-vod
E2E_CASES=ts/no-offset corepack pnpm e2e:recording-segmented-vod
```

`E2E_FORMATS` の既定は `ts` である。`fmp4` を足すと、`direct` と `restarted` の 2 つを比較用に回す。判定基準は TS と同じで、緩めない。fMP4 も `#EXT-X-DISCONTINUITY` なしでつなぎ、`#EXT-X-MAP` は最初の ffmpeg 実行の init segment を 1 つだけ宣言する。

#### アプリの URL に向ける

録画の master を持つ URL に向けると、正例の判定だけをその URL に回す。負例は内蔵フィクスチャでしか作れない。

URL は `E2E_MASTER_URL` で渡す。

```sh
E2E_MASTER_URL=http://localhost:40773/<録画の master.m3u8> \
E2E_SEGMENT_SECONDS=2 E2E_BOUNDARY_SECONDS=12 E2E_FAR_SEEK_SECONDS=52 \
corepack pnpm e2e:recording-segmented-vod
```

- `E2E_SEGMENT_SECONDS` はセグメント長で、既定は 2 である。判定を始める位置を決める
- `E2E_BOUNDARY_SECONDS` は、古い実行のセグメントから新しい実行のセグメントへ切り替わる秒で、既定は 12 である。ffmpeg を起動し直した位置（内蔵では 10 秒）ではない
- `E2E_FAR_SEEK_SECONDS` はバッファの外へ飛ぶ秒で、既定は 52 である。アプリの master の BANDWIDTH から、先読みの目安を `maxBufferSize × 8 / BANDWIDTH` 秒（`maxBufferLength` が大きければそちら）と見積もり、その外を選ぶ。外れていれば `[seek-buffered]` で分かる
- ハーネスのページとメディアは同一オリジンでなければならない。オリジンが違うと、hls.js の XHR が CORS で止まり、`createMediaElementSource` が無音になる。そのためローカルのサーバーが、`E2E_MASTER_URL` のオリジンへ同じパスで中継する

アプリの実装の受け入れでは、フィクスチャが作る `source.ts` を録画として登録し、その録画の master を `E2E_MASTER_URL` に渡す。`source.ts` は `<os.tmpdir()>/rokuban-e2e-recording-segmented-v2/source.ts` にある。境界の秒は、アプリが新しい ffmpeg の出力へ切り替わる位置に合わせる。

#### ブラウザごとの違い

Chrome は Google Chrome を hls.js で判定する。ここでは上の判定をすべて回す。

WebKit は診断用である。Playwright の WebKit はネイティブ HLS を選ぶ。ネイティブ HLS では次を省くか、扱いを変える。

- 音声の周波数と無音は省く。ネイティブ HLS の音声が WebAudio に届かない。主・副の切替は `audioTracks` の選択と再生継続だけを見る。`audio-gap` と `stall` の負例は回さない
- シーク前の要求は省く。先読みを hls.js の設定ではなくブラウザが決める。実行の出力の「省略:」に残す
- `no-offset` は正例として扱う。ネイティブ HLS は、境界と遠距離シークの後でフレーム番号が連続して再生された。この経路では不具合ではない

`off-grid` は、ネイティブ HLS では境界で約 1 秒フレームが描かれなくなり、その間の内容（12〜13 秒）は表示されない。止まったうえで内容が飛ぶ。`[frame-freeze]` と境界のフレーム数で検出する。

実 Safari は `safaridriver` を通して判定する。ネイティブ HLS として上の扱いになり、さらに WebAudio に音声が届かなければ `[audio-unavailable]` で落ちる。Safari の「開発」メニューの「リモートオートメーションを許可」を有効にするか、`safaridriver --enable` を一度実行する。

### シークバーのプレビュー（`seek-tiles.mjs`）

動画の下のスクラブ帯でポインタを動かしたとき、その位置に対応するタイル画像が出ることを実ブラウザで見る。位置は「ポインタの x → 帯の中の割合 → 再生位置 → タイルの格子位置」で決まるが、**jsdom の `getBoundingClientRect()` は常に 0 を返すので、この経路は単体テストでは 1 歩も進まない**。`recording-player.test.tsx` が見るのは「タイルの問い合わせが始まること」「タッチでは始めないこと」だけで、位置の正しさはここが唯一の判定手段になる。

見るのは 9 点:

- ① ホバー位置に対応するタイルが出る。**列の折り返し（タイル 3 → 9）と行送り（タイル 10）を別々の位置で固定する** --- 3 だけだと列数を 10 から 5 にしても同じ答えになる（実測で確認済み）
- ② プレビューが帯の幅に収まり、帯そのものを覆わない
- ③ ポインタが帯から離れると消え、映像の上では出ない（両方向を見る）
- ④ タイルが 404 の録画ではプレビューが出ず、再生面（`src` と `duration`）は従来のまま
- ⑤ 帯をクリックすると、その位置でプレビューに出ていたタイルの時刻へ飛ぶ。**動画の配信は Range に応じる**（実物の streamer と同じ）。応じないと Chromium は動画を seekable にせず、クリックが 0 秒から動かない。比べるのは「見えていたタイルの background-position」と「飛んだ先の時刻のタイル」である
- ⑥ 帯が 1 枚ぶん（320px）より狭い画面（幅 340px）でも、プレビューが帯の幅に収まる
- ⑦ タッチ（`hasTouch` / `isMobile`）ではタップで飛ぶだけで、プレビューは出ない。タップが帯に届いたことを飛んだ先の時刻で確かめる（届かなければ「出ない」は空虚に通る）
- ⑧ 400px（light / dark）と 600px（light）で、プレビュー面積を映像の 16% 以下に抑え、ラベルを 14px 以上で表示して時刻・チャプター行から離す
- ⑨ 原本 120 秒、`keepRanges = [[0, 30000), [40000, 120000)]` の合成カット版（110 秒）を使う。cut 軸 25.5 秒は原本 25.5 秒のタイル #2、50.5 秒は原本 60.5 秒のタイル #6 を出す。ラベルは cut 軸の `0:25` / `0:50` を出す。クリック後の `video.currentTime` も cut 軸の 50.5 秒である。整数秒の境界は CSS ピクセルへの丸めで手前のタイルに入るため、各 10 秒格子の途中を指す。cut 版でもタイルを問い合わせ、404 ならプレビューを出さず再生面を保つ

判定用の動画は ffmpeg で作る（動画の長さが要る）。**VP8/WebM を使う** --- Playwright の Chromium は H.264 を持たない構成があり、コーデックの有無で落ちると「実装が誤っている」と区別できない。ffmpeg が無い環境ではこの判定だけを skip する。

**変異で落ちることを確認済み**: プレビューを `hidden` にする（①②③が落ちる）。`SEEK_TILES_COLUMNS` を 5 にする変異も、①の位置判定で落ちる。実測の文言は「タイル #9 の background-position が -1280px -180px（期待 -2880px 0px）」である。

ホバーの受け口を帯から外枠（映像を含む）へ広げる変異は③で落ちる。クリック先だけ 0.8 倍にずらす変異は⑤⑦で落ちる（「-960px 0px を見てクリックしたが 28.0s（タイル #2）へ飛んだ」）。狭い画面で縮めない変異は⑥で落ちる（「preview 32..352 / scrub 32..308」）。

ホバーの受け口を `onMouseMove` / `onMouseLeave` に戻す変異（修正前の実装）は⑦で落ちる。追加した cut 判定は、修正前の実装でタイル問い合わせが始まらず、25.5 秒・50.5 秒のプレビューも出ずに落ちた。

タイル位置への cut → 原本写像を外し、cut 秒をそのまま格子に使う変異では⑨が落ちる。実測は「cut 50.5s のタイル位置が -1600px 0px（原本 60.5s の期待 -1920px 0px）」で、表示した tile #5 と期待する tile #6 を区別する。これは合成動画でクライアントの写像を測る判定で、実録画のタイル生成と keep 区間の時刻軸が一致することは保証しない。

**Go 側と TS 側の定数が揃っていること自体は、ここでは測れない。** 同じ値を持つファイルが 2 つある。この判定のフィクスチャは手書きで、Go が作る格子を再現していない。揃っていることは両側の単体テストでリテラル固定する。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:seek-tiles
```

<details><summary>根拠</summary>

- Go 側のタイル定数: `internal/worker/seek_tiles.go`
- TS 側のタイル定数: `src/lib/seek-tiles.ts`
- Go 側のタイル数: `TestSeekTileCount`
- worker のシート引数: `TestSeekTilesWorker_ComposeSheetArgs`
- TS 側の格子: `seek-tiles.test.ts`

</details>

### チャプターの目盛りと自動スキップ（`chapters.mjs`）

録画の詳細で、CM の目盛りが帯の正しい位置に出ること・通常の再生で `cut` 区間の先頭に差し掛かると終端へ飛ぶこと・手動のシークで区間の中に入ったときは飛ばないこと・境界の「前後 3 秒」が境界の手前から始まることを実ブラウザで見る。**目盛りの位置は帯の実寸に対する割合で決まり（jsdom の `getBoundingClientRect()` は常に 0）、スキップは実再生の `currentTime` の推移でしか観測できない。** 判定はここが唯一の手段になる。

フィクスチャは ffmpeg で作った 120 秒の WebM に、`[30, 40)` の `cut` な CM と `[60, 70)` の `cut` でない OP を重ねる。**再生位置は localStorage に残る**ので、判定の前に `localStorage.clear()` する（前回の続きから始まると位置の判定が揺れる）。音声トラックが無くても Chromium の自動再生ポリシーは掛かるので、 `--autoplay-policy=no-user-gesture-required` を付けて起動する。

- ① 目盛りの数と左端の座標・幅が区間の割合と一致し、`data-cut` が区間の `cut` と一致する
- ② 区間の手前 2 秒から再生して 4 秒後、位置が区間の終端を越えている（飛ばなければ 32 秒付近にとどまる）
- ③ 区間の中へシークしてから再生しても追い出されない。**cut でない OP を通り抜けても飛ばない**（両方を見る）。再生が進んでいることも確かめる（空虚な成功を防ぐ）
- ④ 編集の `<details>` は閉じているので、先に `summary` を開いてから見る。境界の「前後 3 秒」が 1 秒後に境界の手前 3 秒付近に居る（境界そのものから始める実装と区別できる）。そのまま境界を跨いでも飛ばされない（前後再生の間は自動スキップを止める）
- ⑤ 2 倍速でも前後再生が境界の 3 秒後で止まる。⑤-b は「境界まで」（30 秒で止まる）と「境界から」（33 秒で止まる）も 2 倍速で見る。実時間のタイマーで止める実装なら、「境界まで」は 33 秒付近まで進む

**変異で落ちることを確認済み**: 目盛りの `left` を区間の終端にする変異は①（「実際 x=480.0 w=64.0 / 期待 x=416.0 w=64.0」）。飛び先を `currentTime` に代入しない変異は②（「4 秒後の位置 32.0 秒」）。直前位置を常に 0 にする変異は③（「シークで入った cut 区間から追い出された（位置 42.7 秒）」）。前後再生でスキップ抑制を外す変異は④（「前後 3 秒の再生中に境界で自動スキップが働いた（位置 41.38 秒）」）。

```sh
pnpm build && pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:chapters
```

⑤-b の変異確認として、`timeupdate` の停止判定（`currentTime >= stopAt`）を外した。実時間の保険タイマーだけで止まる形になり、2 倍速の「境界から」が落ちる。Chrome は「⑤-b 2 倍速の「境界から」が 33 秒で止まっていない（位置 37.00 秒 paused=true）」、WebKit は同じ判定で位置 37.01 秒だった。⑤ の前後 3 秒も同時に落ちる。「境界まで」はこの変異では落ちない。保険タイマーの停止後にも境界変換のシークし直しが走り、位置が境界に戻るためである。したがって「境界まで」の 2 倍速は、停止判定そのものではなく停止位置を見ている。
