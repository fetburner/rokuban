# 録画 temp と seek の測定

測定結果の解釈は [再生設計](../../docs/api/media/playback.md) の「録画中の temp を読む方式の測定」にある。アプリ、mirakc、DB は起動しない。共有 PVC の専用ディレクトリで合成 TS を再生し、原本は読み取り専用で解析する。

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

書き手は 31,208 byte を 500 ms ごとに追記する。実際の chunk サイズは合成入力のファイル長から決まり、open のログに残る。1 本目の Job が 240 秒で終わった後に、次の Job を作る。次の Job は同じ inode を O_APPEND で開き、残り 40 秒を書いて終わる。読者は開いた fd を保持し、path stat、fstat、ReadAt を 20 ms ごとに記録する。

両ノードのアーキテクチャに合う probe を使う。記録した追測は amd64 同士であり、arm64 や別 StorageClass の測定値にはしない。runner は末尾を TS packet 境界で切るため、入力全体ではなく実際に書いた prefix の SHA-256 を照合する。有限ファイルとの映像照合にも同じ prefix を使う。

出力には各 Write の前後時刻、初めて長さと内容を観測した時刻、時計校正、Job の manifest が残る。`analyze.py` は未観測の追記と不一致の SHA-256 をエラーにする。時計差の推定は最小 RTT の往復から取り、前後の平均で補正する。polling と I/O の時間も測定値に含む。時計が途中で step しなかったことは保証しない。

終了時には自身で作った Job、Pod、NetworkPolicy、専用ディレクトリだけを削除する。接続断や強制終了の後は、出力の `resources.json` と `job-*.json` で対象を確認して回収する。測定用ディレクトリは `.rp3-measure-` で始まる一意の名前で、既存録画に追記しない。

## 伸長ファイルの FFmpeg

I/O runner とは別に、同じ PVC を read-only でマウントする Pod で次を実行する。`INPUT` は専用ディレクトリの growing.ts、`OUT` は Pod の emptyDir 上の新規ディレクトリとする。run の resources.json で Pod 名と mount を確認できる。I/O runner は終了時に Pod を削除するため、FFmpeg の終了とログ回収まで待つ測定用 Pod を別に用意する。

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

各 segment の mtime、playlist の ENDLIST、終了時刻・終了コードを保存する。入力 prefix と出力 playlist を `-f framehash -hash md5` に通し、入力の frame 150 から末尾までを全件比較する。出力 0 byte の TS と exit 0 を成功扱いしない。追測では 10 秒で途中終了し、90 秒では 6,841 frame が一致した。duration=0 の警告も記録している。

[FFmpeg の file protocol 契約](https://ffmpeg.org/ffmpeg-protocols.html#file)は follow の終了に rw_timeout または割り込み callback を要求する。書き手の終了時刻と reader の timeout の開始時刻は同じではない。有限回の測定から停止時間の上限を決めない。

## 原本の古い PES と PTS wrap

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

## 測定コードの検査

```sh
python3 -m unittest discover -s scripts/playback-measure -p 'test_*.py'
```

2026-10-26 の追測では、累積サイズの判定を壊す変異と、入力格子の target を 0 にする変異で、それぞれ assertion が失敗することを確認した。古い PES と wrap の実録画では、未補正の引数が ±1 frame を超え、補正引数だけが範囲内に収まった。
