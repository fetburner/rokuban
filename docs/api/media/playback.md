> [media.md](../media.md) の録画・追っかけ再生。索引から辿る

### 録画中の追っかけ再生

録画中の `recordings` を、録画開始位置から現在の録画末尾まで HLS で追従再生する。
ライブ視聴と同じ `LiveStreamer` のセッション管理・`live.max_sessions`・idle GC・離脱
ヒントを共有するが、資源の同定子は録画の durable id である。

```
GET  /api/sites/{site}/recordings/{id}/chase/playlist.m3u8[?profile=<name>]
	       → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/chase/offset/{offset}/playlist.m3u8[?profile=<name>]
	       → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/chase/segments/{name}
GET  /api/sites/{site}/recordings/{id}/chase/{name}       （master の映像 / 音声 variant / subtitle playlist）
	       → video/mp2t / text/vtt / application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/chase/offset/{offset}/segments/{name}
GET  /api/sites/{site}/recordings/{id}/chase/offset/{offset}/{name}
	       → video/mp2t / text/vtt / application/vnd.apple.mpegurl
POST /api/sites/{site}/recordings/{id}/chase/leave
POST /api/sites/{site}/recordings/{id}/chase/offset/{offset}/leave
	       → 204（離脱のヒント）
```

これらは録画ファイル配信と同じく `openapi.yaml` には載せない。`{id}` は
`recordings.id` の十進正準形で、DB の `record_sync` から `(site, record_id, status)`
と `kind='original'` の `media_assets` 行の有無を逆引きする。同期行と録画行がどちらも
`recording` または `finished` で、原本行がまだ無い間は新しいセッションを開始できる。
原本行は `state` を問わず見る。`deleted` も ingest が一度 commit した事実なので、
mirakc に record が残っているように見えても新しいセッションを作らない。
正常終了した録画は、既に開始済みのセッションが保持する EVENT playlist とセグメントを
idle GC まで取得できる。ごみ箱・commit 済みで保持セッションの無いもの・失敗・未束縛・
存在しない id は 404 である。URL の `site` は `cmd/rokuban` の site
束縛へルーティングするための値で、DB の録画 site と一致しない要求は 404 にする。

mirakc へは `GET /api/recording/records/{record_id}/stream` を Range なし・優先度
ヘッダーなしで要求する（オフセット省略時、従来どおり録画先頭から追従する経路）。
`{offset}` は録画開始からの 0 以上の整数秒である。省略は `0` と同じである。
オフセット付きでは
`GET /api/recording/records/{record_id}` の `recording.startTime` と `content.length` から
概算バイト位置を求める。TS パケット境界に合わせて `Range: bytes=<position>-` で要求する。
mirakc の Range 応答は要求時点までの有限のスナップショットである。
streamer は先頭からの追従配信が閉じた後、または offset 用の初回 Range を読み切った後に、
消費済みバイト位置から次の Range を要求して録画末尾へ追従する。
録画先頭からの読み捨ては行わない。録画終了との競合で 416/空の 206 と終了状態を
同時に観測した場合は、同じ位置へすぐ Range を送り直す。最終 Range が空になるまで
続けるため、終了直前に追記された差分を読み残さない。status は空 Range の後だけ取得する。
未知の status は終了とみなさず、同じ offset で再試行する
（`TestRecordFollowReaderUnknownStatusRetriesInsteadOfEnding`）。

Range を無視してオフセット付き要求に 200（先頭からの本文）を返す mirakc は安全のため
受け付けず、オフセット再生を 503 にする。Range 対応は mirakc の録画配信 API における
seek + finite response の契約が必要である。現在の運用対象はこの契約を含む
`mirakc 4.0.0-dev.0` 系（実際に配備するイメージは同じ API 契約を満たす版に固定する）で、
古い版では `{offset}` を使わず従来の URL を使う。録画ファイルがまだ 0 バイトなら 204、
追従中に現在位置が末尾へ到達したら 416/空の 206 を返し得るため、いずれも録画終了まで
一定時間待って再試行する。起動待ちの上限（既存の playlist 起動上限 15 秒）を超えたとき
だけ 503/504 とする。204/416 の待機は上流接続失敗として数えない。

オフセットが現在の録画可能範囲以上なら新しいセッションを作らず 416 を返す。録画開始時刻・
コンテンツ長からの初期位置は可変ビットレート等の影響を受ける概算であり、開始後は通常の
HLS シークで補正できる。許容誤差は放送・エンコーダーごとに異なるため、固定値を API 契約
として保証しない。

追っかけの ffmpeg は通常ライブの「直近だけを残す」HLS と異なり、`EVENT` playlist、
`hls_list_size=0`、`temp_file` を使い、`delete_segments` を使わない。ffmpeg は stdin の EOF で
`ENDLIST` を書き、stdin が開いている間と kill されたときは書かない。本物の ffmpeg で
`TestBuildChaseFFmpegArgs_RealFFmpegEndlistOnlyAtStdinEOF` が測り、CI は ffmpeg を入れて skip を禁じている。
したがって追っかけの `ENDLIST` が録画ファイルの終端を表すかは、入力をいつ EOF にするかで決まる。

- **EOF にするのは録画ファイルの終端まで渡した後だけ。** mirakc が既知の終了状態
  （`finished` / `canceled` / `failed`）を返し、同じ offset への最後の Range も空だったときである。
  追っかけ再生では canceled / failed のときも録れた所まで配信して ENDLIST を付ける。record が 404 になったとき（ingest の purge 等）は、
  読んだ位置がコミット済み原本のバイト数と一致するときだけ終端とみなす。ingest は mirakc が録画の終了を返し、最後の Range が
  空になるまで読んでからコミットするので、コミットされたバイト数は終了時点のファイル長である。一致しなければエラーにする。
  ffmpeg が先端より遅れていて purge が先に来た場合や、別経路の原本、mirakc が record を失った場合である。
  判定は `TestChaseRecordNotFoundUsesCommittedOriginal` と
  `TestChasePurgeEndUsesCommittedOriginal` が固定する
- **先頭からの追従配信が閉じても EOF にしない。** 追従配信は mirakc 側の無入力タイムアウトで録画中にも
  閉じうる（実際に閉じる頻度は未検証）。正常に閉じても途中で切れても、読んだバイトの続きから Range で追う
  （`TestFollowChaseRecordContinuesWithRangeAfterFollowCloses`）。Content-Length の無い Range 本文も読む。
  Range の本文が 30 秒 stall したら `Read` 中だけ timer を動かして切り直す。ffmpeg の stdin が詰まり
  reader の `Read` が呼ばれない間は timer を動かさない
  （`TestRecordFollowReaderStallAndConsumerBackpressure`）。
- **一過性の失敗は再試行する。** 5xx・通信断・本文の stall・未知の status を共通の規則で扱う。
  連続 5 回まで再試行し、6 回目でエラーにする（`MaxConsecutiveRetries`）。再試行待ちは 200ms から倍々で、
  5 回目の前が最長 3.2 秒である（`RetryDelay`、`TestRetryDelay`）。
- **Range の間隔。** 通常の要求は 0.5 秒以上あける。追い付いたまま録画中なら、間隔を 1 秒まで広げる。
  データを読み取った後は最短間隔へ戻す。終了 status の後の最終 Range はすぐに送る。
- **入力のエラーでは `ENDLIST` を書かせない。** 対象は再試行の上限を超えた失敗、再試行しても変わらない失敗、
  終端と確かめられない 404 である。このときは ffmpeg を kill してから stdin を閉じる
  （`TestChaseInputErrorDoesNotWriteEndlist`）。
  stdin のパイプと写しは streamer が持つ。os/exec に任せると入力のエラーでもパイプが閉じられ、
  ffmpeg はそれを正常な EOF と区別できない。ffmpeg 自身が異常終了したときに `ENDLIST` を書くかは未検証

入力が EOF になって ffmpeg が**正常終了した場合**は、idle GC が回収するまで
playlist と全セグメントを保持する。これにより、録画完了直後にブラウザが最後の playlist /
segment を取りに来る窓を失わない。保持中は全プロファイルのプレイリストが残るので、
終了後でも再起動なしに `?profile=` を切り替えられる
（`TestFinishedChaseProfileSwitchServesRetainedPlaylists`）。ffmpeg が異常終了した場合（kill を含む）は
壊れたセッションを保持せず、map とファイルを直ちに解放する。mirakc からの入力読み取りエラーで
ffmpeg を止めた場合は、録画 ID ごとに 10 秒間の再作成 cooldown を記録する。
期間中は、その録画の
新しい追っかけセッションを作成しない。すでに存在する健全な offset のセッションは引き続き共有できる。
新しいセッションが必要な playlist 要求には `502 Bad Gateway` を返す。本文は
`追っかけ入力のエラーが続いているため、再作成を一時停止しています。しばらく待ってから再読み込みしてください。`
で、残り秒数を `Retry-After` に入れる。
フロントはこの `Retry-After` の有無で、streamer の cooldown 応答と、streamer の停止・再起動中に nginx や ingress が返す 502 を区別する。プロキシの 502 には `Retry-After` が付かないので、入力失敗として扱わず再生元の選び直しに回せる。
`502` は上流入力の失敗を示すため、チューナーやセッション数の不足を
表す `503 Service Unavailable` とは分類を分ける。cooldown が切れた後の最初の要求は新しいセッションを試し、
入力が回復していれば通常どおり再生できる。再度失敗した場合は cooldown を更新する。
失敗記録とセッションの map からの削除は同じロック区間で行うため、同時要求も新しいセッションを作らない。
通常の異常終了（ffmpeg 自身のクラッシュ）や idle GC は入力失敗として記録しない
（`TestChaseIdleGCIsNotRecordedAsInputFailure`）。
cooldown は、hls.js の再試行が尽きるまでの時間より長くとる。
再生中の失敗は、hls.js のプレイリスト再取得が規定の再試行を使い切って fatal になる順で進む。
その後に再生元を選び直した probe が 502 を受ける。
hls.js 1.7.3 の `playlistLoadPolicy.default.errorRetry` は、
`maxNumRetry: 2`・`retryDelayMs: 1000` と既定の指数 backoff を持つ。
待ちは 1 秒と 2 秒の計 3 秒前後である。
cooldown がこれより短いと、hls.js の再取得自体が先頭から作り直しを起こす。
10 秒はその 3 倍強の余裕で、実測した値ではない（未検証）。
finished の録画でも、原本行がなく取り込み前なら新しいセッションを開始できる。
取り込み済みで保持セッションも無い要求は 404 になる
（`TestFinishedUncommittedChaseUsesOriginalRowExistence`）。

ライブと追っかけのセッション数は合算し、Prometheus の
`rokuban_live_active_sessions{kind="live"|"chase"}` で内訳を見る。セグメントの保存先は
どちらも `live.segment_dir` 配下で、録画バッファとは別の tmpfs / scratch に置く。追っかけ
は録画時間ぶんのセグメントを idle GC まで保持するため、同時視聴数と録画時間に応じた
容量を見積もる。詳細は [operations.md](../../operations.md) §5 を参照する。

### 録画原本のブラウザ再生

完了済みで active な原本 MPEG-2 TS を、site streamer が FFmpeg → HLS の一時セッションとして
変換する。これは `media_assets` に保存する派生物ではない。資源同定は
`(recordings.id, offset)` で、profile は出力 playlist の選択にだけ使うため、同じ録画・同じ
offset の視聴者と profile 切替は 1 本の FFmpeg セッションを共有する。省略 offset と `0` は
同じ先頭セッションである。live / chase と同じ `live.max_sessions`、idle GC、離脱ヒントを使う。
Prometheus は `rokuban_live_active_sessions{kind="original_vod"}` に内訳を出す。

```
GET  /api/sites/{site}/recordings/{id}/original-vod/playlist.m3u8[?profile=<name>]
         → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/playlist.m3u8[?profile=<name>]
         → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/original-vod/segments/{name}
GET  /api/sites/{site}/recordings/{id}/original-vod/{name}
         → video/mp2t / text/vtt / application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/segments/{name}
GET  /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/{name}
         → video/mp2t / text/vtt / application/vnd.apple.mpegurl
POST /api/sites/{site}/recordings/{id}/original-vod/leave
         → 204（離脱のヒント）
POST /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/leave
         → 204（離脱のヒント）
```

`{offset}` は録画先頭からの秒を表す正準な 10 進整数である。`007`、`+5`、`5.0` などの
非正準形は 400、原本の長さ以上は 416 にする。offset ごとに session key と HLS scratch を分ける。
offset 付き playlist / segment / leave はすべて同じ offset の session を参照する。
各 offset は通常の原本 HLS セッションとして `live.max_sessions` に数え、leave ヒント・idle GC・
容量圧力時の idle session 退避も共有する。`offset/0` と offset 無しの URL は同じ session と scratch
を使う。

これらのバイナリ配信ルートは `openapi.yaml` に載せない。開始時に DB から同じ site の
`finished` 録画と `state='active'` の original を引く。原本を read-only で open してから、
同じ asset ID がまだ active で `rel_path` も一致することを DB で再確認する（open-then-verify）。
一致しなければ 404 にする。`rel_path` lock は取らず、streamer は media に何も書かない。

lock が要らない根拠は 3 つある。original の canonical は行の commit より前に rename で置かれる。
unlink は `MarkMediaAssetDeleting` の commit より後にしか起きない。live な行がある `rel_path` へは
別の書き手が公開できない（`media_assets_rel_path_idx`）。したがって再確認で active なら、
open した inode はその行の原本である（`TestOriginalVODVerifiesDBTargetAfterOpen`）。
trash・purge・supersede・失敗・原本不在・別 site / 未束縛 site は 404 にする。
その場合、セッションや DB 行を作らない。

開始後のセグメント要求は asset の状態を見ない。見るのは利用者の操作である録画のごみ箱・purge・
supersede だけで、該当すれば 404 にしてセッションと scratch を回収する。
FFmpeg は開いた inode を読み続ける。
エンコード完了直後の `until_encoded` 削除で canonical path が unlink されても、
視聴中の再生は止まらない。
テストは `TestOriginalVODRetainedSessionSurvivesOriginalDeletion` である。
この確認で DB が `ErrNoRows` 以外のエラーを返したときは、警告を出して配信を続ける。
再生を DB の可用性に依存させないためである。

FFmpeg は既存の live profile、音声 rendition、任意の WebVTT 字幕設定を使う。
**playlist は `-hls_playlist_type event` にする。** `vod` は playlist を FFmpeg の終了時にしか書かない。
ffmpeg 9.0.2 で 20 秒の MPEG-2 TS を実時間で流すと、vod は変換中 `.m3u8` が 0 個で終了後に初めて出た。
この形だと、15 秒で変換が終わらない録画はすべて playlist 待ちの 504 になる。
event は変換の先頭から playlist が書かれ、終了時に `#EXT-X-ENDLIST` が付く。
`-hls_list_size 0` と `temp_file` を使い、`delete_segments` は付けない。
変換中のシーク可能範囲は変換の先端まで伸びていき、末尾まで届くのは変換の終了後になる。
正常終了後も、`#EXT-X-ENDLIST` と全 segment を idle GC まで保持する。
シークや遅れて届く segment 要求には、この保持したファイルで応える。
異常終了時は scratch とセッションを回収する。
原本が引き続き active なら次の要求で作り直せる
（`TestOriginalVODServesPlaylistWhileFFmpegIsStillConverting`）。

offset 付きでは完成済み原本の「映像の長さ」を同じ開いたファイル記述子から ffprobe で調べ、FFmpeg に渡す前に
範囲を確認する。原本は `cmd.ExtraFiles` の先頭として子プロセス fd 3 に渡し、FFmpeg は
`/dev/fd/3` を入力にする。fd は seek 可能な同じ inode を指すため、`until_encoded` が canonical path
を unlink した後も変換できる。通常のパイプ `pipe:0` では seek が効かないため、この fd 方式を選ぶ。
ffprobe が descriptor を読み終わった後は親側で先頭へ戻し、FFmpeg の fd も seek 可能な状態から始める。
offset が 0 より大きいときは `-ss {offset}` を `-i /dev/fd/3` より前に置く。FFmpeg の入力側 seek は
入力の seek point から offset までを decode して捨てる。`-copyts` は付けず、HLS の再生時間軸は
offset ごとに 0 から始めるので、再生位置は `offset + currentTime` として扱う。

**範囲の判定は format の duration でなく映像ストリームの終端で行う。**
format の duration は音声など最長のストリームで決まり、映像より長い。
実バイナリで測った合成 660 秒 TS は format 660.010 秒・映像 660.000 秒で、
format と比べる判定では、`[映像の終端, format の duration)` に入る整数 offset（660）が通った。
FFmpeg は何も出力せず、playlist 待ちの 15 秒後に 504 になった。
いまは映像の終端（`start_time + duration - format の start_time`）の手前 0.5 秒より後ろの offset を 416 にする。
終端ちょうどの offset も最後のフレームより後ろを指して出力が空になりうるので、この余白を取る。
同じ 660 秒 TS で offset 659 は 200（0.14 秒）、660 / 661 / 99999 は 416（0.03 秒）だった。
範囲外の offset は利用者入力の結果なので ERROR ではなく INFO で記録する。

**原本 VOD の退避は mirakc のチューナー解放待ち（`liveMirakcReleaseWait`、5 秒）を挟まない。**
原本 VOD はチューナーを持たない。`max_sessions: 2` で容量が埋まった状態からのシークは、
待ちを挟むと約 5.4 秒、挟まないと 0.13 秒だった（実バイナリ、合成 660 秒 TS）。

#### offset seek の精度と開始時間の測定

精度と範囲は CI の `TestOriginalVODOffsetRealFFmpegSeekAccuracyAndRange` が固定する。
このテストは実 ffmpeg / ffprobe で `runSession` の経路を通り、`ROKUBAN_REQUIRE_FFMPEG` を立てると skip できない。入力は lavfi で作る 40 秒の MPEG-2 + MP2 の TS で、
映像の輝度が録画先頭からの秒に比例し、音声は 41.5 秒まで続く。HLS の先頭セグメントの先頭フレームの輝度から時刻を読む。
FFmpeg 9.0.2、macOS arm64 で offset 0 / 7 / 20 / 38 の誤差はすべて 0.00 秒だった。
offset 40 / 41 / 42 は 416 が 3 秒以内に返る。
format の duration と比べる判定に戻すと、40 と 41 が 15 秒待って 504 になった（変異で確認）。
この条件は 40 秒の合成映像だけで、PTS の不連続・wraparound を含む放送 TS の実録画では未検証である。

HLS の先頭セグメントの映像 PTS は、どの offset でも 1.48 秒だった（上のテストのログ）。
mpegts muxer の既定の遅延で、offset によらない。ブラウザの `currentTime` は 0 から始まる。
HLS の PTS 自体を 0 にそろえているわけではない。

10 分以上の入力での精度は 660 秒の合成 TS で 1 回測った。
25 fps の 320×80 映像に録画先頭からの `秒:フレーム番号` を焼き込んだ。
映像は MPEG-2（GOP 15、B-frame 2、closed GOP）、音声は MP2 で、1 Mbit/s MPEG-TS muxrate でまとめた。
format duration は 660.010022 秒、video stream duration は 660.000000 秒である。
測定に使った runner は不変条件 4（ffmpeg の exec は worker / streamer のみ）を破るため、リポジトリには置いていない。
再実行できる形では残っていない。

| 要求した offset | HLS の先頭フレーム表示 | offset との差 |
| ---: | ---: | ---: |
| 0 秒 | 0.00 秒 | 0.00 秒 |
| 61 秒 | 61.36 秒 | +0.36 秒 |
| 307 秒 | 307.32 秒 | +0.32 秒 |
| 603 秒 | 603.20 秒 | +0.20 秒 |
| 659 秒 | 659.36 秒 | +0.36 秒 |

同じ 5 offset を各 3 回 FFmpeg で変換し、原本 open 後から master playlist が読めるまでを測った。
非ゼロ offset は duration probe と FFmpeg 起動を含む。DB lookup、HTTP/router、原本 open は計時外である。

| offset | master ready 中央値（最小–最大） |
| ---: | ---: |
| 0 秒 | 80 ms（78–93 ms） |
| 61 秒 | 94 ms（92–96 ms） |
| 307 秒 | 93 ms（93–93 ms） |
| 603 秒 | 93 ms（93–95 ms） |
| 659 秒 | 59 ms（59–60 ms） |

非ゼロ offset の duration probe は 17–20 ms だった。659 秒では残り 1 秒の短い出力になる。

原本だけの完了録画はこの経路をブラウザ再生の既定にする。active な録画は既存の chase、
active な encoded がある完了録画は MP4 progressive + Range を使う。原本 MPEG-2 decoder を
持たない FFmpeg build は `live.enabled` の streamer 起動前に拒否し、再生要求後に空の HLS
player だけが出る状態を避ける。

原本 VOD の HLS scratch も `live.segment_dir` に置き、録画バッファ / archive から分離する。
正常終了後も idle GC まで録画全体の変換済み segment が残るため、録画時間、全 live profile の
出力 bitrate、同時セッション数を掛けて tmpfs 容量を見積もる（[operations.md](../../operations.md)
§5）。
