# 録画 temp と seek の測定

測定結果の解釈は [temp を読む方式の測定](../../docs/api/media/playback-temp-measurement.md)にある。アプリ、mirakc、DB は起動しない。共有 PVC の専用ディレクトリで合成 TS を再生し、原本は読み取り専用で解析する。

集計値と追記ごとの観測は `results/` に置く。原本のタイトル、映像、内部 IP は含めない。

必要なものは kubectl、Go、Python 3、FFmpeg/ffprobe、既存 media PVC を使う Deployment、異なる 2 ノード。Pod 内のイメージには sh、tar、timeout、FFmpeg/libx264 が必要。追加の Python パッケージは使わない。

## 別ノードの stat / read と 4 分 slice

まず 280 秒、25 fps、500 kbit/s の合成入力を作る。ファイル名は例である。runner は指定した出力ディレクトリを新規作成する。

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/rp3-probe scripts/playback-measure/probe.go
ffmpeg -hide_banner -loglevel error \
  -f lavfi -i 'testsrc2=size=160x90:rate=25:duration=280' \
  -c:v mpeg2video -g 50 -bf 0 -b:v 250k -muxrate 500k \
  -f mpegts /tmp/rp3-source.ts
python3 scripts/playback-measure/k8s.py \
  --writer-node "$WRITER_NODE" --reader-node "$READER_NODE" \
  --probe /tmp/rp3-probe --source /tmp/rp3-source.ts --output /tmp/rp3-run
python3 scripts/playback-measure/analyze.py /tmp/rp3-run \
  --source /tmp/rp3-source.ts
```

`k8s.py` は既存 Deployment の media PVC、securityContext、イメージ、imagePullSecrets の参照を使う。設定、DB の Secret、デバイスはマウントしない。測定 Pod 間の時計校正用 TCP 18080 だけを許可する NetworkPolicy を作る。API 接続とイメージ取得の権限は実行者が持っている必要がある。

書き手は 31,208 byte を 500 ms ごとに追記する。実際の chunk サイズは合成入力のファイル長から決まり、open のログに残る。1 本目の Job が 240 秒で終わった後に、次の Job を作る。次の Job は同じ inode を O_APPEND で開き、残り 40 秒を書いて終わる。読み手は開いた fd を保持し、path stat、fstat、ReadAt を 20 ms ごとに記録する。

両ノードのアーキテクチャに合う probe を使う。記録した実クラスタの測定は amd64 同士であり、arm64 や別 StorageClass の測定値にはしない。runner は末尾を TS packet 境界で切るため、入力全体ではなく実際に書いた prefix の SHA-256 を照合する。有限ファイルとの映像照合にも同じ prefix を使う。

出力には各 Write の前後時刻、初めて長さと内容を観測した時刻、時計校正、Job の manifest が残る。`analyze.py` は未観測の追記と不一致の SHA-256 をエラーにする。時計差の推定は最小 RTT の往復から取り、前後の平均で補正する。polling と I/O の時間も測定値に含む。時計が途中で step しなかったことは保証しない。

終了時には自身で作った Job、Pod、NetworkPolicy、専用ディレクトリだけを削除する。接続断や強制終了の後は、出力の `resources.json` と `job-*.json` で対象を確認して回収する。測定用ディレクトリは `.rp3-measure-` で始まる一意の名前で、既存録画に追記しない。

## 伸長ファイルの FFmpeg

追記の測定用 runner とは別に、同じ PVC を read-only でマウントする Pod で次を実行する。`INPUT` は専用ディレクトリの growing.ts、`OUT` は Pod の emptyDir 上の新規ディレクトリとする。run の resources.json で Pod 名と mount を確認できる。追記の測定用 runner は終了時に Pod を削除するため、FFmpeg の終了とログ回収まで待つ測定用 Pod を別に用意する。

```sh
ffmpeg -hide_banner -loglevel warning -threads 1 -copyts \
  -follow 1 -rw_timeout 90000000 -skip_estimate_duration_from_pts 1 \
  -ss 4 -i "$INPUT" -map 0:v:0 -an -fps_mode passthrough \
  -vf 'select=gte(pts\,669600),setpts=PTS-129600' \
  -c:v libx264 -threads 1 -preset ultrafast -qp 0 \
  -g 50 -keyint_min 50 -sc_threshold 0 -bf 0 \
  -force_key_frames 'expr:gte(t,6+n_forced*2)' \
  -f hls -hls_time 2 -hls_list_size 0 -start_number 3 -hls_flags temp_file \
  -hls_segment_filename "$OUT/seg%03d.ts" "$OUT/index.m3u8"
```

合成入力の最初の映像 PTS は 129600 tick、目標はその 6 秒後の 669600 tick。実測前に ffprobe で確認する。10 秒の timeout の試行は、`-skip_estimate_duration_from_pts` を付けず、先頭から約 8 秒後に起動した。90 秒の試行は約 151 秒後に起動した。この 2 条件は単一変数の比較ではない。

各 segment の mtime、playlist の ENDLIST、終了時刻・終了コードを保存する。入力 prefix と出力 playlist を `-f framehash -hash md5` に通し、入力の frame 150 から末尾までを全件比較する。出力 0 byte の TS と exit 0 を成功扱いしない。実クラスタの測定では 10 秒で途中終了し、90 秒では 6,841 frame が一致した。duration=0 の警告も記録している。

[FFmpeg の file protocol 契約](https://ffmpeg.org/ffmpeg-protocols.html#file)は follow の終了に rw_timeout または割り込み callback を要求する。書き手の終了時刻と reader の timeout の開始時刻は同じではない。有限回の測定から停止時間の上限を決めない。

## 原本の古い音声 PES と PTS wrap

原本の内容をローカルへ転送せず、Pod 内で縮小・lossless 変換と hash の照合を行う。`--input` は Pod 内の読み取り専用パスである。原本ごとの V と S を ffprobe で取り、90 kHz の整数を渡す。V は最初の video packet の PTS、S は format の start_time。復号できない先頭 packet は基準 framehash に現れないため、期待 frame は復号できた入力 frame の PTS から選ぶ。

```sh
python3 scripts/playback-measure/seek.py \
  --pod "$POD" --input "$ORIGINAL_PATH" \
  --video-origin-90k 8125285560 --format-start-90k 7639495330 \
  --offset 10.36 --reference-frames 900 --output /tmp/rp3-old-pes
python3 scripts/playback-measure/seek.py \
  --pod "$POD" --input "$WRAP_ORIGINAL_PATH" \
  --video-origin-90k 8580400148 --format-start-90k 8580367237 \
  --offset 108 --reference-frames 3900 --output /tmp/rp3-wrap
```

相対 / 絶対 seek × copyts 無し / ありと、pre-roll 2 秒の補正相対 seek を比較する。D=2 秒、各 420 frame、H.264 lossless、320×180、B-frame 無し。出力 segment の先頭 frame の hash を入力に照合し、入力で hash が一意でなければエラーにする。出力 PTS の原点合わせで誤差を相殺しない。期待値は V + offset + n×2 秒以上の最初の復号 frame である。

原本の列挙と metadata の全件取得から wrap 候補を絞り、次のように数値だけを観測する。境界確認後の seek 比較は FFmpeg 既定の overflow 補正を有効にする。原本全体を走査していないときは、その範囲を明記する。

```sh
ffprobe -v quiet -correct_ts_overflow 0 -select_streams v:0 \
  -show_packets -show_entries packet=pts -of csv=p=0 "$WRAP_ORIGINAL_PATH" |
  awk -F, '$1 ~ /^[0-9]+$/ { if (prev > 8000000000 && $1 < 1000000000) { print prev; print $1; exit } prev=$1 }'
```

seek.py がローカルへ保存するのは数値、hash、標準エラー、集計だけである。原本タイトルや内部 IP を公開する成果物に入れない。Pod の `/tmp/rp3-seek-*` は emptyDir の破棄時に消える。

## 製品の seek 引数

`seek.py` の「補正相対 seek」は、ドキュメントの式であって製品の引数ではない。製品の `SegmentManager`（`buildSegmentTransformArgs`）の実引数は、`internal/streamer` の opt-in テスト `TestSegmentTransformSeekMeasure` で測る。引数を写さず製品のビルダーを呼ぶ。環境変数が無ければ Skip する。読み取り専用で、worker・DB・reconciler は起動しない。

| 環境変数 | 意味 | 既定 |
|---|---|---|
| `ROKUBAN_SEEK_INPUT` | 入力の TS のパス | なし |
| `ROKUBAN_SEEK_SYNTH` | `old-pes` か `wrap` で合成入力を作る（INPUT の代わり） | なし |
| `ROKUBAN_SEEK_STARTS` | 起動するセグメント番号 k のカンマ区切り | `3,5` |
| `ROKUBAN_SEEK_COUNT` | k から数えて取るセグメント数 | `6` |
| `ROKUBAN_SEEK_FRAMES` | 基準にする入力の復号 frame 数。k+COUNT の位置まで届く値にする | `900` |
| `ROKUBAN_SEEK_AUDIO` | 入力の音声ストリーム数 | `2` |
| `ROKUBAN_SEEK_ORIGIN_SHIFT` | OriginPTS と期待 frame の V から引く tick 数。V の frame 格子に対する最初の復号 frame の位相 φ をずらす | `0` |
| `ROKUBAN_SEEK_TOLERANCE` | 許す誤差の絶対値 frame | `1` |

出力は QP=0 の無損失で縮小しないので、入力が 1080 の場合はセグメントごとに数十 MB になる。各セグメントの先頭フレームの hash を、入力の復号 frame 列に照合する。期待 frame は `seek.py` と同じで、入力の復号 frame の PTS が V + n·D 以上になる最初のものである。hash が入力内で一意でなければ失敗にする。

```sh
ROKUBAN_SEEK_SYNTH=old-pes ROKUBAN_SEEK_FRAMES=1000 \
  go test ./internal/streamer/ -run TestSegmentTransformSeekMeasure -v -count=1
ROKUBAN_SEEK_INPUT=/path/to/input.ts ROKUBAN_SEEK_AUDIO=1 \
  go test ./internal/streamer/ -run TestSegmentTransformSeekMeasure -v -count=1
```

Pod の中で、原本を読み取り専用のまま測るときは、テストバイナリを linux/amd64 でビルドして Pod に置く。Pod には ffmpeg / ffprobe（libx264 付き）が必要で、書き込める場所（emptyDir の `/tmp` など）を `TMPDIR` にする。1080 の無損失出力はセグメントごとに数十 MB になり、起動位置 k ごとに 6 本ずつ scratch に残る。`TMPDIR` の空きを数 GB 確認してから回す。

```sh
GOOS=linux GOARCH=amd64 go test -c -o /tmp/rp6seek.test ./internal/streamer/
kubectl -n "$NAMESPACE" cp /tmp/rp6seek.test "$POD":/tmp/rp6seek.test
```

古い音声 PES の原本は、k=3 と 5 で測る。`ROKUBAN_SEEK_AUDIO` は原本の音声ストリーム数に合わせる。

```sh
kubectl -n "$NAMESPACE" exec "$POD" -- env TMPDIR=/tmp \
  ROKUBAN_SEEK_INPUT="$OLD_PES_ORIGINAL_PATH" ROKUBAN_SEEK_AUDIO=2 \
  ROKUBAN_SEEK_STARTS=3,5 ROKUBAN_SEEK_FRAMES=900 \
  /tmp/rp6seek.test -test.run TestSegmentTransformSeekMeasure -test.v
```

PTS wrap の原本は、境界をまたぐ変換を含めるため、k を境界の手前に置く。V から 2^33 tick までは約 105.94 秒なので、D=2 秒なら k=51〜53 になる。基準 frame はその先（k+6 本の位置、約 118 秒）まで届く値にする。

```sh
kubectl -n "$NAMESPACE" exec "$POD" -- env TMPDIR=/tmp \
  ROKUBAN_SEEK_INPUT="$WRAP_ORIGINAL_PATH" ROKUBAN_SEEK_AUDIO=2 \
  ROKUBAN_SEEK_STARTS=51,52,53 ROKUBAN_SEEK_FRAMES=3900 \
  /tmp/rp6seek.test -test.run TestSegmentTransformSeekMeasure -test.v
```

`ROKUBAN_SEEK_FRAMES` は入力の frame 数を超えない値にする。結果は標準出力の `k=… n=… error_frames=…` の行である。この手順の Pod 上での実行は未実施で、linux/amd64 のクロスビルドが通ることだけを確認した。

## Pod を使わない seek.py

`--pod` を付けなければ、手元の ffmpeg / ffprobe で同じ比較を走らせる（GNU `timeout` が無い macOS でも動く）。`--scratch-root` は作業ディレクトリの置き場、`--video-origin-90k` に V から φ 分を引いた値を渡すと位相をずらせる。`corrected-trim`（`select` の代わりに `trim=start=T`）と `absolute-noaccurate`（`-noaccurate_seek` 付きの絶対 seek）の比較も出る。測定用の出力の `--gop`、`--force-key-frames`、`--hls-time`、`--enc-time-base` を変えると、誤差が出力側の keyframe の置き方で動くかを確かめられる。

```sh
python3 scripts/playback-measure/seek.py --input /path/to/input.ts \
  --video-origin-90k "$((V - 609))" --format-start-90k "$S" \
  --offset 10.36 --reference-frames 900 --scratch-root /tmp --output /tmp/rp3-local
```

## 測定コードの検査

Go 側の期待 frame の選択と hash の一意性は `go test ./internal/streamer/ -run TestSeekFrameError` で検査する。Python 側は次のとおり。

```sh
python3 -m unittest discover -s scripts/playback-measure -p 'test_*.py'
```

実クラスタの測定では、累積サイズの判定を壊す変異と、入力格子の target を 0 にする変異で、それぞれ assertion が失敗することを確認した。古い音声 PES と PTS wrap の原本では、未補正の引数が ±1 frame を超え、補正相対 seek だけが範囲内に収まった。ただしこの測定の基準 PTS は frame rate の time base に丸められていた。`seek.py` は `-enc_time_base:v 1/90000` を付けるよう直したが、クラスタでの再測定は未実施である。古い音声 PES の原本の n=5,6 の +1 は、この丸めではなく測定用の出力側の keyframe の置き方によるものだった（位相 609 tick の放送 TS で再現。結果は temp 方式の測定の docs にある）。
