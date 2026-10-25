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

ライブ・追っかけ・原本 VOD のセッション数は合算する。セグメントは録画バッファとは別の tmpfs / scratch に置く。ライブは playlist の窓だけを残し、追っかけと原本 VOD は録画全体の EVENT セグメントを idle GC まで保持する。

最大セッション数 `live.max_sessions` は全経路で共有する。内訳は Prometheus の `rokuban_live_active_sessions{kind="live"|"chase"|"original_vod"}` で見る。どの経路も `live.segment_dir` を使う。容量の見積もり式と `sizeLimit` の判断は [operations.md](../../operations.md) §5「ライブのセグメントを録画バッファと同じディスクに置かない」（[k8s-streamer.md](../../operations/k8s-streamer.md)）にある。

#### 録画中の temp を読む方式の測定

録画中のファイルへの追記は、別ノードから見えるまで最大で約 55 秒かかった。映像の時刻を補正した seek は、古い音声や時刻の折り返しを含む実録画で ±1 frame に収まった。

エピックの方式では、録画中の番組は ingest が追記している temp を FFmpeg の入力にし、セグメントを録画時刻上の固定格子にそろえる。現行の追っかけ再生は上の節のとおり mirakc を Range で読む方式のままである。以下は合成入力の先行測定と、実クラスタでの追測である。

合成入力の先行測定では、FFmpeg 9.0.2 を使った。合成入力は 160×90・25 fps・MPEG-2・GOP 2 秒（50 frame）・B-frame なしである。出力は libx264 の lossless にし、出力セグメントの先頭フレームを入力のフレームと framehash で照合した。入力の種類と回数は各表に書く。実クラスタでの追測は FFmpeg 7.1.5 を使った。再実行用の runner と集計結果は `scripts/playback-measure/` にある。

##### 時間軸を録画時刻の格子にそろえる引数

起動位置が違っても、セグメント n の出力 PTS が n·D+C（C は全起動で同じ定数）になる引数を決めた。入力は format の start_time を基準にした相対の seek に pre-roll を付けて読み、フレームの選別と PTS の付け替えをフィルタで行う。

記号は次のとおりである。V は映像の最初の PTS、S は format の start_time で、どちらも `ffprobe` で読んで 90 kHz の整数にする。k は起動するセグメント番号、D はセグメントの秒、P は pre-roll の秒である。例の P90 は P を 90 kHz の整数にした値である。

```sh
# V90 = 映像の最初の PTS、S90 = format の start_time、P90 = pre-roll（0.5 秒なら 45000）。いずれも秒 × 90000 の整数
T90=$((V90 + k*D*90000)); K90=$((k*D*90000))
SS=$(awk -v t=$((T90 - P90 - S90)) 'BEGIN{printf "%.6f", t/90000}')
ffmpeg -copyts -ss "$SS" -i "$INPUT" -map 0:v:0 -an -fps_mode passthrough \
  -vf "select=gte(pts\,$T90),setpts=PTS-$T90+$K90" \
  -c:v libx264 -preset ultrafast -qp 0 -pix_fmt yuv420p \
  -g 50 -keyint_min 50 -sc_threshold 0 -bf 0 \
  -force_key_frames "expr:gte(t,$((k*D))+n_forced*$D)" \
  -f hls -hls_time $D -hls_list_size 0 -start_number $k -hls_flags temp_file \
  -hls_segment_filename 'seg%03d.ts' index.m3u8
```

判定は 2 つである。出力セグメントの先頭フレームは `ffmpeg -i seg -map 0:v:0 -f framehash -hash md5 -` の最初の行を、入力全体の同じ出力の何行目かに照合する。出力 PTS は `ffprobe -select_streams v:0 -show_packets -show_entries packet=pts` の最初の値を使う。

引数ごとの理由は次のとおりである。

- `-copyts` を付けて、フィルタの式を入力の PTS だけで決める。出力の PTS が入力の start_time の扱いに左右されなくなる
- フレームの選別は `trim` ではなく `select` で行う。`trim` は T を含む 1 フレームを残すので、29.97 fps の実録画では T より 555 tick（約 6 ms）早いフレームが先頭に来た
- `-seek_timestamp 1` は使わない。format の start_time が 0 でない入力では、次の「相対 seek と絶対 seek の比較」のとおり出力が空になった

合成入力 3 種で、k=3 と k=5 の 2 回ずつ変換した結果は次のとおりである。P は 2 秒、`-copyts` ありで、各行 1 回の測定である。

| 入力 | format の start_time / V | 起動 k | 出力セグメント n | 先頭フレームの誤差 | 出力 PTS − n·D |
|---|---|---:|---|---:|---:|
| 旧 PES（映像を 5,400 秒進め、別音声を 0 秒に置く） | 0 / 5,400.010022 秒 | 3 | 3〜14（12 本） | 全て 0 frame | 全て 1.400 秒 |
| 同上 | 同上 | 5 | 5〜14（10 本） | 全て 0 frame | 全て 1.400 秒 |
| wrap（映像のみ、PTS が 2^33 をまたぐ） | −13.717689 / −13.717689 秒 | 3 | 3〜14（12 本） | 全て 0 frame | 全て 1.400 秒 |
| 同上 | 同上 | 5 | 5〜14（10 本） | 全て 0 frame | 全て 1.400 秒 |
| 50,000 秒（映像のみ、PTS が 2^32 tick を超える） | 50,000 / 50,000 秒 | 3 | 3〜14（12 本） | 全て 0 frame | 全て 1.400 秒 |
| 同上 | 同上 | 5 | 5〜14（10 本） | 全て 0 frame | 全て 1.400 秒 |

3 種とも、k=3 と k=5 の両方が出した n=5〜14 の出力 PTS は一致した。C は 1.400 秒だった。1.400 秒になる理由は調べていないが、入力と起動位置によらず同じだった。

29.97 fps の実録画では、frame 周期が 2 秒の格子に割り切れないので、出力 PTS − n·D は frame 周期（約 33 ms）に収まる範囲で動く。手元の実録画 2 本（後述）の先頭 420 frame を、320×180 の通常画質で同じ引数に通した。k=3 と k=5 の重なる n=5〜9 の出力 PTS は一致し、n·D との差は 1.406〜1.422 秒で、n が 1 増えるごとに 2 ms 増えた。実録画では出力セグメントの内容の照合までは行っていない。

##### pre-roll の長さ

pre-roll P は入力の GOP の長さ以上にする。seek は要求位置の後ろの keyframe に着くことがあり、その keyframe が T より後ろだと、先頭フレームが格子から外れる。

外れても FFmpeg はエラーにせず、別のフレームから出力する。GOP が 3 秒（75 frame）の合成入力で、旧 PES と wrap の 2 種を各 1 回変換した結果は次のとおりである。2 種の結果は同じだった。誤差は先頭セグメントの先頭フレームで、全セグメントが 0 frame になったものを 0 と書く。

| P | k=1 | k=2 | k=4 |
|---:|---:|---:|---:|
| 0 秒 | +25 frame | +50 frame | +25 frame |
| 1 秒 | +25 frame | 0 | +25 frame |
| 2 秒 | 0 | 0 | 0 |
| 3 秒 | 0 | 0 | 0 |

GOP 2 秒の合成入力では、格子の位置がすべて keyframe なので、P=0 でも 0 frame になった。P=0 が通るのは、この偶然によるものである。実録画 2 本では、k=3 と k=5 の先頭フレームを入力の先頭フレームと照合した。P=0 は 4 回とも外れ、P=0.5・1・2 秒は 12 回とも一致した。

##### 相対 seek と絶対 seek の比較

録画の先頭からの秒をそのまま seek に渡すと、基準が format の start_time になり、映像の開始とずれる。絶対 seek は目標より後ろの keyframe に着く。seek 位置を映像の PTS から start_time を引いた値に換算し、pre-roll とフレーム選別のフィルタを使う前節の方法だけが 0 frame になった。

旧 PES の合成入力で、offset 10.36 秒（格子上にない位置）を要求した結果は次のとおりである。誤差は、出力セグメント n の先頭フレームの、期待した入力フレームとの差で、全セグメントで同じ値だった。`-copyts` なし / ありの順に書く。各 1 回の測定で、旧 PES は format の start_time が 0 秒、V が 5,400.010022 秒である。

| 入力 | `-ss 10.36`（相対） | `-seek_timestamp 1` の絶対 seek | pre-roll 2 秒 + `select` |
|---|---:|---:|---:|
| 旧 PES（出力 15 / 9 / 10 本） | −259 / −259 frame | +41 / +41 frame | 0 frame（`-copyts` あり） |
| wrap（出力 4 / 4 / 5 本） | +41 / +41 frame | +41 / +41 frame | 0 frame（`-copyts` あり） |

旧 PES の相対 seek は、`-ss 10.36` が入力の PTS 10.36 秒を指し、映像の開始（5,400 秒）より前なので、映像の先頭フレームに丸められた。第 2 音声の先頭 PES だけが古い実録画で、原本 VOD の `-ss` が全部先頭に丸められた事例と同じ形である。`-copyts` の有無では選ばれるフレームは変わらなかった。

絶対 seek は、`-copyts` を付けると start_time が大きい入力で出力が空になった。映像のみ 30 秒（GOP 50、B-frame なし）の合成入力で、`-seek_timestamp 1 -ss <start_time + 10.36>` の出力フレーム数を数えた（FFmpeg 9.0.2、各 1 回）。start_time が 5,400 秒と 50,000 秒の入力は、`-copyts` なしで 450、ありで 0 だった。start_time が 0.04 秒の入力は、なし・ありとも 450 だった。wrap（start_time −13.717689 秒）は、なしで 450、ありで 408 だった。この wrap の 408 は相対の `-ss 10.36` でも同じで、理由は調べていない。

実録画 2 本（start_time は約 43,588 秒と約 5,793 秒）でも、`-copyts` ありの絶対 seek は 0 frame だった。同じ実録画に `-ss 6` を渡すと start_time + 6 秒から出た。FFmpeg の文書にない挙動で理由は説明できていないので、`-seek_timestamp` に頼らない。

##### 伸びている入力の追従

伸びている入力は、前節の引数に追従と読み待ちの時間切れの指定を足せば、途中から読み続けられる。書き手が生きている間にセグメントが公開され、先頭フレームの誤差は 0、出力 PTS は格子どおりだった。

24 秒の合成入力（旧 PES と wrap）を、同じバイト列のまま実時間で書き足して測った。書き手は 188 byte の倍数ずつ、バッファなしで一定の速さで追記する。読み手は書き手の開始 8 秒後に k=2・P=2 秒で起動し、`-follow 1 -rw_timeout 2000000` を付けた。ホストの一時ディレクトリでの測定で、PVC 上の挙動は示さない。

入力ごとに 5 回、計 10 回の結果は同じ傾向だった。出力は n=2〜11 の 10 本で、そのうち 9 本は書き手が生きている間に公開された。最初のセグメントは読み手の起動の 0.08〜0.13 秒後に公開された。全セグメントの先頭フレームの誤差は 0 frame、出力 PTS − n·D は 1.400 秒で、全セグメントのフレーム数の合計は期待どおり 500 だった。読み手は書き手の終了の 1.99〜2.01 秒後に exit 0 で終わった。

##### 録画が終わったときの止め方

入力の読み待ちの時間切れで、FFmpeg が正常終了することは確かめた。ただし、終了コードを録画完了の判定に使えない。プロセスへの SIGINT は、先行測定で停止を早めず末尾フレームを失った。

[FFmpeg の文書](https://ffmpeg.org/ffmpeg-protocols.html#file)の file プロトコルの `follow` には、終わらせるには `rw_timeout` か割り込みコールバックが要ると書いてある。SIGINT が読み待ちを中断するとは書かれていないので、契約として頼れない。測定でも SIGINT は不利だった。

| 終わらせ方 | 回数 | 終了コード | 書き手の終了から終了まで | 全フレーム数（期待 500） |
|---|---:|---:|---:|---:|
| 時間切れ（rw_timeout 2 秒） | 10 | 0 | 1,993〜2,005 ms | 500（末尾フレームも一致） |
| SIGINT（書き手の終了直後に送る。rw_timeout 10 秒） | 10 | 255 | 9,986〜10,006 ms | 498（末尾 2 フレームが欠けた） |

どちらも旧 PES と wrap を各 5 回、前節の引数で測った。SIGINT は待機中の読みを早く終わらせず、実質 `rw_timeout` の時間切れまで待った。しかも出力の末尾 2 フレームを失った。先行の測定（ホストの一時ファイル、4 回）では、書き手の終了直後に送った SIGINT が 60〜9,999 ms と揺れた。

時間切れは、書き手の停止が `rw_timeout` より長いと、録画の途中でも exit 0 で終わる。書き手を実時間の 14〜19 秒の間（5 秒間）止めて `rw_timeout` を 2 秒にした 2 回では、どちらも exit 0 で、出力は 249 frame（期待は 500）で終わった。終了コード 0 は録画の終了を意味しない。

したがって、時間切れを採るなら、`rw_timeout` は書き手が止まる最長の時間より長くし、録画が終わったかどうかは FFmpeg の終了の外で判定する必要がある。書き手が止まる最長の時間には、`--once` の Pod が入れ替わる 4 分 slice の境目が含まれる。下の PVC 測定では候補を試したが、有限回の観測から停止時間の上限は保証できない。

##### 実録画の母集団と時刻の基準

古い第 2 音声の時刻を持つ原本でも、映像の時刻を基準に seek を補正すると、先頭 frame の誤差は ±1 frame に収まった。

2026-10-26 に、運用中の media PVC にある `.m2ts` 原本を列挙した。対象は 51 本で、全件の `ffprobe` による開始時刻と duration の取得が成功した。MP4 は TS 原本の母集団に含めない。FFmpeg は配備イメージと同じ 7.1.5、実行 UID/GID は 65534 である。

古い音声 PES の原本が 1 本見つかった。最初の video packet の PTS は 8125285560 tick だった。第 2 音声の最初の PTS は 7639495330 tick だった。その音声の次の packet は 8125524130 tick に戻っていた。format の start_time は第 2 音声の古い値になり、映像との差は 5,397.669222 秒だった。

同じ原本で offset 10.36 秒を要求し、次の表を得た。D=2 秒、出力は 320×180 の lossless H.264、各組 420 frame、各 1 回である。入力先頭の 900 frame を同じ縮小処理で framehash にし、出力の各 segment の先頭 frame を照合した。照合対象の hash はすべて入力内で一意だった。

期待 frame は、入力の復号 frame の PTS が `V + offset + n·D` 以上になる最初のものとした。先頭 packet は復号できず、最初の復号 frame は V より 39,648 tick 後だった。packet 数や出力 PTS から frame 番号を推定せず、入力の復号 frame 列で判定した。

| n | 相対 / copyts 無し | 相対 / あり | 絶対 / 無し | 補正相対 + copyts |
|---:|---:|---:|---:|---:|
| 0 | −298 | −298 | +2 | 0 |
| 1 | −298 | −298 | +2 | 0 |
| 2 | −298 | −298 | +2 | 0 |
| 3 | −298 | −298 | +2 | 0 |
| 4 | −298 | −298 | +2 | 0 |
| 5 | −297 | −297 | +3 | +1 |
| 6 | −297 | −297 | +3 | +1 |

値は frame 単位の誤差である。絶対 seek + copyts は 60 秒で打ち切り、復号できる出力 segment は 0 本だった（timeout の exit 124）。正常終了した空出力とは区別する。補正相対は前節の式で P=2 秒とし、`select` と `setpts` を使った。この原本の 7 segment は ±1 frame に収まった。

##### 実録画の PTS wrap

実録画で時刻の折り返しが見つかった。境界後への seek は、補正した相対時刻とフレーム選別を使うと誤差が 0 frame だった。

51 本の映像の開始時刻と duration から、33 bit の境界をまたぐ候補を 4 本得た。候補全件を packet 走査したわけではない。そのうち、映像の開始 PTS が 8,580,400,148 tick の 1 本を `ffprobe -show_packets` で確認した。

`-correct_ts_overflow 0` で先頭から読むと、PTS が 8589931670 から 81 tick に折り返す隣接 packet があった。既定の補正ありでは、境界前後の区間で 449 packet を読み、8589400139〜8590745483 tick だった。以降の V と目標時刻は、FFmpeg が補正した連続時間軸で扱い、要求時刻だけを 33 bit に丸めない。

この原本の offset 108 秒（境界の後）で同じ比較をした。基準の framehash は先頭 3,900 frame、各組の出力は 420 frame、各 1 回である。入力の hash の一意性も確認した。

| n | 相対 / copyts 無し | 相対 / あり | 絶対 / 無し | 補正相対 + copyts |
|---:|---:|---:|---:|---:|
| 0 | −7 | −7 | +8 | 0 |
| 1 | −7 | −7 | +8 | 0 |
| 2 | −7 | −7 | +8 | 0 |
| 3 | −7 | −7 | +8 | 0 |
| 4 | −7 | −7 | +8 | 0 |
| 5 | −7 | −7 | +8 | 0 |
| 6 | −7 | −7 | +8 | 0 |

絶対 seek + copyts は、こちらも 60 秒の timeout（exit 124）で、有効な segment は 0 本だった。補正相対は P=2 秒で、7 segment すべて 0 frame だった。これは境界後への seek の確認で、原本全体の連続変換や音声の継ぎ目を保証しない。

##### PVC の追記の遅れ

別ノードから追記を読めるまでの遅れは、2 回の試行で中央値が約 23 秒、最大が約 55 秒だった。4 分後の書き手の交代でもファイルを引き継げた。

運用中の media PVC そのものを使い、別ノードに書き手と読み手を置いた。StorageClass は `ds420plus-kagawa`、RWX、NFS 4.1 である。両ノードは amd64 で、書き手は read-write、読み手は read-only のマウントを使った。`noac` や `actimeo=0` は指定していない。

280 秒の合成 TS を、188 byte 境界の 31,208 byte ずつ、500 ms 間隔で 560 回追記した。書き手は ingest と同じ `O_RDWR|O_APPEND` と排他 flock を使い、slice の終わりだけ Sync/Close した。実際の ingest や DB は動かさず、専用ディレクトリのファイルで再現した。

読み手は 20 ms 間隔で path の stat、開いたままの fd の fstat、同じ fd の ReadAt を記録した。遅れは各 Write の完了から、対応する累積サイズを初めて観測した時点までである。fstat の時刻は、その直後の read が返った時刻で近似した。read はサイズの観測だけでなく全バイトを hash に流し、入力と SHA-256 が一致することを確認した。

時計は測定の前後に Pod 間 HTTP を各 30 往復させて校正した。最小 RTT は 5.26 / 5.08 ms、推定時計差は +0.61 / −0.06 ms だった。表の遅れはその平均で補正した観測値で、polling と I/O の所要時間を含む。ミリ秒以下の精度や、途中に時計補正が無かったことは保証しない。

| 観測 | 回数 | 中央値 | 最大 |
|---|---:|---:|---:|
| path stat | 560 | 22.710 秒 | 54.401 秒 |
| persistent fd の fstat | 560 | 23.080 秒 | 54.700 秒 |
| persistent fd の read | 560 | 23.181 秒 | 54.700 秒 |

最初の書き手を 4 分動かし、別の準備済み Pod が同じファイルを追記した。最後の Write から次の Write までは 1.630 秒、Close からは 1.117 秒だった。次の slice の最初の追記は、stat で 10.053 秒後、read で 10.230 秒後に見えた。全体で read の増分が無い最長区間は 51.315 秒だった。

準備済み Pod だけでは起動待ちを測れないため、別ファイルでもう一度測った。今度は 4 分で終了する Job の完了後に次の Job を作り、実際の Pod 起動を含めた。560 回の stat の遅れは中央値 23.217 秒、最大 54.028 秒で、read は 23.644 秒と 54.337 秒だった。

Job 交代では、最後の Write から次の Write まで 5.957 秒、Close からは 5.438 秒だった。次の slice の最初の追記は、stat で 31.602 秒後、read で 31.910 秒後に見えた。全体で read の増分が無い最長区間は 51.385 秒で、全バイトの SHA-256 も一致した。時計校正の最小 RTT は前後で 7.69 / 5.78 ms だった。

この値は 1 つの StorageClass とノード対の観測である。media PVC が読めなかった先行の timeout 記録を、遅れの標本には含めない。NFS の属性 cache が原因という切り分けはしておらず、マウント設定の変更も試していない。

##### PVC 上での FFmpeg の追従と終了

10 秒の読み待ちでは録画途中で空の出力を正常終了した。90 秒待つ設定では、途中から末尾までの全フレームが有限ファイル版と一致した。

同じ伸長ファイルを別ノードの FFmpeg 7.1.5 で読んだ。`-follow 1 -rw_timeout 10000000` と途中からの seek を指定した 1 回では、書き手が動いている間に exit 0 になった。出力は 0 byte の TS 1 本と ENDLIST 付き playlist だった。読み手の起動から終了までは 40.736 秒だった。

次に `-rw_timeout 90000000 -skip_estimate_duration_from_pts 1` を指定した。V=129600、k=3、D=2 秒、P=2 秒で、前節の補正相対 seek と `select` を使った。起動は録画相当の開始から約 151 秒後で、既存部分を読み終わってから追記を待った。

137 segment が生成され、write 完了前にも segment が公開された。有限ファイル版の入力から framehash を取り、6 秒から末尾までの 6,841 frame が出力とすべて一致した。先頭だけでなく末尾 frame も一致し、入力バイト列の一致とは別に確認した。

n=3 / 5 / 120 / 139 の segment の最初の映像 PTS は、それぞれ 666000 / 1026000 / 21726000 / 25146000 tick だった。`n·D + 1.400 秒` と一致した。FFmpeg は書き手の Close の 98.505 秒後に exit 0 で終了し、ENDLIST が付いた。PVC で追記が見えるまでの遅れもあるため、Close からちょうど 90 秒後に終わるわけではない。

変換中には packet の duration が 0 という警告が出た。全フレームの内容一致は確認したが、音声、字幕、ブラウザ再生はこの測定に含めない。90 秒の指定と duration 推定を省く指定を別々には比較していないため、成功を片方だけの効果とはしない。

この環境で 2 秒や 10 秒を採用する根拠は無い。90 秒は完走した候補であり、普遍的な timeout の値ではない。録画完了は外部の状態で確かめ、途中終了と終端を区別する必要がある。実際の River 配送、再試行、ノード障害を含む停止時間の上限は、今回の再現では測っていない。

##### 未解決の項目

この節の方式を採るために、次が残っている。

- 未解決: 障害や配送待ちを含む停止時間に対する timeout と再開方針。有限回の PVC 測定は上限の保証にならない
- 未解決: 放送 TS 全般の最大 GOP 長に対する pre-roll の十分性。P=2 秒は比較した実録画の要求位置でのみ確認した
- 未解決: 音声と字幕。映像だけを変換して測ったので、第 2 音声の継ぎ目も測っていない
- 未解決: wrap をまたぐ原本全体の連続変換。今回は境界の packet と境界後の seek を確認した

### 録画原本のブラウザ再生

完了済みで active な原本 MPEG-2 TS を、site streamer が FFmpeg で HLS の一時セッションに変換して再生する。変換結果は派生物として保存しない。同じ録画・同じ offset の視聴者と profile 切替は 1 本のセッションを共有する。

プレイリスト起動時は、原本が無ければ 404、範囲外 offset は 416 を返す。同時セッション上限と streamer のシャットダウン中は 503、起動待ちのタイムアウトは 504、それ以外の起動失敗は 500 を返す。503 は待てば直る状態、500 は起動の失敗、504 は時間切れで、クライアントが案内を言い分けられるようにしている。シャットダウン中を 503 に残すのは、ローリング更新中の一時的な状態で、待てば直るからである。500 の詳細はログに残す。

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

セッションは録画と offset で同定し、profile は出力 playlist の選択にだけ使う。同じ録画・同じ offset の視聴者は 1 本のセッションを共有する。省略した offset と 0 は同じ先頭セッションで、ライブ・追っかけと同じ最大セッション数・idle GC・離脱ヒントに従う。最大セッション数を超えた場合は 503 になる。

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

原本 VOD の HLS scratch も `live.segment_dir` に置き、録画バッファ / archive から分離する。正常終了後も idle GC まで録画全体の変換済み segment が残る。tmpfs 容量の見積もりは [k8s-streamer.md](../../operations/k8s-streamer.md) にある。

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
