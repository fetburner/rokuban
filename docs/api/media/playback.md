> [media.md](../media.md) の録画・追っかけ再生。索引から辿る

### 録画中の追っかけ再生

録画中の番組を、録画開始位置から現在の録画末尾まで HLS で追従再生する。ライブ視聴とセッション管理・最大セッション数・idle GC・離脱ヒントを共有し、資源の同定子は録画の durable id である。

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
<details><summary>根拠</summary>

- ライブ視聴と共有するセッション管理: `LiveStreamer`、`live.max_sessions`

</details>

#### 開始できる条件と URL

追っかけの URL は、録画ファイル配信と同じ理由で OpenAPI に載せない（[recording.md](recording.md)「OpenAPI には載せない」）。新しいセッションを開始できるかは、同期行の状態と原本の行の有無から逆引きして決め、原本の行がある録画は新しく開始しない。

`{id}` は `recordings` の行の id の十進正準形である。同期行の `(site, record_id, status)` と、原本のアセット行の有無を逆引きして、セッション可否を決める。

| 状態 | 挙動 |
|---|---|
| 同期行と録画行が `recording` / `finished` で、原本行が無い | 新しいセッションを開始できる |
| 原本行がある（`state` を問わず、`deleted` を含む） | 新しいセッションを開始しない |
| 録画が正常終了し、既にセッションがある | EVENT playlist とセグメントを idle GC まで取得できる |
| ごみ箱、commit 済みで保持セッション無し、失敗、未束縛、存在しない id、または URL の `site` と DB の録画 site が不一致 | 404 |

`deleted` の原本行も ingest が commit した事実なので、新しいセッションを開始しない理由になる。URL の `site` は site 束縛へルーティングするための値であり、DB の録画 site と一致させる。

<details><summary>根拠</summary>

- 録画 id の正準形: `recordings.id`
- 同期行: `record_sync`
- 原本のアセット行: `kind='original'` の `media_assets`
- site 束縛へのルーティング: `cmd/rokuban`

</details>

#### mirakc からの読み取り

mirakc へは、録画先頭から追従する経路で要求する。offset 付きでは概算した位置から Range で読み、読み切るたびに、消費済み位置から次の Range を要求する。

mirakc へは `GET /api/recording/records/{record_id}/stream` を Range なし・優先度ヘッダーなしで要求する。これは offset を省略したときに録画先頭から追従する経路である。

`{offset}` は録画開始からの 0 以上の整数秒で、省略時は `0` と同じである。offset 付きでは `GET /api/recording/records/{record_id}` の `recording.startTime` と `content.length` から概算位置を求める。TS パケット境界に合わせて `Range: bytes=<position>-` を送る。

mirakc の Range 応答は要求時点までの有限 snapshot である。先頭からの追従配信が閉じた後、または初回 offset Range を読み切った後に、消費済み位置から次の Range を要求する。録画先頭からの読み捨てはしない。入力ループ全体の状態遷移は「EOF と ENDLIST の決め方」の図に示す。

録画終了と競合して 416 / 空の 206 と終了状態を同時に見た場合は、同じ位置へすぐ Range を再送する。最後の Range が空になるまで続け、終了直前の追記を読み残さない。status は空 Range の後だけ取得し、未知の status は終了とみなさず同じ位置で再試行する。

<details><summary>根拠</summary>

- 未知 status は終端とみなさず、同じ offset で再試行する: `TestRecordFollowReaderUnknownStatusRetriesInsteadOfEnding`
- follow stream の終了後に Range を続ける: `TestFollowChaseRecordContinuesWithRangeAfterFollowCloses`

</details>

#### offset 付き再生の前提と範囲

offset 付き再生は、mirakc の録画配信 API が Range の seek と有限の応答に対応していることを前提にする。Range を無視してオフセット付き要求に 200（先頭からの本文）を返す mirakc は、安全のため受け付けず、オフセット再生を 503 にする。

現在の運用対象はこの契約を含む `mirakc 4.0.0-dev.0` 系（実際に配備するイメージは同じ API 契約を満たすバージョンに固定する）で、古いバージョンでは `{offset}` を使わず従来の URL を使う。

録画ファイルがまだ 0 バイトなら 204、追従中に現在位置が末尾へ到達したら 416/空の 206 を返し得るため、いずれも録画終了まで一定時間待って再試行する。起動待ちの上限（既存の playlist 起動上限 15 秒）を超えたときだけ 503/504 とする。204/416 の待機は上流接続失敗として数えない。

オフセットが現在の録画可能範囲以上なら、新しいセッションを作らず 416 を返す。録画開始時刻・コンテンツ長からの初期位置は可変ビットレート等の影響を受ける概算であり、開始後は通常の HLS シークで補正できる。許容誤差は放送・エンコーダーごとに異なるため、固定値を API 契約として保証しない。待機と 503/504 の遷移は「EOF と ENDLIST の決め方」の図に示す。

#### EOF と ENDLIST の決め方

追っかけの終端は入力を閉じる時点で決まり、EOF にするのは録画末尾まで読み切ったときだけである。追従配信が閉じただけでは EOF にせず、Range で追い続ける。

```mermaid
stateDiagram-v2
    state "録画先頭から追従配信" as follow
    state "初回 offset Range" as first
    state "消費位置から Range" as range
    state "起動待ち（204 / 初回 Range の 416）" as startup
    state "同じ位置へ即再送" as resend
    state "終了状態を確認" as check
    state "record 404 の照合" as compare
    state "一過性の失敗を再試行" as retry
    state "EOF にして stdin を閉じる" as eof
    state "kill してから stdin を閉じる" as kill
    state "503 / 504" as fail
    [*] --> startup
    startup --> follow : offset 無し
    startup --> first : offset 付き
    startup --> fail : 15 秒の起動上限を超える
    follow --> range : 配信が閉じる
    first --> range : 読み切る
    range --> range : 読み切ったら次の Range
    range --> range : 末尾に追い付いたら（416 / 空の 206）間隔を広げて同じ位置へ
    range --> resend : 416 / 空の 206 と終了状態を同時に見る
    resend --> check : 空 Range の後に status を取得
    check --> eof : finished / canceled / failed
    check --> retry : 未知の status
    range --> compare : mirakc record が 404
    compare --> eof : 読み取り位置が commit 済みサイズと一致
    compare --> kill : 一致しなければエラー
    range --> retry : 30 秒 stall や一過性の失敗
    retry --> range : 連続 5 回まで同じ位置で再試行
    retry --> kill : 6 回目でエラー
    eof --> [*] : ENDLIST を書く
    kill --> [*] : ENDLIST を書かない
```

追っかけは `EVENT` playlist とし、`hls_list_size=0` / `temp_file` を使って `delete_segments` は使わない。ffmpeg は stdin の EOF で `ENDLIST` を書くので、入力を閉じる時点で終端の意味が決まる。stdin が開いたままなら書かず、kill 時も書かない。

EOF にするのは録画末尾まで読んだ後だけである。mirakc が `finished` / `canceled` / `failed` を返し、同じ offset の最後の Range も空になったときに限る。canceled / failed でも録れた所まで配信して `ENDLIST` を付ける。

mirakc record が 404 なら、読み取り位置と commit 済み原本のサイズが一致するときだけ EOF にする。ingest は mirakc が録画の終了を返し、最後の Range が空になるまで読んでから commit する。だから commit されたバイト数は終了時点のファイル長である。一致しなければ終端とせずエラーにする。ffmpeg の遅れ、別経路の原本、mirakc の record 消失では一致しない場合がある。

追従配信が閉じても EOF にしない。mirakc 側の無入力 timeout で録画中にも閉じることがある（頻度は未検証）。正常終了でも途中切断でも消費位置から Range で追い、Content-Length が無い本文も読む。

<details><summary>根拠</summary>

- `ENDLIST` は stdin EOF のときだけ出す: `TestBuildChaseFFmpegArgs_RealFFmpegEndlistOnlyAtStdinEOF`
- この実 ffmpeg 検証は CI で skip を禁じる: `ROKUBAN_REQUIRE_FFMPEG`
- record 404 後の commit 済み原本との照合: `TestChaseRecordNotFoundUsesCommittedOriginal`、`TestChasePurgeEndUsesCommittedOriginal`

</details>

#### 入力エラー時の ffmpeg の止め方

入力エラーでは終端の印を書かせない。再試行上限超過、再試行しても変わらない失敗、終端と確かめられない 404 では、ffmpeg を kill してから stdin を閉じる。

stdin の pipe は streamer が持ち、標準のプロセス実行の close では入力エラーと正常 EOF を区別できない。ffmpeg 自身が異常終了したときの `ENDLIST` は未検証である。

<details><summary>根拠</summary>

- 入力エラーで正常 EOF を書かない: `TestChaseInputErrorDoesNotWriteEndlist`
- stdin の pipe を閉じる標準のプロセス実行: `os/exec`

</details>

#### 再接続と再試行

Range 本文が 30 秒止まったら再接続し、一過性の失敗は連続 5 回まで再試行する。通常の Range 要求は 0.5 秒以上あけ、録画中に末尾へ追い付いたら間隔を広げる。

Range 本文が 30 秒 stall したら再接続する。timer は本文の読み取りを待っている間だけ動かし、ffmpeg stdin の backpressure で reader が呼ばれない間は止める。

5xx、通信断、本文 stall、未知 status は連続 5 回まで再試行し、6 回目でエラーにする。待ちは 200ms から倍々で、5 回目の前が最大 3.2 秒である。

通常 Range 要求は 0.5 秒以上あける。録画中に末尾へ追い付いたら間隔を 1 秒まで広げ、データを読んだ後は最短間隔に戻す。終了 status 後の最後の Range はすぐ送る。状態遷移は「EOF と ENDLIST の決め方」の図に示す。

<details><summary>根拠</summary>

- stall timer と consumer backpressure: `TestRecordFollowReaderStallAndConsumerBackpressure`
- stall timer が待つ本文の読み取り: `Read`
- 再試行回数と delay: `MaxConsecutiveRetries`、`RetryDelay`、`TestRetryDelay`

</details>

#### 入力エラー時のセッションと cooldown

mirakc 入力の読み取りエラーでは、録画 ID ごとに 10 秒の cooldown を置き、新しいセッションの作成を止める。cooldown 中に新しいセッションが要るときは、502 に Retry-After を付けて返す。

```mermaid
stateDiagram-v2
    state "通常" as normal
    state "cooldown 10 秒" as cd
    state "再作成を試す" as recreate
    [*] --> normal
    normal --> cd : mirakc 入力の読み取りエラー
    cd --> cd : 新しい session が必要な要求に 502 と Retry-After を返す。既存の健全な offset session は共有できる
    cd --> recreate : 10 秒が過ぎて次の要求が来る
    recreate --> normal : 再作成できる
    recreate --> cd : 入力失敗が続けば cooldown を更新
    normal --> normal : idle GC や ffmpeg 自身の異常終了は入力失敗として記録しない
```

cooldown 中に新しい session が要る要求には `502 Bad Gateway` を返し、残り秒数の `Retry-After` を付ける。502 の本文は `追っかけ入力のエラーが続いているため、再作成を一時停止しています。しばらく待ってから再読み込みしてください。` である。cooldown の記録と session map からの削除は同じ lock 区間で行うため、同時要求も新しい session を作らない。

`502` と `Retry-After` の組み合わせで streamer の cooldown と proxy の 502 を区別する。proxy の 502 にはこの header が無く、入力失敗として扱わず再生元を選び直せる。上流入力の失敗を示す 502 は、チューナーや session 数不足を示す `503 Service Unavailable` と分類を分ける。

再生中の失敗では、hls.js の playlist 再試行が尽きて fatal になった後、再生元選択の probe が 502 を受ける。cooldown が 3 秒より短いと、再生元を選び直す前に hls.js の再試行が新しい session を作ることがある。

```mermaid
sequenceDiagram
    participant H as hls.js
    participant S as streamer
    participant P as 再生元選択の probe
    H->>S: playlist 要求
    S-->>H: 失敗
    Note over H: 1 秒待つ
    H->>S: 再試行 1 回目
    Note over H: 2 秒待つ
    H->>S: 再試行 2 回目
    Note over H: 合計 3 秒前後で再試行が尽き fatal
    P->>S: probe
    S-->>P: 502
```

hls.js 1.7.3 の既定は再試行 2 回・初回待ち 1 秒・指数 backoff なので、待ちは 1 秒と 2 秒の計 3 秒前後になる。10 秒はこの約 3 秒より長く、3 倍強の余裕を取る値である。実測値ではない（未検証）。

<details><summary>根拠</summary>

- idle GC や ffmpeg crash は入力失敗にしない: `TestChaseIdleGCIsNotRecordedAsInputFailure`
- hls.js 1.7.3 の設定箇所: `playlistLoadPolicy.default.errorRetry`
- 再試行の設定: `maxNumRetry: 2`、`retryDelayMs: 1000`

</details>

#### 終了後のセッション

ffmpeg が入力 EOF の後に正常終了したら、idle GC まで playlist と全 segment を保持する。異常終了したとき（kill を含む）は、使えなくなった session とファイルを直ちに解放する。

正常終了の後は、既存の playlist から `?profile=` を切り替えられる。録画が finished でも ingest 前なら、原本行が無い間は新しい session を開始できる。ingest 後で保持 session が無ければ 404 である。

<details><summary>根拠</summary>

- 正常終了後の profile 切替: `TestFinishedChaseProfileSwitchServesRetainedPlaylists`
- finished / ingest 前の新規 session: `TestFinishedUncommittedChaseUsesOriginalRowExistence`

</details>

#### セッション数と容量

ライブと追っかけのセッション数は合算する。セグメントは録画バッファとは別の tmpfs / scratch に置き、録画時間ぶんが idle GC まで残るので、容量は同時視聴数と録画時間から見積もる。

最大セッション数 `live.max_sessions` はライブと共有する。内訳は Prometheus の `rokuban_live_active_sessions{kind="live"|"chase"}` で見る。セグメントの保存先はどちらも `live.segment_dir` 配下である。追っかけは録画時間ぶんのセグメントを idle GC まで保持するため、同時視聴数と録画時間に応じた容量を見積もる。詳細は [operations.md](../../operations.md) §5 を参照する。

#### 録画中 temp の追従と offset の時刻基準

録画中の追っかけは ingest の temp を追記入力にする。seek 位置は映像の先頭 PTS を原点にする。実際の共有 PVC 上で追記の可視性と FFmpeg の追従を測るまでは、この入力方式を運用結果で確認した扱いにしない。

##### 実 PVC の追記可視性

共有ストレージで追記の可視性を確認できていない。

実 PVC の追記遅延は未測定である。既存 Pod からの `stat /mnt/media` は 5 秒以内に戻らず、別ノードへ作った read-only probe Pod も 30 秒以内に Ready にならなかった。これは NFS の可視遅延ではなく、stat 完了と Pod 起動のタイムアウトである。中央値・最大値と 4 分 slice 境界の見え方は得られていない。

測定するときは、運用中の media PVC と同じ StorageClass に、追記 Pod と読み取り Pod を別ノードで置く。読み取り側は temp の `stat` と末尾の読み取りを短い間隔で記録し、書き手の時刻と比較する。4 分後に別 Pod が同じ temp を `O_APPEND` で開く境界まで続けて測る。PVC が読み取れない状態では遅延値を記録しない。

##### 伸長中の FFmpeg 入力

合成入力は追記完了後に終了したが、共有ストレージ上の挙動は未検証である。

streamer イメージの FFmpeg 7.1.5 で、160×90 / 25 fps の合成 MPEG-TS を `/dev/shm` に 8 秒間書き、2 秒後に別プロセスから読んだ。reader は `-follow 1 -rw_timeout 2000000 -ss 1` で開始し、2 秒 segment の HLS を出した。書き手終了後 2,003 ms で終了し、終了コード 0、segment 4 本、`ENDLIST` ありだった。終了時に demux の I/O error がログへ出たが、playlist は閉じた。

同じ入力で `rw_timeout=10000000` とし、書き手終了後に FFmpeg へ SIGINT を送ると、終了まで 9,502 ms かかり、終了コード 255、`ENDLIST` ありだった。SIGINT は file read の待ちを早めなかった。この測定では `rw_timeout` が終了待ちの上限になった。どちらも tmpfs の結果であり、NFS 上の file read は未検証である。

##### 先頭 audio PTS が古い入力

format の先頭時刻を使う直接 seek は、この入力で目標フレームを選ばなかった。

FFmpeg 7.1.5 で 30 秒の合成 MPEG-TS を作った。video と主音声を 5,400 秒ずらし、別音声の最初の PES を 0 秒から置いた。`-mpegts_copyts 1` で保存した結果、format の start time は 0 秒、video の先頭 PTS は 5,400.010022 秒、別音声の先頭 PTS は 0 秒だった。要求 offset 10 秒の目標 PTS は 5,410.010022 秒である。

直接の `-ss` はどちらも目標から外れた。フレーム誤差は 25 fps で数えた。`-copyts` の有無で同じ decoded frame が選ばれることは framehash で確認した。

| 入力 seek | `-copyts` | 先頭 video PTS | 目標との差 |
|---|---:|---:|---:|
| `-ss 10` | なし / あり | 5,400.010022 秒 | −250 frame |
| `-seek_timestamp 1 -ss 5410.010022` | なし / あり | 5,410.690022 秒 | +17 frame |
| 2 秒 pre-roll + `trim` | あり | 5,410.010022 秒 | 0 frame |

測定した合成 GOP では、目標より 2 秒前を入力 seek し、`trim=start=5410.010022` で目標 PTS より前を落とし、`setpts=PTS-STARTPTS` で HLS 時間を 0 に戻すと一致した。対応する引数は `-copyts -seek_timestamp 1 -ss 5408.010022` と `trim=start=5410.010022,setpts=PTS-STARTPTS` である。pre-roll の 2 秒はこの合成 GOP だけで確認した値で、放送 TS の GOP に対する上限ではない。

`-ss 10` は 30 秒すべて（2 秒 segment 15 本）を出した。直接の絶対時刻 seek は 20 秒（10 本）を出した。`-copyts` を付けると前者の HLS 先頭 PTS は 5,401.400 秒、後者は 5,412.080 秒になった。`-copyts` を付けない出力はどちらも 1.440 秒から始まった。seek 位置の正しさと muxer の出力 PTS は別に確認する。

##### 33 bit PTS wrap

実録画は読めず、wrap を含む合成入力で seek を測った。

実録画は PVC から読めなかったため、`-output_ts_offset 95430` を付けた 20 秒の合成 MPEG-TS を使った。PTS の wrap は `2^33 / 90000 = 95,443.717689` 秒である。`ffprobe -show_packets` は映像 500 packet の PTS を −13.717689 秒から 6.282311 秒まで出し、範囲 0 秒を通過した。これは raw PTS が wrap する位置を含む。

wrap を含む入力の offset 10 秒では、映像原点は −13.717689 秒、目標は −3.717689 秒だった。直接 seek は `-copyts` なしで 92 frame、ありで 17 frame 遅れた。2 秒 pre-roll と目標 PTS の `trim` を使うと差は 0 frame だった。

##### segment PTS 格子

測定した HLS segment の先頭 PTS は、両入力で 2 秒間隔の格子に一致した。

pre-roll と `trim` の後に `setpts=PTS-STARTPTS` を行った HLS 出力で、segment 時間 `D=2` 秒、25 fps の先頭映像 PTS を比べた。原点は segment 0 の先頭 PTS 1.440 秒である。PTS が wrap する入力も、測った segment の格子ずれは 0 frame だった。

| segment n | 古い audio PTS | PTS wrap |
|---:|---:|---:|
| 0 | 1.440 秒 / 0 frame | 1.440 秒 / 0 frame |
| 1 | 3.440 秒 / 0 frame | 3.440 秒 / 0 frame |
| 2 | 5.440 秒 / 0 frame | 5.440 秒 / 0 frame |
| 3 | 7.440 秒 / 0 frame | 7.440 秒 / 0 frame |
| 4 | 9.440 秒 / 0 frame | 9.440 秒 / 0 frame |
| 5 | 11.440 秒 / 0 frame | — |
| 6 | 13.440 秒 / 0 frame | — |
| 7 | 15.440 秒 / 0 frame | — |
| 8 | 17.440 秒 / 0 frame | — |
| 9 | 19.440 秒 / 0 frame | — |

実録画での seek 精度、実 PVC 上の追記遅延、4 分境界、NFS 上での `-follow 1` 終了は未検証である。上の PTS 結果は FFmpeg 7.1.5 と合成入力に限る。

### 録画原本のブラウザ再生

完了済みで active な原本 MPEG-2 TS を、site streamer が FFmpeg で HLS の一時セッションに変換して再生する。変換結果は派生物として保存しない。同じ録画・同じ offset の視聴者と profile 切替は 1 本のセッションを共有する。

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
#### 資源の同定とセッションの共有

セッションは録画と offset で同定し、profile は出力 playlist の選択にだけ使う。同じ録画・同じ offset の視聴者は 1 本のセッションを共有する。省略した offset と 0 は同じ先頭セッションで、ライブ・追っかけと同じ最大セッション数・idle GC・離脱ヒントに従う。

これらのバイナリ配信ルートは `openapi.yaml` に載せない。Prometheus は `rokuban_live_active_sessions{kind="original_vod"}` に内訳を出す。

`{offset}` は録画先頭からの秒を表す正準な 10 進整数である。`007`、`+5`、`5.0` などの非正準形は 400、原本の長さ以上は 416 にする。offset ごとに session key と HLS scratch を分ける。offset 付き playlist / segment / leave はすべて同じ offset の session を参照する。各 offset は通常の原本 HLS セッションとして `live.max_sessions` に数え、leave ヒント・idle GC・容量圧力時の idle session 退避も共有する。`offset/0` と offset 無しの URL は同じ session と scratch を使う。

<details><summary>根拠</summary>

- セッションの同定子: `(recordings.id, offset)`

</details>

#### 原本の確認と open-then-verify

開始時に DB で同じ site の完了録画と active な原本を引き、原本を読み取り専用で開く。その後、同じアセットがまだ active で相対パスも一致することを DB で再確認し、一致しなければ 404 にする。

相対パスの lock は取らず、streamer は media に何も書かない。

lock を取らなくても open した inode と active な原本行を照合できる。

| 順序・制約 | 結果 |
|---|---|
| canonical file は DB 行の commit より前に rename する | active 行の公開時には canonical file が既にある |
| unlink は deleting 状態の commit より後に行う | active 行を確認した inode が並行削除で別 file にならない |
| live 行がある `rel_path` は他の書き手が公開できない | 同じパスの別アセットにすり替わらない |

したがって再確認時に active なら、open 済み inode はその行の原本である。trash・purge・supersede・失敗・原本不在・別 site / 未束縛 site は 404 にし、その場合は session や DB 行を作らない。

<details><summary>根拠</summary>

- 引く録画と原本の条件: `finished`、`state='active'`
- open-then-verify で inode と DB 対象を照合: `TestOriginalVODVerifiesDBTargetAfterOpen`
- live な行がある `rel_path` へ別の書き手が公開できない: `media_assets_rel_path_idx`
- unlink は deleting 状態の commit より後にしか起きない: `MarkMediaAssetDeleting`

</details>

#### 開始後の要求と DB エラー時の継続

開始後の segment 要求は、利用者の操作である録画の trash・purge・supersede だけを見る。encode 完了後に原本が消えても、視聴中の再生は止まらない。録画の行が見つからない場合を除き、再確認で DB エラーが出ても警告を記録して配信を続ける。

asset の状態は見ない。該当すれば 404 にして session と scratch を回収する。

FFmpeg は開いた inode を読み続ける。encode 完了直後に `until_encoded` で canonical path が unlink されても、視聴中の再生は止まらない。

再確認で、行が見つからないとき以外の DB エラーが出たら警告を記録して配信を続け、再生を DB の可用性に依存させない。

<details><summary>根拠</summary>

- 行が見つからないことを表すエラー: `ErrNoRows`
- original unlink 後も開いた inode から再生を継続: `TestOriginalVODRetainedSessionSurvivesOriginalDeletion`

</details>

#### playlist の種類と終了後の扱い

playlist は変換開始から書かれる event 形式にする。vod 形式は FFmpeg の終了時にしか playlist を書かず、変換が長いと playlist 待ちで 504 になる。正常終了後は終端と全 segment を idle GC まで保持する。

FFmpeg は既存の live profile、音声 rendition、任意の WebVTT 字幕設定を使う。playlist は `-hls_playlist_type event` にする。`vod` は FFmpeg の終了時にしか playlist を書かない。ffmpeg 9.0.2 で 20 秒の MPEG-2 TS を実時間変換したとき、変換中の `.m3u8` は 0 個で、終了後に初めて出た。

変換が 15 秒を超えると `vod` は playlist 待ちで 504 になる。`event` は変換開始から playlist を書き、終了時に `#EXT-X-ENDLIST` を付ける。

`-hls_list_size 0` と `temp_file` を使い、`delete_segments` は付けない。変換中の seek 範囲は変換先端まで伸び、末尾に届くのは変換完了後である。

正常終了後は `#EXT-X-ENDLIST` と全 segment を idle GC まで保持し、seek と遅れて届く segment 要求に応える。異常終了時は scratch と session を回収する。原本が active のままなら次の要求で作り直せる。

<details><summary>根拠</summary>

- 変換中に playlist を配信: `TestOriginalVODServesPlaylistWhileFFmpegIsStillConverting`

</details>

#### offset 付きの入力と範囲判定

offset 付きでは、開いたままの原本の fd から映像の長さを調べ、FFmpeg に渡す前に範囲を確認する。範囲の判定は映像ストリームの終端で行う。format の duration は、音声など最長の stream で決まって映像より長くなるので使わない。

原本は子プロセスの fd 3 に渡すため、canonical path が unlink されても同じ seek 可能な inode を読める。通常の `pipe:0` では seek できないため、この方式にする。ffprobe の後に descriptor を先頭へ戻し、FFmpeg も先頭から読めるようにする。offset が 0 より大きいときは `-ss {offset}` を `-i /dev/fd/3` より前に置き、入力側 seek で必要な範囲まで decode して捨てる。

`-copyts` は使わず、HLS 時間軸は offset ごとに 0 から始める。再生位置は `offset + currentTime` として扱う。

映像より長くなる format の duration を使うと、映像の終端より後ろの offset が通ってしまう。

合成した 660 秒の TS で測った。format duration 660.010 秒と比較すると、`[映像の終端, format の duration)` に入る offset 660 が通った。FFmpeg は出力せず、playlist 待ちの 15 秒後に 504 になった。映像の duration 660.000 秒から終端を求め（`start_time + duration - format の start_time`）、その 0.5 秒手前より後ろの offset は 416 にする。offset 659 は 200 で 0.14 秒、offset 660 / 661 / 99999 は 416 で 0.03 秒だった。

終端ちょうどでも最後のフレームより後ろを指して空出力になる可能性があるため、0.5 秒の余白を取る。範囲外 offset は利用者入力なので INFO で記録する。ERROR にはしない。

<details><summary>根拠</summary>

- seek 範囲の判定: `TestOriginalVODOffsetRealFFmpegSeekAccuracyAndRange`
- seekable descriptor を子プロセスへ渡す: `cmd.ExtraFiles`、`/dev/fd/3`

</details>

#### 退避の待ち時間

原本 VOD の退避は、mirakc のチューナー解放待ち（5 秒）を挟まない。原本 VOD はチューナーを持たないからである。

`max_sessions: 2` で容量が埋まった状態からの seek は、待ちありで約 5.4 秒、待ちなしで 0.13 秒だった（実バイナリ、合成 660 秒 TS）。

<details><summary>根拠</summary>

- チューナー解放待ちの定数: `liveMirakcReleaseWait`

</details>

#### 既定の再生経路と容量

原本だけの完了録画は、この原本 VOD の経路をブラウザ再生の既定にする。active な録画は追っかけ、active な encoded がある完了録画は MP4 progressive + Range を使う。

原本 MPEG-2 decoder を持たない FFmpeg build は `live.enabled` の streamer 起動前に拒否し、再生要求後に空の HLS player だけが出る状態を避ける。

原本 VOD の HLS scratch も `live.segment_dir` に置き、録画バッファ / archive から分離する。正常終了後も idle GC まで録画全体の変換済み segment が残るため、録画時間、全 live profile の出力 bitrate、同時セッション数を掛けて tmpfs 容量を見積もる（[operations.md](../../operations.md) §5）。

#### offset seek の精度と開始時間の測定

原本 VOD の offset seek は、合成した TS で精度と開始時間を測った。放送 TS の実録画での値は未検証である。

##### CI の seek 精度

CI の seek 精度確認は、実 ffmpeg / ffprobe と合成した 40 秒の TS で行う。offset 0・7・20・38 秒の誤差はすべて 0.00 秒で、試した範囲外の offset（40〜42 秒）は 416 が 3 秒以内に返る。

CI の seek 精度確認は実 ffmpeg / ffprobe と lavfi 生成の 40 秒 MPEG-2 + MP2 TS を使う。映像の輝度は録画先頭からの秒に比例し、音声は 41.5 秒まで続く。HLS 先頭 segment の先頭 frame の輝度から時刻を読む。

| 要求 offset | 結果（FFmpeg 9.0.2、macOS arm64） |
|---|---|
| 0 / 7 / 20 / 38 | 誤差はすべて 0.00 秒 |
| 40 / 41 / 42 | 416 が 3 秒以内に返る |
| format duration 比較に戻す変異で 40 / 41 | 15 秒待って 504 |

この条件は 40 秒の合成映像だけで、PTS の不連続・wraparound を含む放送 TS の実録画では未検証である。

##### 先頭フレームの PTS

HLS の先頭セグメントの映像 PTS は、どの offset でも 1.48 秒だった。ブラウザの再生位置は 0 から始まる。

上のテストのログで測った。1.48 秒は mpegts muxer の既定の遅延で、offset によらない。ブラウザの `currentTime` は 0 から始まる。HLS の PTS 自体を 0 にそろえているわけではない。

##### 10 分以上の入力での精度

660 秒の合成 TS で、5 つの offset の精度を 1 回測った。先頭フレームの表示は、offset 0 を除いて offset より 0.20〜0.36 秒後ろだった。測定に使った runner は、リポジトリに置いておらず、再実行できる形では残っていない。

10 分以上の入力での精度は 660 秒の合成 TS で 1 回測った。25 fps の 320×80 映像に録画先頭からの `秒:フレーム番号` を焼き込んだ。映像は MPEG-2（GOP 15、B-frame 2、closed GOP）、音声は MP2 で、1 Mbit/s MPEG-TS muxrate でまとめた。format duration は 660.010022 秒、video stream duration は 660.000000 秒である。測定に使った runner は、不変条件 4（ffmpeg の exec は worker / streamer のみ）を破るためリポジトリに置いていない。再実行できる形では残っていない。

| 要求した offset | HLS の先頭フレーム表示 | offset との差 |
| ---: | ---: | ---: |
| 0 秒 | 0.00 秒 | 0.00 秒 |
| 61 秒 | 61.36 秒 | +0.36 秒 |
| 307 秒 | 307.32 秒 | +0.32 秒 |
| 603 秒 | 603.20 秒 | +0.20 秒 |
| 659 秒 | 659.36 秒 | +0.36 秒 |

##### master playlist までの開始時間

同じ 5 offset を各 3 回変換し、原本を開いてから master playlist が読めるまでを測った。中央値は 59〜94 ms で、非ゼロ offset の duration probe は 17〜20 ms だった。

同じ 5 offset を各 3 回 FFmpeg で変換し、原本 open 後から master playlist が読めるまでを測った。非ゼロ offset は duration probe と FFmpeg 起動を含む。DB lookup、HTTP/router、原本 open は計時外である。

| offset | master ready 中央値（最小–最大） |
| ---: | ---: |
| 0 秒 | 80 ms（78–93 ms） |
| 61 秒 | 94 ms（92–96 ms） |
| 307 秒 | 93 ms（93–93 ms） |
| 603 秒 | 93 ms（93–95 ms） |
| 659 秒 | 59 ms（59–60 ms） |

非ゼロ offset の duration probe は 17–20 ms だった。659 秒では残り 1 秒の短い出力になる。

<details><summary>根拠</summary>

- offset seek の精度と範囲: `TestOriginalVODOffsetRealFFmpegSeekAccuracyAndRange`、`runSession`、`ROKUBAN_REQUIRE_FFMPEG`
- 測定 runner をリポジトリに置かない: 不変条件 4

</details>
