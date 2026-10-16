> [configuration.md](../configuration.md) §スキーマ構造のうち、カット版の節。索引から辿る。

### カット版（`encode.profiles[].cut`）

カット版を選ぶなら、カットしないプロファイルを 1 つ以上含める。確認に再生が要り、原本はブラウザで再生できないので再生にエンコードが要り、エンコードに確認が要る循環になるためである。自動検出のままではカット版を作らず、ユーザーが確認したチャプターだけが対象である。

**切る対象は録画側の事実としてチャプターが持ち、その出力に切り取りを適用するかは出力の性質としてプロファイルが持つ。** チャプターは自動検出（`cm_detect`）の結果をユーザーが確認したもので、その `cut=true` 区間が落ちる。自動検出のままでは作らない。誤検出のまま本編が削られ、原本がごみ箱を経由せずに消えると取り返せないためである。

カット版の相対パスには世代番号が入る（1 世代目から付ける）。チャプターを直しても自動では作り直さず、UI の「編集前の内容です」から `POST /api/recordings/{id}/encoded/{profile}/reencode` で作り直す（[storage/contract.md](../storage/contract.md) §3）。

ルール保存・予約 overrides・事後追加 API・ingest の凍結の 4 経路が同じ判定を共有し、マージ結果が `cut: true` だけになった場合は凍結時に `cut` を落とす。意図は overrides に残るので、`cut` でないプロファイルを足せば戻る。

<details><summary>根拠</summary>

- 確認済みを示す行: `recording_chapter_ownership`
- 相対パスの列: `rel_path`
- 世代番号を含むファイル名の形: `…_{profile}.g{n}.{container}`
- 検査: `worker.CheckEncodeProfileMeaning`（呼び出しは `buildWorkerAdapter`）
- 過去の意味を引くクエリ: `ListEncodedMeaningEvidence`

</details>

#### 同じ名前で意味を変えない

**同じ名前のまま、カット版かどうか、または出力コンテナを変える設定は、worker の起動時に拒む。** 名前が成果物の意味の契約だからである。

録画の desired はプロファイル名の集合として凍結され、encode は実行時に名前から今の定義を引く。意味を変えると、既存の encoded と設定が食い違う。作り直し要求を満たされるまで残す形にすると、その要求は永久に満たされず、`until_encoded` の原本を握り続ける。

変えてはならない属性は `cut` と `container` である。判定基準は、既存の証拠から過去の値を読めるかどうかである。active な encoded の相対パスから、`cut` は世代番号 `_{profile}.g{n}` の有無、`container` は拡張子を読む。`media_asset_cuts` の行は、catalog の rescue が戻さないので証拠にしない。作り直し要求の行は、カット版でしか作れないので `cut` の証拠になる（`container` は分からないので比べない）。`crf` や `hwaccel` など品質と機材の設定は、ファイルから読めず、機材の交換で変わるのが普通なので含めない。

検査は encode を実行する worker が起動時に行い、食い違えば名前を変えるよう促すエラーで起動を拒む。`config.Load` は DB を読めず、api は encode を実行しないので、実行する側に置く。次は拒まない。

- プロファイルを設定から消す。満たせない desired は `rokuban_encode_reconcile_unsatisfiable` に見え、原本は守られる。
- active な encoded も作り直し要求の行も無い名前。過去の意味が存在しない。

以前に名前を変えずに `cut` を反転させた環境では、同じ名前の active な encoded に、カット版とそうでないものが混ざる。どちらの設定でも起動できず、出口はその名前を改名することだけである。

#### カット版の argv と切る時刻

カット版は filtergraph で組み、映像も音声も時刻で切る。原点は入力の最早の開始時刻で、チャプターの原点と同じである。

```
-hide_banner -nostats -y
[-hwaccel K [-hwaccel_device D] [-hwaccel_output_format F] | -vaapi_device D] [input_extra_args…]
-i INPUT
-filter_complex <trim/atrim → concat → deinterlace → scale>    # アプリが組む
-map [vout] -map [aout]
-c:v VC -c:a AC …
```

`hwaccel.output_format` があれば、通常の encode と同じ hwaccel 引数を `-i` の前に出す。省略時は従来の救済経路を使う。`hwaccel.kind: vaapi` なら `-vaapi_device D` を出す。各区間を映像は `trim=start=…:end=…`、音声は `atrim=start=…:end=…` で切り、`concat` したあとに、`scaler` から導出した deinterlace / scale を連結する。VAAPI の GPU 経路は `deinterlace_vaapi` → `scale_vaapi` を使い、`format=nv12,hwupload` は付けない。救済経路では CPU filter の後ろに `format=nv12,hwupload` を付ける。

映像を「最初の映像フレームから数えたフレーム番号」で切ると、放送 TS では映像だけずれる。境界は同じフレーム番号から秒へ換算する（`frame / fps`）ので、区間ごとに A/V のずれが蓄積しない。trim の後に PTS から区間の開始時刻を引くのは、`concat` が各区間の先頭を 0 とみなすためである。`PTS-STARTPTS` は使わず開始時刻を引くことで、映像の先頭が区間の頭より遅れる場合も遅れを残す。

**出力側に `-map` を書けないので、アプリがストリームを選ぶ。** ffmpeg の既定の選択（映像は画面サイズが最大のもの、音声はチャンネル数が最大のもの、同点は若い番号）を ffprobe で再現する。再現しないとカット版だけ別のストリームが選ばれ、カットしない版と音声が食い違う。

字幕サイドカーは同じ ffmpeg 起動の別出力なので `trim` が掛からない。書き出した後に同じ keep 区間の写像で時刻を付け替える。CM 区間の中に収まるキューは捨て、境界をまたぐキューは keep 側でクリップする。

カット版にだけ掛かる起動エラーは次の通り。HW フレームに CPU filter を掛ける組み合わせと、VAAPI の前提が揃わない組み合わせである。

- `hwaccel.output_format` があり、`height > 0` または `deinterlace: true` なのに `scaler: vaapi` でない。HW フレームの後ろに CPU filter は置けないため
- `hwaccel.output_format` を指定する場合は `vaapi` のみ。指定すると hwupload を付けないので、ソフトウェア形式を HW へ上げる手段が無いため
- `scaler: vaapi` で filter があるのに `hwaccel.output_format: vaapi` が無い。upload は filter 後なので VAAPI filter に CPU フレームを渡せないため
- `hwaccel.kind` が `vaapi` 以外。VAAPI scaler と救済経路を使えないため
- `hwaccel.kind: vaapi` なのに `device` が無い。`-hwaccel_device` または `-vaapi_device` に渡すものが無いため
- `extra_args` の `-map`。ストリームの並びはアプリが握るため（live と同じ理由）

HW フレームは trim と concat を通せる。VAAPI の GPU 経路は FFmpeg 7.1 以降を推奨し、5.1〜7.0 では救済経路を使う。

5.1〜7.0 は MPEG-2 のデコード surface が固定 6 枚で、カット版では枯れて `get_buffer() failed` からつなぎ目にブロックノイズが出る。ジョブは成功扱いで終わるので、壊れたカット版がそのまま公開される。7.1 以降は動的プールになり、7.1.5-0+deb13u1 / libva 2.22.0-3 / iHD 25.2.3 でつなぎ目 3 の 30 分原本全体を処理しても `get_buffer() failed` は出なかった。未検証: 7.1 以降でつなぎ目の多い録画。

5.1〜7.0 は `-extra_hw_frames` で直さず、救済経路を使う。足りない枚数はつなぎ目の数で増えるので、固定値では足りない。5.1.9-0+deb12u1 / libva 2.17.0 / iHD 23.1.1 では、つなぎ目 1 は 1 枚、つなぎ目 3 は 4 枚の追加で解消した。つなぎ目 15 は 16 枚の追加でも `Cannot allocate memory` で止まった。救済経路は `-hwaccel` なしの CPU デコードなので、このプールを使わない。

<details><summary>根拠</summary>

- HW フレームが trim / concat を通れるソース上の根拠（n5.1.6 / n9.0）: trim.c・avf_concat.c がフレーム内容と HW pixel format を見ない
- これらの filter の出力が入力の HW フレームの文脈を引き継ぐ: avfilter.c の `hw_frames_ctx`
- 同じことの実測（ffmpeg 9.0.2、VideoToolbox）: trim → setpts → concat → `scale_vt` → `h264_videotoolbox`

</details>

#### 追加引数の許可リスト

値の個数まで既知の許可リストにあるものだけを受け付ける。

`extra_args` / `input_extra_args` で値を取らないのは `-an` `-vn` `-sn` `-dn` `-shortest` `-nostdin` `-re` である。直後の 1 トークンを値として取るのは `-movflags` `-map` `-global_quality` `-cq` `-q:v` `-b:v` `-b:a` である。`-probesize` `-analyzeduration` `-extra_hw_frames` も 1 トークンを取る。それ以外と裸の位置引数は起動エラーになる。

値を取らないフラグも明示しているため、`["-an", "/tmp/evil.mp4"]` のように 2 本目の出力パスをフラグの値に見せかけることはできない。`-filter:v:0` / `-lavfi` のような filtergraph の別名・ストリーム指定子付き表記も許可リストの外であり、完全一致の denylist が別名を取りこぼす形は採らない。

**ライブではさらに `-an` `-vn` `-sn` `-map` を拒否する。** ライブはストリームの並び（映像・音声 rendition 3 本・字幕）を `-var_stream_map` で持つ。並びを変えると ffmpeg が起動時に落ち、利用者には 504 しか見えない（[api/media.md](../api/media.md) §音声）。

