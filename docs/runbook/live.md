## ライブ視聴（M4-3 / M4-4）の確認手順

2 段に分かれる。**①は実 mirakc / 実チューナーが要る**（streamer が実際に ffmpeg を
起動してチューナーを掴む）。**②は要らない**（ブラウザ側の配線 --- hls.js の動的
import・MSE への実再生・チャンネル切替時の cleanup ---だけを見る。issue #92 の
着手時点ではこの判定手段が無く、レビューで「テストが実際には何も守っていない」
ことが判明した経緯がある。詳細は [docs/frontend.md](../frontend.md) §フロントエンド
実装（M4-4）参照）。

### ① idle GC と実再生（実 mirakc が必要）

前提: `live.enabled: true`、`live.ffmpeg` が PATH にある、`live.profiles` を
1 つ以上設定済み（[config.example.yml](../../config.example.yml) の `live:` 節）。

```sh
docker compose exec rokuban rokuban server --roles all --config /config.yml
```

1. ブラウザで `/live` を開き、チャンネルを選ぶ。数秒で再生が始まる
2. 別ターミナルでメトリクスを見る:

   ```sh
   curl -s http://localhost:40773/metrics | grep rokuban_live
   # rokuban_live_active_sessions 0
   # rokuban_live_idle_gc_reclaimed_total 0
   # rokuban_live_idle_gc_last_pass_timestamp_seconds 1.7...e9
   ```

   再生中は `rokuban_live_active_sessions` が `1`（見ているチャンネル数）になる
3. ブラウザのタブを閉じる（または別チャンネルへ切り替える）。セグメント要求が
   止まってから **30〜45 秒**（`live.idle_timeout` 既定 30 秒 + GC 周期
   `idle_timeout/2` = 15 秒ぶんの遅れ）で `rokuban_live_active_sessions` が
   `0` に戻り、`rokuban_live_idle_gc_reclaimed_total` が `1` 増える
4. チューナー本数が少ない環境で複数チャンネルを連続でザップすると、
   `live.max_sessions`（既定 4）に達した時点で 503
   `too many concurrent live sessions on this process` が、チューナー自体が
   枯渇した場合は 503 `live stream unavailable` が返る（画面には
   「チューナー不足または同時視聴数の上限」+ 30 秒待つ案内が出る）。
   フロント側はチャンネル切替を 400ms デバウンスしているが、実際に選んで
   留まったチャンネルの前セッションは今までと同様 30〜45 秒残る ---
   デバウンスは「通り過ぎたチャンネル」を掴まないようにするだけの緩和であり、
   idle GC の遅延自体を無くすものではない

### ② ブラウザ側の配線（mirakc 不要）

`web/e2e/live.mjs`。HLS プレイリスト/セグメントは Playwright の `page.route`
でブラウザ側から丸ごと差し替えるため、streamer 側は `live.enabled` を立てる
必要すら無い（サーバーは「サービス一覧を返す」以外の実仕事をしない）。

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

判定する 5 点（詳細はスクリプト冒頭のコメント）:

1. hls.js の動的 import チャンク（`assets/hls-*.js`）が実際に要求される
2. MSE がアタッチされる（`video.src` が `blob:` になる）
3. **実 Chrome のみ**（`channel: 'chrome'`。bundled Chromium は H.264/AAC 非対応）:
   `video.play()` 後に `currentTime` が進み、`videoWidth > 0`
4. チャンネル切替後、旧チャンネルへのセグメント要求が 0 件になる
   （`LivePlayer` の effect cleanup が実際に効いていることの検証）
5. 503（本文つき）でエラー文言が出て、再読み込みで復帰する

初回実行は ffmpeg で固定フィクスチャ（testsrc + sine を H.264/AAC でエンコード
した 40 秒ぶん）を生成し `os.tmpdir()` にキャッシュする（`E2E_LIVE_REBUILD_FIXTURE=1`
で強制再生成）。**ffmpeg が無い環境・Chrome が無い環境では、その判定だけを
「未測定」として報告し（NG にはしない）残りは続行する。**

**この手段が実際に発見した回帰**: `supportsNativeHls`（`lib/live.ts`）が
`canPlayType('application/vnd.apple.mpegurl')` の戻り値 `'maybe'` も対応と
見なしていたところ、実 Chrome（bundled Chromium だけでなく `channel: 'chrome'`
の本物でも）はこの MIME タイプに `'maybe'` を返す（対応の印ではなく「分から
ないが試す価値はある」という楽観的な既定応答）。修正前のままリリースしていたら、
Chrome ユーザー全員が `video.src` に m3u8 を直接渡され、hls.js を一切経由せず
沈黙して再生できなかった。ユニットテスト（jsdom）はこの分岐を一度も実行して
いなかった --- `canPlayType` が jsdom では常に `''` を返すため。

### CI では回さない

`web/e2e/checks.mjs` と同じ理由（実サーバー・実データ依存）。①は実 mirakc/
実チューナーも要るため輪をかけて CI に載せられない。②のみなら理論上は
サーバー + Postgres + Chrome があれば CI でも回せるが、実 Chrome チャンネルの
インストールと ffmpeg の用意が CI イメージに新しい依存を足すため、現時点では
ローカル受け入れ確認の位置づけのままにしてある。
