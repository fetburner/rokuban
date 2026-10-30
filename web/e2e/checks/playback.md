# 録画再生のブラウザ確認

### 世代付き録画 HLS

`recording-playback-timeline.mjs` は、実ブラウザが録画 API の `hlsPlayback` にある master URL を直接読み、録画時間軸から seek することを確認する。Go の fixture test が実装の変換引数で HLS と目印入り TS を作るため、Go と ffmpeg が要る。

確認する項目:

- `hlsPlayback` が空のとき HLS を要求せず、encoded が無い場合は再生可能ファイルの空状態を出す
- recordings SSE 後に `hlsPlayback` が届くと再生開始操作を出す
- `/api/media/stores/{store}/recordings/{id}/hls/v{gen}/{profile}/master.m3u8` を使い、録画中の時間軸と seek を実メディアで測る
- ingest commit の前後で同じ世代付き URL を維持する
- 世代 URL が 410 になり API が一時的に HLS を返さない場合も、新世代の URL を取得して停止直前の位置から再生を続ける

```sh
cd web
pnpm build
pnpm preview --port 4173 --strictPort &
E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-playback-timeline
```

### LivePlayer の 503 文言・離脱ヒント（`live-player-modes.mjs`）

ライブの離脱ヒントと、ライブ・録画 HLS の 503 文言を、短い実メディアを再生する実ブラウザで測る。mirakc と実録画は要らない。

`/api/**` と HLS は `page.route` で差し替える。fixture は ffmpeg が作った 20 秒の HLS で、Chromium の hls.js が再生する。jsdom には `navigator.sendBeacon` が無く fetch に落ちるため、離脱ヒントの実送信は実ブラウザでしか測れない。

- `visibilityState` を `visible` にした `visibilitychange` は、leave を 1 件も増やさない。hidden の判定を外す変異（visible でも送る）を落とす
- `visibilityState` を `hidden` にした `visibilitychange` と `pagehide` は、ライブの `/live/leave` へ POST を 1 件ずつ送る。録画 HLS はセッションを作らないので離脱ヒントを持たない
- playlist の 503 は、ライブがチューナー案内、録画 HLS が「いま録画を再生できません。」と再試行案内を表示し、録画 HLS にライブの案内を出さない

```sh
E2E_URL=http://localhost:4173 pnpm e2e:live-player-modes
E2E_VALIDATE_FIXTURES_ONLY=1 node e2e/live-player-modes.mjs
```

`E2E_VALIDATE_FIXTURES_ONLY=1` は録画 fixture を Zod 契約で検査するだけでブラウザを起動しない。CI の `browser-e2e` ジョブが実ブラウザ判定を回す。

### 録画 HLS の再起動境界（`recording-segmented-vod.mjs`）

録画再生は、録画時刻上の固定長セグメントを合成 playlist で配り、位置が離れたら ffmpeg を 1 つ前の格子点から起動し直す方式である。この方式の合否は jsdom では測れない。止まらずに再生できるか、継ぎ目で音が切れないかを実ブラウザで判定する。アプリの実装より先に判定手段を置いている。

アプリは playlist を格子から先に合成するので、ffmpeg をどこで再起動したかを知らない。そのため再起動境界に `#EXT-X-DISCONTINUITY` を付けられない。内蔵フィクスチャも付けない。

フィクスチャの HLS は、製品の `buildSegmentTransformArgs`（`internal/streamer/segment_transform.go`）が組む引数で作る。ffmpeg を起動するのは Go のテストで、`recording-segmented-fixture.mjs` が `go test` で呼び、出力を playlist でつなぐ。環境変数 `ROKUBAN_SEGMENTED_PLAYBACK_FIXTURE_DIR` が無いとき、そのテストは何も書き出さない。

JS で ffmpeg の HLS の引数を組むのをやめたのは、製品の引数と時間軸の扱いが食い違ったまま判定が通るためである。実際に、製品には無い 2 つのずれが正例に入っていた。素材の最初のフレームが複製され、本物のフレームが 1 枠後ろに見えたこと。第 1 run だけ時間軸が 21.3 ms ずれて、境界に 12 ms の重なりができたことである。

素材の `source.ts` は 60 秒の合成 TS で、映像は 29.97 fps（30000/1001）にしてある。実際の録画と同じく、2 秒の格子にフレームが割り切れない（59.94 フレーム）条件を入れるためである。映像にフレーム番号を 11 bit の箱で焼き、音声は左 440 Hz・右 880 Hz の 2ch AAC とする。セグメントは 2 秒で、標準・主・副の 3 rendition を持つ。製品の出力は TS セグメントである。`E2E_FMP4=1` のときだけ、比較用の fMP4 も作る。

時間軸の起点は、素材の最初の映像 PTS（`OriginPTS`）である。Go のテストが `ffprobe` で読み、`SegmentTransformInput` に渡す。この素材は AAC の priming のため映像が音声より 21.3 ms 遅れて始まる（音声 1.400 秒、映像 1.4213 秒）。製品の引数は `trim` と `-output_ts_offset` で映像を起点に揃えるので、先頭フレームの複製が起きない。

playlist は JS が合成する。`EXTINF` は格子の長さ（最後だけ残り）、`#EXT-X-PROGRAM-DATE-TIME` は格子の位置から付ける。`master.m3u8` の BANDWIDTH は 20 Mbps に固定する。先読みの範囲は hls.js の設定 `maxBufferLength`（秒）・`maxBufferSize`（バイト）と BANDWIDTH で決まる。

ffmpeg が書く master の BANDWIDTH は約 1.87 Mbps である。この値だと、先読みの計算値（60 MB × 8 / 1.87 Mbps ≒ 257 秒）が 60 秒の録画を超え、遠距離シークがバッファの内側になりうる。1.8 Mbps では 60 秒がすべて先に要求された観測がある。

- `direct` は `startNumber = 0` の run 1 本で通して作る
- `restarted` は、その run の番号 0〜5 のセグメントと、`startNumber = 5` で起動し直した run の番号 6 以降をつなぐ。境界は 12 秒の格子点である。製品は要求の 1 つ前の番号から変換を始めるので、要求 6 の変換が `startNumber = 5` になる形に合わせてある
- `off-grid`（負例）は、`restarted` の引数から seek・`trim`・`-output_ts_offset` の位置を、格子点ではない 11 秒へ書き換える
- `no-offset`（負例）は、`restarted` の引数から `-output_ts_offset` を外す
- `audio-gap`（負例）は、`restarted` の標準音声のフィルタに、再起動直後（run の 2 秒後）の AAC 1 フレーム分（1024 samples、約 21 ms）の無音を足す
- 映像セグメントの遅延（負例）は、境界の映像セグメントの応答を、再生位置が境界の手前に来るまで保留して作る。`stall` と呼ぶ
- `restarted-k16` は診断用で、既定では回さない（`E2E_CASES=ts/restarted-k16`）。`startNumber = 16`（境界 34 秒）で起動し直す。最初のフレームと T のずれが 1 フレームに近い位置である

#### 時間軸の起点と再起動のずれ

hls.js 1.7.3 と WebKit は、音声の無い映像 variant で、時間軸の 0 を最初の映像フレームに置いた。これは観測であって約束ではない。

hls.js は、各トラックの先頭の DTS の最小を起点にする（`node_modules/hls.js/src/remux/mp4-remuxer.ts` の `generateIS`）。このフィクスチャの映像 variant は音声を持たないので、`mediaTime` はフレーム番号 k に対して k × 1001/30000 秒になった。ほかのバージョンやブラウザでは未検証である。

29.97 fps では、再起動の位置によって、最初のフレームが T から 0〜1 フレーム後ろにずれる。`-force_key_frames` の expr の `t` はエンコーダに届いた最初のフレームから数えるので、以前の `setpts=PTS-STARTPTS` ではキーフレームの格子がそのずれに引きずられた。エンコーダの既定の time base（frame rate の逆数）への丸めも、起動位置で向きが変わった。そのため、番号 16 の再起動では境界の 1019 が描かれず、`media-grid` が −32.0 ms になって判定を通らなかった。

製品の引数は、最初のフレームだけを T に置き、残りは元の PTS から T を引く。エンコーダの time base は 90 kHz、`-fps_mode:v passthrough` にした。Chrome 155 / hls.js 1.7.3 では、どちらの再起動でも境界のフレームは欠けなかった。`restarted`（番号 5）は 359〜361、`restarted-k16`（番号 16）は 1018〜1020 が続けて描かれた。境界の後の `media-grid` はどちらも 0.0 ms だった。WebKit では測り直していない。

#### 判定

判定は 6 つある。

- 再生が止まらない。再生中に `currentTime` が 500 ms 以上進まない回数を数える。シーク後に時刻が進むまでは数えない
- 境界の ±0.5 秒で、焼いたフレーム番号が `mediaTime` と、`mediaTime` が `currentTime` と、それぞれ ±1 フレームで一致する。不一致は 1 枚でも落とす
- 内容が壁時計に対して飛ばず、戻らず、止まらない。連続する frame callback の間で、フレーム番号の進みが表示時刻の差 × 29.97 + 3 を超えない。callback の間隔が 250 ms を超えない。境界の ±0.5 秒に 20 枚以上が描かれる。`currentTime` ごと内容が飛ぶブラウザでは、marker・`mediaTime`・`currentTime` が一致したままになるので、この判定でしか見えない
- 境界の ±0.8 秒で、AudioWorklet が 128 サンプル（48 kHz なら約 2.7 ms）ごとの左右 RMS を全サンプルについて求め、無音が 2 回続かない。標準音声の周波数が 440 Hz / 880 Hz のままである
- 主・副・標準の切替で、WebAudio の左右周波数が変わる
- 遠距離シークで、seeked の位置がシーク先 ±1 フレームである。再生を始めて、シーク先 ±0.5 秒のフレームが 3 枚目までに描かれる。そのうち最初のフレームの番号がシーク先 ±1 フレームで、先頭 5 枚のフレーム番号が自分の `mediaTime` と ±1 フレームで一致する。シーク先のセグメントをシーク前に要求していない（サーバーの要求ログで見る）

hls.js が non-fatal で報告する次の 4 つは、内容の欠けか停止の現れとして失敗にする。`bufferSeekOverHole`・`bufferNudgeOnStall`・`bufferStalledError`・`bufferAppendNoProgress` である。

負例は、期待する名前の失敗で落ちたときだけ「期待どおり」とする。名前は `[timecode]`・`[content-jump]`・`[frame-freeze]`・`[seek-frame]`・`[seek-buffered]`・`[audio-gap]`・`[stall]`・`[hls-gap]` のように、各失敗の先頭に付く。待ちが尽きて例外になったものは `[progress]` であり、検出とは数えない。期待する名前は次のとおりである。

- `off-grid` は `[timecode]`・`[content-jump]`・`[frame-freeze]` のどれか
- `no-offset` は `[seek-frame]`・`[content-jump]`・`[frame-freeze]` のどれか
- `audio-gap` は `[audio-gap]`
- `stall` は `[stall]`
- `event/shrink` は `[playlist-shrink]`
- `event/extinf-change` は `[playlist-mutated]`

`no-offset` は、Chrome の hls.js では境界を通して再生するだけでは検出できなかった。境界の ±0.5 秒は判定を通り、遠距離シークの後で着地が 48.6 秒になって `[seek-frame]` になり、`bufferAppendNoProgress` が出て再生が進まなかった（`[progress]`）。原因は調べていない。ほかの条件での挙動は未検証である。

再生経路は、アプリと同じ `Hls.isSupported()` の判定を使う。対応時は hls.js で再生し、非対応ならテストを失敗させる。実行ごとに経路を出力する。hls.js の設定もアプリと同じである。

```sh
cd web
corepack pnpm e2e:recording-segmented-fixture   # フィクスチャだけ作る（Go の TestWriteSegmentedPlaybackFixture を呼ぶ）
corepack pnpm e2e:recording-segmented-vod       # E2E_BROWSER=chrome（既定）
E2E_DUMP_FRAMES=1 E2E_CASES=ts/restarted corepack pnpm e2e:recording-segmented-vod   # 境界のフレームを出す
E2E_CASES=ts/no-offset corepack pnpm e2e:recording-segmented-vod
```

`E2E_CASES` はカンマ区切りのケース名で、一致するケースだけを回す。一致が 0 件なら失敗にする。診断用のケースは、名前を書いたときだけ回る。

fMP4 の比較では、製品の変換引数からコンテナと出力ファイル名だけを替える。再起動境界に `DISCONTINUITY` は付けず、`EXT-X-MAP` も切り替えない。全区間に連続版の init を使い、TS と同じ判定を回す。

```sh
E2E_FMP4=1 corepack pnpm e2e:recording-segmented-vod  # TS と fMP4 の両方
E2E_FMP4=1 E2E_CASES=fmp4/direct,fmp4/restarted corepack pnpm e2e:recording-segmented-vod
```

`E2E_FIXTURE_DIR` で生成先を変えられる。同時に実行するときは、実行ごとに別のディレクトリを渡す。同じ場所では生成中のファイルを消し合う。

fMP4 の `tfdt` は、既定ではその run の先頭を 0 にし、`-output_ts_offset` を含まない。12 秒の境界に置く再起動の run の fragment が 2.01 秒の時刻を持ち、Chrome では境界のフレームが 15 枚になって音声も途切れた。比較用の fMP4 には `-hls_segment_options movflags=+frag_discont` を付け、元の時刻を `tfdt` に書かせる。付けると連続版と再起動版の `tfdt` が一致した（境界の fragment はどちらも 1209000）。

Chrome 155.0.8059.85 / hls.js 1.7.3 / ffmpeg 9.0.2 では、fMP4 の連続版・再起動版と負例 4 つが TS と同じく期待どおりになった。Safari 27.0.1 では、fMP4 の連続版・再起動版の映像の判定が通った。音声は WebAudio で測れなかった（下記）。

#### 録画中の EVENT playlist

録画中の playlist は、載せた行を後から変えずに伸び、commit の後に同じ URL のまま最後の短いセグメントと ENDLIST が付く。内蔵のフィクスチャでこの形を再現し、伸びている間に上の 6 つの判定を回してから、終端まで再生する。

内蔵の `event/growing` は、`ts/restarted` のセグメントから伸びる playlist を作る。

- 最初は境界の手前へシークできる行数を載せ、`E2E_EVENT_GROW_INTERVAL_MS`（既定はセグメント長）ごとに 1 行足す
- `E2E_EVENT_SEGMENT_COUNT`（既定 30）行に達した応答で、最後の行の EXTINF を 1 秒にし、`#EXT-X-ENDLIST` を付ける。最後の TS も 1 秒に切り詰め、EXTINF と長さをそろえる
- 行数の上限はフィクスチャの 30 本である。足りなければ起動時に必要な行数を出して止まる

判定は次の順に進む。

1. 再生位置が進み、同じ media playlist をプレーヤーが 2 回以上要求し、ENDLIST の前に行が増えたことを待つ。待たずに判定すると、取得が始まる前に「止まらない」「行が変わらない」が通ってしまう
2. プレーヤーが要求した playlist の応答だけを記録し、判定のための要求は足さない。前回の応答の各行の URI・EXTINF・PROGRAM-DATE-TIME が、次の応答の同じ位置にあることを確かめる。行が減れば `[playlist-shrink]`、既出の行が変われば `[playlist-mutated]` にする
3. 上の 6 つの判定を、ENDLIST が配られる前に終える。先に配られたか、最初の応答にすでに付いていたら `[event-growth]` にする
4. 遠距離シークの行き先は、先読みの外で、まだ要求しておらず、後ろに 2 行が載っているセグメントの先頭から選ぶ
5. ENDLIST を含む応答をプレーヤーが取った後、`video.duration` がその playlist の総尺に届くのを待つ。終端の 3 セグメント手前へシークし、再生が着地から終端までの大半（3 セグメント - 1 秒）進んで `video.ended` になることを確かめる。外れたら `[ended]` にする

負例は 2 つある。`event/shrink` は、240 行の playlist を commit の応答で 182 行に縮める。commit の後に載せ済みの行が縮んだ実録画の不具合を再現する。`event/extinf-change` は、commit の応答で先頭の行の EXTINF を変える。どちらも通常の判定と同じ手順で回し、期待する名前が 1 つでも出れば期待どおりとする。

Chrome 156.0.8078.76 / hls.js 1.7.3 / ffmpeg 9.0.2 では、`event/growing` を既定の間隔で 10 回続けて回して 10 回とも通り、負例 2 つが期待した名前で落ちた。

```sh
E2E_BROWSER=chrome E2E_CASES=event/growing,event/shrink,event/extinf-change corepack pnpm e2e:recording-segmented-vod
```

#### アプリの URL に向ける

録画の master を持つ URL に向けると、正例の判定だけをその URL に回す。負例は内蔵フィクスチャでしか作れない。

URL は `E2E_MASTER_URL` で渡す。

録画中の master を渡すと、「録画中の EVENT playlist」と同じ手順で、伸びている間から ENDLIST の後まで判定を回す。最初に取った media playlist に ENDLIST があれば、完成した録画として今までどおり判定する。`E2E_EVENT_ENDLIST_TIMEOUT_MS`（既定 90 秒）は、シーク先が載るまでと ENDLIST が付くまでの待ちの上限である。

録画中の URL は、録画開始から 1 分以上経ったものに向ける。遠距離シークの行き先（先読みの外で、後ろに 2 行）がすでに載っているためである。録画終了の 2 分以上前に始める。6 つの判定を ENDLIST の前に終えるためで、先に付くと `[event-growth]` になる。

```sh
E2E_MASTER_URL=http://localhost:40773/<録画の master.m3u8> \
E2E_BROWSER=chrome E2E_SEGMENT_SECONDS=2 E2E_BOUNDARY_SECONDS=12 E2E_FAR_SEEK_SECONDS=52 \
E2E_EVENT_ENDLIST_TIMEOUT_MS=180000 corepack pnpm e2e:recording-segmented-vod
```

- `E2E_SEGMENT_SECONDS` はセグメント長で、既定は 2 である。判定を始める位置を決める
- `E2E_BOUNDARY_SECONDS` は、古い実行のセグメントから新しい実行のセグメントへ切り替わる秒で、既定は 12 である。ffmpeg を起動し直した位置（内蔵では 10 秒）ではない
- `E2E_FAR_SEEK_SECONDS` は遠距離シークの秒で、既定は 52 である。完成した録画では、アプリの master の BANDWIDTH から先読みの目安を `maxBufferSize × 8 / BANDWIDTH` 秒（`maxBufferLength` が大きければそちら）と見積もり、その外を選ぶ。録画中の録画では、この秒と先読みの端の遅い方を下限にして、playlist に載っていて未要求のセグメントを選ぶ。完成した録画で先読みの内側を選ぶと `[seek-buffered]` で分かる
- ハーネスのページとメディアは同一オリジンでなければならない。オリジンが違うと、hls.js の XHR が CORS で止まり、`createMediaElementSource` が無音になる。そのためローカルのサーバーが、`E2E_MASTER_URL` のオリジンへ同じパスで中継する

アプリの実装の受け入れでは、フィクスチャが作る `source.ts` を録画として登録し、その録画の master を `E2E_MASTER_URL` に渡す。`source.ts` は `<os.tmpdir()>/rokuban-e2e-recording-segmented-v3/generated/source.ts` にある。境界の秒は、アプリが新しい ffmpeg の出力へ切り替わる位置に合わせる。

#### ブラウザごとの違い

Chrome と Playwright WebKit は、どちらも `Hls.isSupported()` が真なら hls.js で再生する。WebKit は hls.js 経路の診断用で、Safari / iPhone は確認対象にしない。ブラウザごとの測定差は、この E2E を hls.js 経路で実行してから記録する。


### S3 の署名付き URL への 302（`s3-302.mjs`）

rokuban が S3 の署名付き URL へ 302 で飛ばした動画を、実 Chrome と Playwright WebKit で測る。測るのは Range 要求の宛先・署名の期限切れ・字幕・シークと再開位置である。

fixture が再現するのは配信の条件である。ローカルの MinIO に 90 秒の H.264 MP4 と WebVTT を置き、fixture のリダイレクターが署名付き URL へ 302 を返す。S3 宛ての要求は別 port のプロキシが MinIO へ転送し、要求先・Range・status を記録する。

プロキシは動画の応答を、動画のビットレートの 3 倍の速度に絞る。絞らないとブラウザが fixture をまるごと先読みし、シークがキャッシュから返って Range 要求が出ない可能性がある。シークの直前に `video.buffered` がシーク先を含んでいれば、測定の前提が崩れた旨の失敗にする。

判定用ページは再開位置をブラウザ内に保存し、302 の後に読み込んだ動画で再生位置を復元する。製品のプレイヤーも `recording-player.tsx` の `onLoadedMetadata` で `currentTime` に代入して位置を戻すので、ブラウザから見た経路は同じである。プレイヤーの戻し方を変えたら、このページも見直す。これはブラウザの動作の測定で、製品 API の保存契約は対象外である。

MinIO の CORS 許可は、サーバー全体の設定として `http://127.0.0.1:4174` に固定する。固定した版の MinIO は、バケットごとの CORS を設定できず、サーバー全体の設定しか持たない。字幕の負例では、プロキシが負例のバケット宛ての要求から Origin ヘッダーを外し、バケットに CORS が無い場合を再現する。

必要なものは Node の固定版、Go、ffmpeg、MinIO である。MinIO は CI の `browser-s3-302` と同じ固定した版で起動する。版の権威は `.github/actions/build-minio/action.yml` の `ref` で、下の `go install` にはその commit を渡す。

```sh
go install github.com/minio/minio@<action.yml の ref の commit>
mkdir -p /tmp/rokuban-s3-302-minio
MINIO_ROOT_USER=rokuban MINIO_ROOT_PASSWORD=rokuban-test-secret MINIO_API_CORS_ALLOW_ORIGIN=http://127.0.0.1:4174 \
  "$(go env GOPATH)/bin/minio" server /tmp/rokuban-s3-302-minio --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
```

別のターミナルから `web/` で実行する。

```sh
corepack pnpm e2e:s3-302                                # 実 Chrome
E2E_BROWSER=webkit corepack pnpm e2e:s3-302            # Playwright WebKit 診断
E2E_S3_DISABLE_CORS=1 corepack pnpm e2e:s3-302          # 字幕の CORS を壊す負例
E2E_S3_VIDEO_TTL_SECONDS=2 corepack pnpm e2e:s3-302     # 動画の署名の期限を 2 秒にする負例
```

負例の 2 つは、どちらも非 0 で終わる。落ちる判定は、前者が字幕の cue の読み込み、後者が通常の署名 URL でのシークである。

期限切れの判定は結果の記録で、良し悪しは決めない。結果は 3 つに分けて出力する。元の URL へ戻れば `refreshed-at-origin`、403 を受けて video が error を出せば `media-error`、403 を受けても error が無ければ `403-without-error` である。期限後の Range 要求・S3 の最終の応答・error かシークの完了のどれかが観測できないときは、失敗にする。3 つのどれにも当たらないときも失敗にする。

CI の `browser-s3-302` は実 Chrome で回す。Playwright WebKit はローカル診断用である。

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
