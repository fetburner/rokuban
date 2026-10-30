# 録画 HLS 配信

> [media.md](../media.md) の録画 HLS 配信。索引から辿る。

### 世代付き録画 HLS

録画中と完了後の録画を、置き場と録画と世代で決まる同じ URL から HLS 配信する。

URL は置き場名・録画 id・ingest 世代で同定する。API は `hlsPlayback` に profile ごとの master URL を返す。この URL は `openapi.yaml` に載せない。

```text
GET /api/media/stores/{store}/recordings/{id}/hls/v{gen}/{profile}/master.m3u8
GET /api/media/stores/{store}/recordings/{id}/hls/v{gen}/{profile}/{rendition}/index.m3u8
GET /api/media/stores/{store}/recordings/{id}/hls/v{gen}/{profile}/{rendition}/{n}.ts
```

`{store}` は `storage.stores` に定義した FS 置き場の名前と一致する必要がある。振り分けの鍵を site ではなく置き場にするのは、規則によって 1 つの site の原本が複数の置き場に分かれうるからである。

#### 入力と世代

録画中は ingest の temp を、commit の後は原本を、同じ URL で読む。どちらを読むかは、ファイルの有無ではなく DB の行で決める。

原本の行があれば commit 済みの原本を読む。原本の行が無く `recording_ingest_progress` の行があれば、その `temp_rel_path` の temp を読む。temp の置き場も URL の `{store}` と一致させる。

`{gen}` は、temp では `recording_ingest_progress.temp_generation`、原本では `media_assets.ingest_generation`（NULL は 0）である。commit が同じ世代を原本の行へ写すので、temp から原本へ切り替わっても URL は変わらない。キャッシュ済みのセグメントも同じバイト列から作るため、commit の後も使える。temp が作り直されて世代が変わったら、古い世代の URL は 410 にする。

#### playlist

playlist は、ffmpeg の出力を待たずにアプリが EVENT 型で組み立てる。

各セグメントには `recordings.started_at + n × D` の `#EXT-X-PROGRAM-DATE-TIME` を付ける。`D` は profile の `live.profiles[].segment_seconds` である。

映像の長さは、temp でも原本でも同じ関数で測る。定義は「末尾の映像パケットの最大の pts + duration − 原点」で、2^33 を法として取る。PTS は約 26.5 時間で巻き戻る。原点は先頭の映像パケットの PTS である。format 全体の duration は音声など最長のストリームに引かれるので使わない。ffprobe の stream duration は、FFmpeg の文書に無い推定が入るので使わない。

末尾の測り方は次のとおりである。

- ファイル末尾の数 MiB（4 MiB から始め、32 MiB、256 MiB へ広げる）を、ffprobe に標準入力で渡す。`/dev/fd/N` で fd を渡すと、Linux では通常ファイルを先頭から開き直すので、末尾だけを渡したことにならない。Go は TS を解釈しない
- 窓に映像パケットが 1 つも無ければ窓を広げる。最大の窓にも無ければ、temp は前回の値を使い、原本は失敗にする
- 窓だけを渡した ffprobe は `duration` を `N/A` で返すことがある（実 ffprobe の 64 KiB の窓で測定）。そのときは、重複を除いた pts の隣り合う差の最小の正の値（フレーム間隔）を `duration` とする
- 最大の 2 つの pts の差は使わない。B フレームがあると、I/P フレームの直後で切れた窓で 3 フレームぶんになり、長さが縮む（29.97 fps、`-bf 2`、64 KiB の窓で測定）

録画中は、観測した映像の長さに全体が収まるセグメントだけを載せる。セグメント n は `(n+1) × D ≤ 観測した長さ` のときに載り、載せる行はすべて D 秒の EXTINF である。壁時計からは決めない。壁時計で決めると、録画が終わった後に ingest が長引いたとき、原本に無いセグメントを載せ続ける。実録画では 240 行の playlist が commit の後に 182 行へ縮み、載せ済みの番号が 416 になった。EVENT は載せた行を変更・削除できない（RFC 8216 §6.2.1）。

観測は、録画・世代・入力の種類ごとに 1 本にまとめる。temp のサイズが前回と同じなら前の値を使い、メモリ上では最大値を保つ。観測が前回より小さくても、載せた行は減らない。

録画が終わって ingest が commit したら、原本の長さで最後の短いセグメントと `#EXT-X-ENDLIST` を足す。temp は追記だけで原本と同じバイト列であり、長さも同じ関数で測るので、録画中に載せた行と EXTINF は commit の後も変わらない。載せた行が commit の前後で変わらないためには、`duration` が `N/A` のときの補完も単調である必要がある。

ENDLIST を付けるのは、原本の行が DB にあるときだけである。ffmpeg の終了、入力のエラー、record の状態が未知であることは、終端の根拠にしない。

DB に問い合わせられない間も、付けた ENDLIST は外さない。そのときはキャッシュした原本の metadata を、temp の metadata より優先する。

映像 PTS が同じ世代の中で後退する録画は、保証の範囲外である。固定長の格子が PTS の単調性を前提にしており、完了した録画の配信も同じ前提に立つ。

#### 取り消し・失敗した録画

取り消し・失敗した録画は、配信を保証しない。ingest は temp と進捗行を消す（[ingest-retry.md](../../recording/ingest-retry.md)）。行が引けなくなると、playlist もセグメントも 404 になり、走っていた変換は止まる。視聴は止まり、途中までのバイトは終わった後にも見られない。

途中までを見せ続けるには、temp を残すことになる。残すと、進捗行の寿命、孤児の回収、後継の録画のバイトの混入を扱う費用がかかる。視聴中だけ録れた所まで見せる価値は、その費用に見合わない。

#### rendition とセグメント

映像と 3 本の音声を MPEG-TS の rendition にし、字幕があれば WebVTT の rendition を足す。

3 本の音声は標準・主・副である。`live.captions` が有効で原本に字幕ストリームがある場合は、字幕の rendition も master に載せる。映像・音声は `.ts`、字幕は `.vtt` のセグメントを返す。

temp を読む変換の引数と、入力の種類を変換の組に含める理由は、[入力と世代](#入力と世代)で決めてある。

長さが確定した入力（原本）では、全 rendition を映像と同じ番号で終わらせる。実録画では音声が映像より約 0.36 秒短く、FFmpeg が音声だけ 1 つ前の番号で ENDLIST を出し、終端の音声が 503 になった。そこで音声のフィルタに `apad,atrim=end=<映像の終端 − 起動位置>` を足し、PTS で切る。`apad=whole_dur` は、サンプル数で数えるので使わない。途中から起動した変換や途中で欠けた音声で、番号がずれるからである。

完成判定も、長さが確定した入力では rendition ごとの ENDLIST と最後のファイルで行う。途中で止まる副音声や字幕は、埋めても番号が揃う保証が無い。

ただし、どの rendition も、同じ変換の映像が ENDLIST を書き、映像の最後のファイルが確定した最後の番号に届いたときだけ完成とみなす。入力が途中で切れると、映像はそれより手前で終わるが、音声は apad の無音で確定した映像の終端まで進む。そのため、映像が届いていない番号の音声と字幕は公開しない。

伸びる入力では、ENDLIST を終端とみなさない（`rw_timeout` で終わることがある）。

#### 配信の可否と DB

要求ごとに DB を引き、ごみ箱・完全削除・置換でないことと、原本または進捗行のある temp があることを確かめる。配信できなければ、原因ごとに次のとおり 404 か 410 を返す。

- 行が見つからない（ごみ箱・完全削除・置換・入力なし・取り消し・失敗）、temp の置き場が違う、原本ファイルが無い、開き直した入力が別のアセットだった場合は 404 にする。その世代の変換を止め、キャッシュを捨てる
- URL の世代が今の世代と違う場合は 410 にする。URL が指す世代について、同じく変換を止めてキャッシュを捨てる
- `{store}` が違う場合は 404 にするだけで、変換にもキャッシュにも触らない

原本は、有効なコピーを持つ行があれば、`recordings.status` にかかわらず配信する。完了状態の述語を持たないのは、commit が mirakc の record の状態で決まり、status は watcher が後から書くからである。述語を持つと、commit の直後に status が追い付くまで 404 になり、変換も止まる。

新しく変換を始めるときは、原本または temp の fd を開いてから DB を引き直し、同じ種類・相対パス・世代であることを確かめる。DB に問い合わせられないときは、すでに開いている変換とキャッシュを使い続ける。行が無いと分かったときは止める。DB が使えない間に新しい変換を始める要求は 503 にする。

キャッシュにあるセグメントはそのまま返す。無いセグメントや、playlist に載せたがまだ変換できないセグメントは、`SegmentManager` が必要な範囲を変換するのを待って返す。待たずに 404 を返さないのは、RFC 8216 §6.2.1 に従い、playlist に載せたセグメントを取得できないものとして扱わないためである。前段の CDN が 404 をキャッシュすると、変換が終わった後も 404 が返り続けうる。待つ時間は `recording_hls.wait_timeout` までで、超えた要求は 503 と `Retry-After` を返す。セグメントは `private, max-age=31536000, immutable`、playlist は `no-cache` にする。世代が URL に含まれるので、セグメントは長期にキャッシュできる。

<details><summary>根拠</summary>

- 載せる行が `(n+1) × D ≤ 観測した長さ` で、全て D 秒、縮まない: `TestRecordingHLSTempPlaylistListsOnlyObservedVideo`
- 録画終了後に ingest が長引いても、行と EXTINF が commit の後も変わらない: `TestRecordingHLSCommitKeepsPublishedRowsWhenIngestOutlastsRecording`
- ENDLIST は原本からだけ付く: `TestRecordingHLSEndlistOnlyFromTheOriginal`
- DB に問い合わせられない間も ENDLIST と載せた行を保つ: `TestRecordingHLSDatabaseOutageKeepsEndlistAndPublishedRows`
- 入力エラーで ENDLIST を付けない: `TestRecordingHLSTempFollowsAndOriginalHasKnownEnd`
- 変換が終了しても再試行させ、ENDLIST を付けない: `TestRecordingHLSTempTransformErrorsAreRetryableAndDoNotEndPlaylist`
- 観測を 1 本にまとめ、サイズが同じなら測り直さない: `TestRecordingHLSTempObservationIsSharedAndSkippedForUnchangedSize`
- 窓に映像パケットが無いとき窓を広げる: `TestMeasureRecordingHLSVideoDurationWidensTheWindow`
- 広げても無ければ前回の値を使う: `TestRecordingHLSTempWithoutVideoPacketInTheWindowKeepsPreviousValue`
- 末尾の窓だけを標準入力に渡す: `TestMeasureRecordingHLSVideoDurationPipesOnlyTheTail`
- 窓だけの測定と全体の測定が一致する（実 ffprobe）: `TestMeasureRecordingHLSVideoDurationWithRealFFprobe`
- PTS の巻き戻りをまたいでも長さが負や飛び値にならない（playlist）: `TestRecordingHLSTempPlaylistAcrossPTSWrap`
- 同上（長さの計算）: `TestRecordingHLSVideoDurationFromPackets`
- 入力が途中で切れても、映像が届いていない番号の無音を公開しない: `TestSegmentTransformTruncatedInputDoesNotPublishSilentAudioWithRealFFmpeg`
- 原本の行があれば status が recording でも原本を返す: `TestRecordingHLSTargetPreservesCommittedGeneration`
- B フレームがあっても、切り出しを長くして長さが縮まない: `TestMeasureRecordingHLSVideoDurationDoesNotShrinkWithBFrames`
- 取り消し・失敗で行が消えたら 404 になり変換が止まる: `TestRecordingHLSCanceledIngestReturns404AndStopsTransform`
- 古い世代の URL は 410: `TestRecordingHLSTempGenerationChangeReturnsGone`
- 音声が映像より短くても、全音声の最後とその前の番号を取得できる（実 FFmpeg）: `TestSegmentTransformAudioRenditionsEndAtTheVideoNumberWithRealFFmpeg`
- 完成判定が rendition ごとで、伸びる入力では ENDLIST を終端にしない: `TestSegmentTransformFinalIsDecidedPerRenditionOnlyForKnownLength`
- 届く前に終わった映像は完成とみなさない: `TestSegmentTransformEndlistMakesOnlyTheRecordingsLastSegmentFinal`

</details>

#### PTS と変換キャッシュ

セグメント番号は、録画時刻の格子に固定する。

変換は、セグメント `n` の映像・音声 PTS を、原本の先頭映像 PTS を原点とする `origin + n × D` の格子に揃える。ffmpeg を別の位置から起動し直すたびに起点が変わると、同じ番号でも境界がずれる。要求が今の変換から離れると、変換は要求の位置から起動し直される。そのため入力には `-copyts -seek_timestamp 1 -noaccurate_seek` と原点からの `-ss` を使い、2 秒の pre-roll の後で trim と PTS filter を適用する。

2 秒の pre-roll は、合成 GOP での測定をもとにした既定値である。実録画の全 GOP 長での trim 精度は未検証である。

同時に動かせる変換の数は `recording_hls.max_concurrent` までである。上限に達した状態で新しい変換が要るセグメントを要求すると、その要求が空きを待つ。待機中の要求を持たない変換のうち、最後に要求された時刻が最も古いものを止め、それが終わるまで待つ。待機中の要求を持つ変換しか残らなければ、どれかが終わるまで待つ。`recording_hls.wait_timeout` を超えた要求は 503 と `Retry-After` を返す。

キャッシュの容量と tmpfs の大きさは、[k8s-streamer.md](../../operations/k8s-streamer.md) の「世代付き録画 HLS の scratch」で決める。変換とキャッシュは専用 Pod の tmpfs に置く。原本と temp は読み取り専用で開く。

キャッシュの使用バイト数、ヒットとミス、動いている変換の数、503 の件数は次のメトリクスで見る。

- `rokuban_recording_hls_cache_bytes`
- `rokuban_recording_hls_cache_requests_total`
- `rokuban_recording_hls_active_transforms`
- `rokuban_recording_hls_unavailable_total`

世代付き録画 HLS は `-copyts` を使い、録画全体で共通の PTS 格子にセグメントを置く。

<details><summary>根拠</summary>

- PTS 格子と pre-roll: `TestSegmentTransformRealFFmpegKeepsRequestedPTSGrid`

</details>
