> [live.md](../live.md) の一部。索引から辿る

# ライブ視聴 --- EPGStation 水準のシンプルな UI

リッチな視聴体験（ハードウェアトランスコード、低遅延、コメント連携）は KonomiTV が既に高水準で解決しており、そこに張り合わない。同じ mirakc を共有すれば KonomiTV との共存も可能（チューナーは優先度調停で録画が勝つ）。

Rokuban 自体のライブ視聴は「チャンネル一覧から選んでブラウザ再生、画質切り替え程度」で良い。

## 実装方式

- **mirakc `/api/services/{id}/stream` → ffmpeg → HLS** の薄いパイプ。セグメントはローカルスクラッチに書き、**streamer が配信する**（配信経路の詳細は [api.md](../../api.md) のライブ HLS 配信）
- `streamer` ロールが担当。リアルタイムに mirakc からの帯域を張り続けるため、分散モードではエッジ寄りに配置する（エンコードジョブと違いレイテンシ・帯域制約がある）

## セッション管理

- **ライブ視聴セッションは意図的に in-memory**。落ちたらクライアント再接続で済む使い捨て状態であり、「すべての状態を Postgres に」の原則の明示的な例外（参照: [overview.md](../../overview.md) の crash-only 設計原則）
- 「クライアントがいなくなったら ffmpeg を止める」idle GC が必要。セグメント要求がアプリを通ることで last-access の更新がタダで手に入る（参照: [api.md](../../api.md) のライブ HLS 配信）

## 録画中の追っかけ再生

録画詳細の再生枠は 1 つで、録画中かつライブ能力が有効なら追っかけを再生元に選ぶ。
通常の詳細表示ではポスターと再生ボタンを出し、押すまでは `/chase/playlist.m3u8` を
要求しない。ホームの「録画中」帯、番組リスト、番組表グリッドのダイアログからは
`/recordings/{recordings.id}#chase` へリンクし、詳細を開くと再生を始める。
明示的な再生操作の後にだけ変換セッションを作ることで、
表示するだけの閲覧で streamer の資源を保持しない。

放送が終わって `recordings.status` が `finished` になっても、`ingest.state` が
`pending` / `transferring` なら詳細から新しい追っかけを始められる。commit 前の追っかけは
状態更新だけでは再生元を替えず、同じ `<video>` と既存 EVENT playlist で続ける。commit 後に
mirakc の record が消えて再生元エラーになったときは、詳細が REST で録画状態を取り直してから
原本 HLS / encoded を選ぶ。SSE はヒントなので、この切替を SSE の到着に依存させない。
再生元は詳細を開いたときに選び、再生中は範囲外へのシーク、再生元のエラー、詳細を開き直した
ときだけ選び直す。追っかけの終端は録画ファイルの終端なので選び直さない
（[recordings/playback.md](../recordings/playback.md) §録画のブラウザ再生）。

番組リストと番組表グリッドのダイアログにも同じ着地先への入口を置く。番組画面は
番組一覧 API が番組と同じ放送イベントに対応する録画中録画の `recordings.id` を
`recordingId` として任意で返す。サーバーは `site`・`networkId`・`serviceId`・`eventId`・
`startAt` を使って `recordings` を照合する。`programId` と `recordings.id` は別の
identity なので、リンク先には API が返した `recordingId` を使う。対応する録画が無い場合は、
既存の再生ボタンを従来どおりライブへ向ける。録画が終了・失敗・キャンセル・ごみ箱の
場合も追っかけへは切り替えない。録画状態の SSE 通知を受けた番組表は一覧 API を取り直す。

追っかけの URL は次の固定深さで、mirakc の record id をブラウザ側に持たない。`site` は
前段が site ごとの streamer Service を選ぶために含め、DB 上の録画の site と一致しない
要求は streamer が 404 にする。

```
/api/sites/{site}/recordings/{recordings.id}/chase/playlist.m3u8[?profile=<name>]
/api/sites/{site}/recordings/{recordings.id}/chase/offset/{offset}/playlist.m3u8[?profile=<name>]
/api/sites/{site}/recordings/{recordings.id}/chase/segments/{name}
/api/sites/{site}/recordings/{recordings.id}/chase/offset/{offset}/segments/{name}
/api/sites/{site}/recordings/{recordings.id}/chase/leave
/api/sites/{site}/recordings/{recordings.id}/chase/offset/{offset}/leave
```

ブラウザの再生経路はライブと同じ HLS の梯子（Safari の native HLS / hls.js）を通る。hls.js には `startPosition: 0` を渡す。

追っかけの時間軸は操作バー内のカスタムシークバー 1 本にする。左端は番組開始 (`startAt`)、右端は番組終了予定 (`startAt + durationMs`) である。

録画済みの塗りはファイル先頭 (`max(startedAt, startAt)`) から現在までで導出し、1 秒ごとに伸ばす。`started_at` はチューナーを開いた時刻で、ファイル先頭ではない。予定を超えて録画している間は軸を録画済みまで広げ、予定終端の印を残す。

録画済みの上限はサーバーが追いつくため 1 秒手前にする。追っかけ中は seek tile が無いので、ドラッグ中は時刻だけをプレビューし、映像サムネイルは出さない。

現在位置とプレビュー時刻は番組開始からの経過で表示する。分は 60 を超えても合計分で書く（70 分 12 秒は `70:12`）。チューナーが番組開始より前に開いていても、ファイル先頭と軸の 0 秒は番組開始に揃う。録画が遅れて始まれば、録画済み範囲は軸の途中から始まる。

放送の実時刻はタイトル行にだけ出す。シーク先を録画先頭からの秒 `t` とする。現在のセッション範囲 (`offset` から `offset + seekable.end`) 内なら、`currentTime = t - offset` で移動し、playlist を取り直さない。

開始 `offset` より前なら、その秒を新しい offset として `/chase/offset/{offset}/...` に張り直し、古いセッションへ leave ヒントを送る。変換済み playlist の `seekable` 終端より後でも録画済み上限より前なら、その秒を新しい offset として張り直す。録画の先端（録画済みの 1 秒手前）より後ろは先端に丸める。

張り直す前に再生中だったなら、張り直した後も再生を続ける（原本 HLS と同じ）。原本の commit 後は新しい追っかけセッションを作れないので、範囲外のシークは詳細画面が最新の録画状態で再生元を選び直す。finished でも ingest が `pending` / `transferring` の間は追っかけを続けられる（[recordings/playback.md](../recordings/playback.md) §録画のブラウザ再生）。

ドラッグ中は位置のプレビューだけを出し、pointer up（キーボードでは操作キーを離したとき）に確定する。録画先端へは赤い印か End キーで移動する。専用の「最新」ボタン、秒数入力、「この位置から再生」ボタンは置かない。

マウスを載せただけでは、つまみ・時刻・`aria-valuenow` を動かさない。その時刻を吹き出しで出し、未録画の位置（ファイル先頭より前・先端より後ろ）なら「まだ録画されていません」を添える。

録画の開始が番組開始より遅いとき、録画済みと再生済みの塗りは録画開始位置から始める。デスクトップはシークバーの下に左端・先端・予定終端の目盛りを出す。デスクトップでは先端の印の下に時刻（`25:01`）だけを置き、押すと先端へ移ることは、文言で書かず印のアクセシブル名と title に置く。

延長中は「予定 60:00 ¦」の ¦ を予定終端の印に合わせ、「延長中 · 先端 70:12」を右端に出す。印に合わせると行からはみ出すなら行の中へ寄せ、端のラベルと重なる場合は端のほうを隠す。先端ラベルは隠さず予定ラベルを手前へ寄せる。スマホは目盛りを出さず、時刻を `12:00 / 25:01` と短くする。

つまみを動かさない場合は従来の URL で録画先頭から始め、保存済みの再生位置を復元する。
つまみを動かして 0 秒を明示した場合は、保存済みの位置を復元せず先頭から始める。
画質セレクタは操作バーの再生設定メニューに入れる。プロファイル切替ではセッションを
作り直さず、同じセッションの playlist だけを取り直して再生位置を保つ。

音声もライブと同じ標準 / 主 / 副の 3 択にする。同じ操作バーを持つ再生元は同じように
使えるべきだからである。`?audio=main|sub` を録画詳細の search に置き、標準は省略する。
音声切替は HLS master の URL や追っかけセッションを変えず、プレイヤーが選ぶ rendition
だけを替える。

追っかけの HLS は EVENT playlist で過去の segment を保持する。ライブのように窓が
スライドしないため、PDT を付けず音声を切り替えても再生が止まらないことを
`web/e2e/chase-audio.mjs` の長時間再生判定で確認する。

`durationMs` は録画作成時の番組スナップショットで、watcher は追っかけ中の延長に合わせて
更新しない。録画済みの秒数が予定尺を超えたら、タイムラインの終端を録画済み位置まで
広げ、予定終端を印で残す。未録画側の時刻は予定として表示する。

追っかけは録画再生なので、VOD と共通の `rokuban:playback-rate` に速度を保存し、通常のライブ視聴には適用しない（実時間の放送は先端より先へ進めないため）。
macOS Safari 26.6.2 では、ENDLIST の無い EVENT playlist の 1.25 / 1.5 / 2 倍再生が止まる一方、1 倍は進むため、有限尺になるまで 1 倍に固定し、完了後に保存速度へ戻す。

再生位置は `/api/recordings/{id}/playback-position` に原本時間軸の ms で保存する。
live の配信プロファイルや VOD のエンコードプロファイルでは分けず、原本 VOD と共通の値を使う。
追っかけではプレイヤー内部の相対秒に開始 offset を加えて保存する。成長中の EVENT playlist
では duration を終端とみなさず、視聴済み判定もしない。再生中は 15 秒ごと、pause / seeked /
pagehide で保存する。
画面遷移・`pagehide`・visibility hidden では `POST .../chase/leave` を送る。
オフセット付きなら同じ `/offset/{offset}` を含め、sendBeacon を優先する。
これは共有セッションを即時停止する命令ではなく idle GC を早めるヒントである。
