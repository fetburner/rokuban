> [runbook.md](../runbook.md) の一部。索引から辿る。

## ライブ視聴の確認手順

2 段に分かれる。**①は実 mirakc / 実チューナーが要る**（streamer が実際に ffmpeg を
起動してチューナーを掴む）。**②は要らない**（ブラウザ側の配線 --- hls.js の動的
import・MSE への実再生・チャンネル切替時の cleanup --- だけを見る）。

### ① idle GC と実再生（実 mirakc が必要）

前提: `live.enabled: true`、`live.ffmpeg` が PATH にある、`live.profiles` を
1 つ以上設定済み（[config.example.yml](../../config.example.yml) の `live:` 節）。

**`live.enabled` が false（既定。`config.compose.yml` にも `live:` 節は無い）だと
そもそもライブに辿り着けない**（issue #209）。主ナビに「ライブ」が出ず、`/live` を
直接開くと「この環境ではライブ視聴が無効です」になる。設定が効いているかは
`curl -s http://localhost:40773/api/capabilities` が `{"live":true}` を返すかで
確かめられる。

```sh
docker compose exec rokuban rokuban server --all --config /config.yml
```

1. ブラウザで `/live` を開き、チャンネルを選ぶ（この時点ではまだ何も始まらない
   --- issue #234 M7-1 で選択と視聴開始を分離した）。「再生」ボタンを押すと
   数秒で再生が始まる
2. **iPhone の Safari で `/live` を開いて再生できることを確認する（未実施）**。
   iPhone は `window.MediaSource` を持たない（`ManagedMediaSource` のみ。iPad と
   違う）。**ネイティブ HLS 経路に入れないと iOS 17.1 未満では
   「このブラウザはライブ視聴（HLS）に対応していません」になる**。再生経路の
   判定（`supportsNativeHls`）は macOS の WebKit / Chromium / Chrome / Firefox
   では実測して固定してある（`web/e2e/live.mjs` の⑥）。**iOS 実機は誰も
   確認していない**。`canPlayType('video/mp2t')` の戻り値が macOS の WebKit と
   同じ `'maybe'` である保証は無い。違っていた場合は hls.js 経路へ落ちる
   （iOS 17.1 以降なら ManagedMediaSource で再生できるが、それ未満では
   `claimsHlsPlaylistSupport` の最後の砦で `<video>` に直接渡る形になる）。
   確認できる人が実機で開いて、①再生できること ②開発者ツールで
   `assets/hls-*.js` を読み込んでいないこと（読み込んでいたらネイティブ分岐に
   入れていない）を見る
3. 別ターミナルでメトリクスを見る:

   ```sh
   curl -s http://localhost:40773/metrics | grep rokuban_live
   # rokuban_live_active_sessions 0
   # rokuban_live_idle_gc_reclaimed_total 0
   # rokuban_live_leave_hints_total{result="deadline_shortened"} 0
   # rokuban_live_idle_gc_last_pass_timestamp_seconds 1.7...e9
   ```

   再生中は `rokuban_live_active_sessions` が `1`（見ているチャンネル数）になる
4. ブラウザのタブを閉じる（または別チャンネルへ切り替える）。**離脱ヒントが届けば
   十数秒**（猶予 8 秒 = `3 × segment_seconds + 2s` + GC 周期 4 秒ぶんの遅れ。
   実測 13 秒）で
   `rokuban_live_active_sessions` が `0` に戻り、
   `rokuban_live_idle_gc_reclaimed_total` が `1` 増える。同時に
   `rokuban_live_leave_hints_total{result="deadline_shortened"}` も `1` 増えている
   はず（**ここが増えずに回収された場合、ヒントは届いていない** ---
   その場合の回収は `live.idle_timeout` 既定 30 秒 + GC 周期 4 秒 = **30 秒強**
   後になる（実測 33 秒））。**この秒数は偽 mirakc + 偽 ffmpeg に対する実バイナリ
   （`rokuban server --roles streamer`）で実測した**（ヒントあり 13 秒 /
   ヒント無し 33 秒。`rokuban_live_active_sessions` が 0 に戻るまでを 1 秒間隔で
   ポーリング。issue #191）。**実チューナー・実 ffmpeg では未測定** ---
   ffmpeg の停止に掛かる時間だけ伸びうるので、この手順で確かめる
   - 同じチャンネルを 2 つのタブで開いて片方だけ閉じると、
     `rokuban_live_leave_hints_total` は増えるが
     `rokuban_live_active_sessions` は `1` のまま下がらない（残っているタブの
     セグメント要求が idle 期限を戻す）。**これがヒントを「停止命令」にしなかった
     理由そのもの**（[api.md](../api.md) §ライブ視聴の HLS）。偽 mirakc に対する
     実バイナリでは実測済み（2 秒ごとに `leave` を送りながらセグメントを取り
     続ける形で 20 秒間、セッションは落ちなかった。ヒント 10 回に対し回収 0 回）
5. チューナー本数が少ない環境で複数チャンネルへ連続で「再生」を押すと、
   `live.max_sessions`（既定 4）に達した時点で 503
   `too many concurrent live sessions on this process` が、チューナー自体が
   枯渇した場合は 503 `live stream unavailable` が返る（画面には
   「チューナー不足または同時視聴数の上限」+ 30 秒待つ案内が出る）。**チャンネル
   選択自体（`?serviceId=` を切り替えるだけ）はセッションを起こさない**
   （issue #234 M7-1）ため、ここで積まれるのは実際に「再生」を押した本数だけで、
   通り過ぎただけのチャンネルは対象外 --- 以前あった 400ms のデバウンス
   （ザッピングでセッションが積まれないようにする緩和）は選択自体がコスト 0 に
   なったことで存在理由が消え、削除した。押して留まったチャンネルの前セッションは
   離脱ヒントが届けば十数秒、届かなければ 30 秒強残る（上の 4）
6. **実配信で一時停止しても誤ってエラーにならないことを確認する（未実施）**。
   ネイティブ経路は `stalled` / `waiting` が猶予（12 秒）を超えたら失敗と見なすが、
   WebKit は**一時停止した瞬間にも `stalled` を出す**。そのため一度でも再生が
   始まった後の一時停止中は失敗と見なさないようにしてある
   （`live-player.tsx` の `watchNativeMedia`）。**この抑止をブラウザ側で
   機械判定する手段は無い**。`web/e2e/live.mjs` は一時停止を一度も作らない。
   実装側の分岐は `live-player.test.tsx`（jsdom）が測る。ブラウザが実際に
   pause 時に `stalled` を出し再開時に `waiting` を再送することは、
   レビュー時の WebKit 手動測定が根拠である。**実配信では未確認**（e2e の
   フィクスチャは `-hls_list_size 0` 生成の `#EXT-X-ENDLIST` を持つ VOD 形で、
   実配信のローリングウィンドウとは挙動が違いうる）。実機で再生 →
   一時停止 → 30 秒放置 → エラー画面が出ないこと、再開して再生が続くことを見る

### ② ブラウザ側の配線（mirakc 不要）

`web/e2e/live.mjs`。HLS プレイリスト/セグメントは Playwright の `page.route`
でブラウザ側から丸ごと差し替える。streamer 側は `live.enabled` を立てる
必要すら無い（サーバーは「サービス一覧を返す」以外の実仕事をしない）。
`GET /api/capabilities` も同じく差し替えている --- 立てていないサーバーだと
画面が「無効です」になって①〜⑦が全滅するため（issue #209）。

`E2E_LIVE_SERVICE_A` / `_B` に渡すのは **SI の `serviceId`**（下の投入例なら
9001 / 9002）。ライブの URL に載るのも SI の `(network_id, service_id)` そのもの
なので、合成 id への読み替えは要らない。

準備（初回のみ）:

```sh
# EPG プロジェクションに最低 2 つのチャンネルが要る。mirakc からの実同期でも、
# 以下のような直接 INSERT でも足りる（`has_programs` は epg_programs の
# 存在から導出されるので番組行も入れる）
psql "$DATABASE_URL" -c "
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ('default', 1, 9001, 1, 0, 1, 'E2E チャンネル A', 'GR', '13', false),
       ('default', 1, 9002, 1, 0, 2, 'E2E チャンネル B', 'GR', '15', false);
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, name, description)
VALUES ('default', 900101, 1, 9001, 1, now() - interval '5 minutes', 3600000, now() + interval '55 minutes', 'テスト番組 A', ''),
       ('default', 900201, 1, 9002, 1, now() - interval '5 minutes', 3600000, now() + interval '55 minutes', 'テスト番組 B', '');
"
```

実行:

```sh
cd web && pnpm build
go build -o /tmp/rokuban ./cmd/rokuban
/tmp/rokuban server --roles api --config dev.local.yml &
E2E_LIVE_SERVICE_A=9001 E2E_LIVE_SERVICE_B=9002 node web/e2e/live.mjs
```

ブラウザは初回だけ取得する（**WebKit も要る**。⑥がそれでしか測れない）。

```sh
pnpm exec playwright install chromium webkit
```

判定する点（詳細はスクリプト冒頭のコメント）:

0. **選択と視聴開始の分離（issue #234 M7-1）**: `/live?serviceId=` を開いた
   直後はプレイリスト/セグメント要求が 0 件（`page.route` で観測）で、「再生」
   ボタンを押して初めて要求が飛ぶ。ffmpeg 不要（フィクスチャを使わない）で
   bundled Chromium だけで測れるため、①〜⑦と異なりゲートしていない。
   ①〜⑦は「再生」ボタンを押した後の挙動を見るものなので、`page.goto` の後に
   このボタンを押す手順を挟んでいる
1. hls.js の動的 import チャンク（`assets/hls-*.js`）が実際に要求される
2. MSE がアタッチされる。`video.currentSrc` が `blob:` になる。`src` は
   hls.js が `sourceopen` 後に object URL を revoke するので短命である。
   これだけを見ると取り逃がす
3. **実 Chrome のみ**（`channel: 'chrome'`。bundled Chromium は H.264/AAC 非対応）:
   `video.play()` 後に `currentTime` が進み、`videoWidth > 0`
4. チャンネル切替後、旧チャンネルへのセグメント要求が 0 件になる
   （`LivePlayer` の effect cleanup が実際に効いていることの検証）
5. 503（本文つき）でエラー文言が出て、再読み込みで復帰する
6. **WebKit（Safari 相当）でネイティブ HLS 経路に入る**: `assets/hls-*.js` を
   **読み込まない**・`<video>` に m3u8 の URL がそのまま渡る・そのまま再生が
   進む（`currentTime` が進み `videoWidth > 0`。WebKit は `<video>` が MPEG-2 TS
   を demux できる唯一のエンジンなので、フィクスチャをネイティブに再生できる）
7. **ネイティブ経路のメディア失敗が画面に出る**（WebKit）: プレイリストは 200 だが
   セグメントが 404 / 無応答のとき、エラー表示と `再読み込み` ボタンが出る。
   probe は HTTP 層しか見ないので、ここを `<video>` のイベントで拾えていないと
   **永久に止まった黒いプレイヤー**になる。壊れ方で出るイベントが違う
   （404 は `error`、無応答は `stalled` のみ）ので 2 通りとも見る

8. **チャンネル切り替えで離脱ヒントが実際に飛ぶ**（`POST .../live/leave`。④ と
   同じ切り替え操作を観測する）。**jsdom では原理的に測れない** ---
   `navigator.sendBeacon` が jsdom に無いため、ユニットテストが見ているのは
   差し替えた関数が呼ばれたかという配線だけで、実ブラウザが本当にネットワーク
   要求として送出するかはここでしか出ない。離れた側（A）にだけ飛ぶことを見る。
   これから見るチャンネル（B）には飛ばないことも見る

初回実行は ffmpeg で固定フィクスチャ（testsrc + sine を H.264/AAC でエンコード
した 40 秒ぶん）を生成する。`os.tmpdir()` にキャッシュする（`E2E_LIVE_REBUILD_FIXTURE=1`
で強制再生成）。**ffmpeg が無い環境・Chrome が無い環境では、その判定だけを
「未測定」として報告し（NG にはしない）残りは続行する**。

**この手段が実際に発見した回帰（2 件）**:

1. `supportsNativeHls`（`lib/live.ts`）が
   `canPlayType('application/vnd.apple.mpegurl')` の戻り値 `'maybe'` も対応と
   見なしていた。実 Chrome（bundled Chromium だけでなく `channel: 'chrome'` の
   本物でも）はこの MIME タイプに `'maybe'` を返す。そのままリリースしていたら
   Chrome ユーザー全員が `video.src` に m3u8 を直接渡され、hls.js を一切経由せず
   沈黙して再生できなかった。ユニットテスト（jsdom）はこの分岐を一度も実行して
   いなかった --- `canPlayType` が jsdom では常に `''` を返すため
2. その修正（`'probably'` のみを対応と見なす）が**どの実ブラウザでも false**に
   なっていた。3 エンジンとも codecs 無しの m3u8 には `'maybe'` しか返さず、
   codecs 付きなら 3 エンジンとも `'probably'` を返す。戻り値を決めているのは
   codecs の有無であってエンジンではない。**①〜⑤ が Chromium 系しか回して
   いなかったので、この回帰は e2e 緑のまま通った。**⑥（WebKit）を足して初めて
   機械判定できるようになった。判定は「プレイリストとセグメントの両方の
   Content-Type を `<video>` が再生できるか」（`video/mp2t` を demux できるのは
   WebKit だけ）に置き換えてある

### CI では回さない

`web/e2e/checks.mjs` と同じ理由（実サーバー・実データ依存）。①は実 mirakc/
実チューナーも要るため輪をかけて CI に載せられない。②のみなら理論上は
サーバー + Postgres + Chrome があれば CI でも回せる。ただし、実 Chrome チャンネルの
インストールと ffmpeg の用意が CI イメージに新しい依存を足すため、現時点では
ローカル受け入れ確認の位置づけのままにしてある。

## 経緯と失敗事例

- ②の判定手段（`web/e2e/live.mjs`）は、ライブ視聴のフロントエンド実装
  （issue #92 / M4-4）の着手時点には無く、レビューで「テストが実際には何も
  守っていない」ことが判明して作られた。実装より先に判定手段を作る教訓の実例
  （CLAUDE.md「テスト規律」）
