> [live.md](../live.md) の音声・字幕・キー操作。索引から辿る

### 音声・字幕・キー操作

**音声（二重音声の主 / 副）を選ぶ UI を持つ。**`標準` / `主音声` / `副音声` の 3 択で、選択は `?audio=main|sub` に持つ（標準は書かない）。streamer は 3 本の音声 rendition を常に出しているので（[api.md](../../api.md) §音声）、**切替は `LivePlayer` が取るトラックを替えるだけ**である。プレイリストの取り直しもセッションの作り直しも無く、同じチャンネルを見ている他の視聴者にも影響しない。

- **常に 3 択で出す。** streamer はセッション開始時に ffprobe で音声 ES 数を調べる。1 本なら従来どおり L / R を両耳へ写し、複数なら 1 本目を標準 / 主、2 本目を副に使う（[api.md](../../api.md) §音声）。1 ES の通常ステレオで主 / 副を選ぶと片側だけになる。
- **ライブの音声 ES 数はセッション開始時に固定する。** ffmpeg の map は起動後に変えられない。次の番組で本数が変わったときの追従は未解決（[api.md](../../api.md) §ライブ視聴の HLS）
- **トラックは位置で選ぶ**（0 = 標準 / 1 = 主 / 2 = 副。`liveAudioTrackIndex`）。hls.js は `audioTrack`、WebKit のネイティブ経路は `video.audioTracks` の `enabled` を使う
- **メイン effect の依存に `audio` を入れない。** 入れると切替のたびに hls.js を作り直す。画質の切替などで作り直されたときは、トラック一覧が届いた時点（`AUDIO_TRACKS_UPDATED` / `addtrack`）で URL の選択に揃え直す
- **チャンネルを切り替えても音声を保つ**（一覧のリンクが `?audio=` を運ぶ）
- **判定は `web/e2e/live-audio.mjs`**（実ブラウザ + ffmpeg が書き続ける実ライブ HLS）。Chromium は WebAudio で左右の周波数を測る。主 → 副 → 標準 → 主とそれぞれ聴いてから切り替える。WebKit は `audioTracks` と取得する rendition を見る（ネイティブ HLS の音は WebAudio に来ないので、鳴っている音は測れない）

**切替を跨いで持ち越すのは、字幕の表示と「再生中だったか」である。** cleanup 時に `<video>` のトラック状態と `paused` を読む。再生中なら `canplay` を待って再開する（[quality.md](quality.md)「降格のあとは再生を再開する」）。

**字幕は hls.js が新しいマニフェストを読むと既定選択へ戻すため、表示状態を戻す。** 実装確認では `SubtitleTrackController.onManifestLoading` が `trackId` を `-1` に戻す。`MANIFEST_PARSED` で `subtitleDisplay` を設定し直す。その時点では既定トラックはまだ選ばれていない（`onManifestParsed` はトラック一覧を入れるだけ）ので、この設定は後の選択のときに `toggleTrackModes` が読む値として効く。

ネイティブ経路は `loadedmetadata` でトラックの mode を揃え直す。

上の字幕を戻す因果（hls.js が選択を既定に戻す → `MANIFEST_PARSED` で設定し直す → 後の選択で効く）は実ブラウザで未検証である（判定手段がまだ無い）。

<details><summary>根拠</summary>

- 実装: hls.js 1.7.1 の `SubtitleTrackController.onManifestLoading` / `onManifestParsed`、`toggleTrackModes`
- 配線: `LivePlayer` の cleanup、`MANIFEST_PARSED`、ネイティブ `loadedmetadata`

</details>

**音量とミュートは持ち越す必要が無い（実測）。** cleanup は `removeAttribute('src')` + `load()` を行うが、`load()` は音量・ミュートを既定に戻さない。**Chromium と WebKit の両方で実測した。** 復元のコードを外したビルドでも、切替後に `video.volume` と `video.muted` が保たれる（`web/e2e/live.mjs` の ⑨）。

したがって ⑨ が守っているのは**切替で `LivePlayer` を作り直さないこと**の側である。`key={selectedProfile}` を足す変異で音量・ミュートの両方が失われて ⑨ が落ちることを確認済み。

**判定は `web/e2e/live.mjs` の ⑨ にある**（実ブラウザ + 実バイナリ）。jsdom では原理的に測れない 2 点 —— 一覧が遅れて届いてもプレイリストを取り直さないこと、切替で `<video>` が作り直されないこと —— を含む。⑨ は `key={selectedProfile}` の変異で 4 件の NG（遅延到着での取り直し・離脱ヒントが飛ぶ・音量が失われる・ミュートが失われる）を出すことを確認してある。

**字幕の持ち越しも実ブラウザで実測してある。** フィクスチャに字幕レンディション（master playlist + WebVTT）を持たせ、`live.mjs` の ⑩ が 2 方向を見る。

- **hls.js 経路（Chromium）**: 利用者が切ってから画質を切り替えると `mode` は `hidden` のまま（再表示されない）。上の `MANIFEST_PARSED` の 1 行を外すと `showing` に戻る
- **ネイティブ経路（WebKit）**: 利用者が入にしてから切り替えると `showing` のまま。**`loadedmetadata` で揃えるだけでは足りなかった** —— 実測で `disabled` に戻った（WebKit は `loadedmetadata` の時点でまだトラックを作っていない）。そこで `TextTrackList` の `addtrack` でも適用している

**WebKit の字幕は既定で非表示である**（レンディションに `DEFAULT=YES` を書いても Safari は利用者が有効にするまで出さない。実測）。したがって 2 方向を見ている —— hls.js 経路は「切ったまま」、ネイティブ経路は「入にしたまま」。

**ライブでも Space（再生 / 一時停止）・M（ミュート）・F（フルスクリーン）を使える。** 操作バーに同じボタンがあるので、キー操作も同じにする。

シークのページショートカット（← / →・J / L・0〜9）はライブに適用しない。番組軸は録画の再生時間軸ではなく、0〜9 の割合シークにも有限の尺がない。

入力欄・選択欄・リンク・ボタン・編集可能領域・`[role="slider"]` にフォーカスがある間はページショートカットを処理しない。`<video>` は除外しない。`controls={false}` の自前操作バーなので、ネイティブ controls との二重処理はない。

キー操作の適用表は [recordings/chapters.md](../recordings/chapters.md) §キー操作を参照する。

端末共通の速度設定も通常のライブには適用しない。ネイティブ HLS の `playbackRate` が実 Safari で有効かは未検証であり、速度変更をライブへ広げる根拠はない。
