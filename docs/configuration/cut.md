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

</details>

#### 同じ名前で意味を変えない

**同じ名前のまま、カット版かどうか、または出力の形式を変える設定は、api と worker が起動時に拒む。** encode は、録画に凍結したプロファイル名から、実行時に今の定義を引く。名前を変えずに定義の中身を変えると、その名前で作った既存の成果物と食い違う。

たとえば `cut` を外しても、その名前の既存のカット版は作り直されずに残る。active な encoded があれば、その名前は揃っているとみなすからである。設定の上ではカット版でないのに、ファイルはカット版のままになる。

変えてはならない属性は、次の 2 つを両方満たすものである。

1. 成果物の意味に効く。`cut` はカット版かどうかを、`container` はファイルの形式を決める。
2. 既存の資産か作り直し要求の行から、過去の値を読める。active な encoded の相対パスから、`cut` は世代番号 `_{profile}.g{n}` の有無を、`container` は拡張子を読む。作り直し要求の行は、押した時点でカット版だった証拠になる。この行からは `container` を読めないので比べない。

`media_asset_cuts` の行は証拠にしない。catalog の rescue は encoded を戻すが、この行は戻さない。

次の属性は 2 を満たさないので含めない。

- `video_codec`・`height`・`crf`・`hwaccel` は、相対パスにも DB にも過去の値が残らない。読むにはファイルを ffprobe するしかなく、api はファイルを読めない。値の文字列は、同じ形式のまま機材に合わせて変わることもある（`libx264` から `h264_vaapi` へ、など）。
- `subtitles` も過去の値が残らない。サイドカーが無いことは字幕の無い録画でも起きるので、ファイルからも決まらない。

<details><summary>根拠</summary>

- 意味の検査: `worker.CheckEncodeProfileMeaning`（呼び出しは `runServer`）
- 過去の意味を引くクエリ: `ListEncodedMeaningEvidence`
- 世代番号の読み手: `worker.cutGeneration`（書き手 `EncodedRelPath` の逆）
- 名前の検査: `config.ValidateEncodeProfileName`（書き手 `EncodedRelPath` も同じ関数で検査する）

</details>

##### 相対パスから読める名前

相対パスから読むには、名前が相対パスにそのまま入らなければならない。設定は次の名前を拒み、encode の書き手も同じ規則でその名前のパスを作らない。

- サニタイズで変わる名前（`/` や `:` を含む、前後に空白がある、`..` を含む）。別の名前と同じ相対パスになる。
- `.` で終わる名前。続く `.` と合わさった `..` がサニタイズで潰され、カット版と読めなくなる。
- `.g` と、先頭が 0 でない数字で終わる名前（`x.g1`、`foo.g12`）。カット版でない `x.g1` は、`x` のカット版の世代 1 と同じ相対パスになる。`x.g0` や `x.g01` は書き手が書かない世代なので、拒まない。
- `rescue-` で始まる名前。catalog を使わない rescue が、拾った encoded に付ける名前である。そのパスは世代番号の形を持たない。

これらを拒むと、同じ原本から作る相対パスは、名前と世代ごとに異なる。別の原本どうしでは、同じになることがある（原本 `r_a` の `b` と、原本 `r` の `a_b`）。そのパスを削除済みでない別の行が持っているとき、後から公開する側のエンコード試行は失敗し、既存のファイルは変わらない（[storage/contract.md](../storage/contract.md) §派生物の公開（encode））。

この規則は設定の読み込みで検査するので、全ロールに効く。規則に当たる名前が設定にあると、更新しただけで全ロールが起動を拒む。意味の検査の対象でない watcher・streamer・notifier も止まる。

規則に当たる名前は改名する。改名すると、旧名で凍結した録画のうち旧名の encoded がまだ無いものは、満たせなくなる（`rokuban_encode_reconcile_unsatisfiable` に出る）。その `until_encoded` の原本は消えずに残る。旧名は設定に戻せないので、対処はその録画のエンコード方針から旧名を外すことである（[runbook/troubleshooting.md](../runbook/troubleshooting.md) §エンコードが失敗している（理由を知りたい））。

新名は、旧名をサニタイズした名前（`a:b` なら `a_b`）と同じにしない。旧名の encoded が削除済みでない録画では、新名のエンコードが同じ相対パスを予約できず、毎回失敗して試行の上限で止まるからである。止まった後の扱いは [runbook/troubleshooting.md](../runbook/troubleshooting.md) §encode が上限に達して止まった にある。

<details><summary>根拠</summary>

- 公開が `rel_path` の予約を rename より先に行うこと: `publishEncoded`
- 削除済みでない行がパスを予約し続けること: 一意索引 `media_assets_rel_path_idx`（`WHERE state <> 'deleted'`）
- active の行とパスが衝突したとき、ファイルを保護して試行を失敗として記録すること: `TestEncodeWorker_ConflictingActiveRelPathDoesNotOverwrite`

</details>

##### 検査の置き場と拒まないもの

検査は、api と worker が DB に繋いだ直後に行う。HTTP の待ち受けとジョブの購読より前である。設定を読み込む時点では DB を読めないので、そこには置けない。worker には、ingest だけを購読する site の worker も含む。

api は自分の設定でカット版かを判定し、作り直し要求を書く。worker は凍結と encode でカット版かを読む。分割構成では、api と worker が同じ設定を別の Pod で読む。片方だけが拒むと、起動したもう片方が食い違った設定のまま書く。api なら作り直し要求の行を、worker なら食い違った意味の encoded を書く。

要求行はカット版の証拠になるので、設定を戻しても名前の証拠が混ざり、worker は起動できない。改名しても行は残り、未充足の要求がある間は `until_encoded` の原本を消さない。

次は拒まない。

- プロファイルを設定から消す。満たせない desired は `rokuban_encode_reconcile_unsatisfiable` に見え、原本は守られる。
- active な encoded も作り直し要求の行も無い名前。過去の意味が存在しない。

##### 混ざった名前の出口

検査は起動時にしか走らない。ロールアウトの間は、旧設定のプロセスと新設定のプロセスが同時に動く。その名前にまだ証拠が無いと、新設定のプロセスも起動できる。両方が同じ名前で書くと、カット版とそうでないものが混ざる。混ざった名前は、どちらの設定でも起動できない。

要求行を書くとき、api は自分の設定でカット版かを確かめるだけで、DB にある過去の証拠とは照合しない。凍結済みの desired は名前しか持たず、証拠の無い名前では照合先が無いためである。残った要求行が起動時の検査でカット版の証拠になり、`cut: false` のまま次に起動するプロセスは拒まれる。

出口は、名前を改名するか、設定から消すかである。どちらでも、旧名で凍結した録画のうち旧名の encoded がまだ無いものは、満たせなくなる（`rokuban_encode_reconcile_unsatisfiable` に出る）。その `until_encoded` の原本は消えずに残る。

<details><summary>根拠</summary>

- 要求行だけが残る名前を `cut: false` で拒むこと: `TestCheckEncodeProfileMeaning/request_only,_cut_flipped`

</details>

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

