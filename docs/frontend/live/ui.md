> [live.md](../live.md) の「フロントエンド実装」。索引から辿る

# ライブ画面の分割索引

ライブ画面の文書を節ごとのファイルに分けた索引で、元の節名から担当ファイルを引ける。読む順は、チャンネル選択・画質・音声、再生経路・離脱、予定・予約・チューナー・遅延の 3 組である。

| 元の節・機能 | 内容 | ファイル |
|---|---|---|
| ライブの操作バー・番組軸・チャンネル行 | 操作バー・番組軸・チャンネル一覧 | [controls.md](controls.md) |
| サイトとサービスの同定・選択・リンク | site と service の同定、選択、リンク、能力 API | [selection.md](selection.md) |
| 画質選択と自動降格 | 画質選択と自動降格 | [quality.md](quality.md) |
| 音声・字幕・キー操作 | 音声・字幕・キー操作 | [audio.md](audio.md) |
| ブラウザ再生能力、probe、エラー分類 | ブラウザ再生能力、probe、エラー分類 | [playback.md](playback.md) |
| 離脱ヒントとセッション表示 | 離脱ヒントとセッション表示 | [sessions.md](sessions.md) |
| いま放送中は予定であって観測ではない / 録画予約による中断予測 | 予定番組と予約による割り込み | [planning.md](planning.md) |
| チューナー状態の行 | チューナー射影の表示 | [tuners.md](tuners.md) |
| 遅延・バッファの計器 | 再生経路別の計測 | [metrics.md](metrics.md) |
| 実機確認について | 実機確認の手順 | [verification.md](verification.md) |

3 組の読み先は、順に [selection.md](selection.md)・[quality.md](quality.md)・[audio.md](audio.md)、[playback.md](playback.md)・[sessions.md](sessions.md)、[planning.md](planning.md)・[tuners.md](tuners.md)・[metrics.md](metrics.md) である。

## 外部から参照されている節名の対応

他の文書やコードが名指しで参照している節名と、その節があるファイルの対応を示す。「フロントエンド実装」は、分割した全ファイルに対応する。

| 節名 | ファイル |
|---|---|
| フロントエンド実装 | [controls.md](controls.md), [selection.md](selection.md), [quality.md](quality.md), [audio.md](audio.md), [playback.md](playback.md), [sessions.md](sessions.md), [planning.md](planning.md), [tuners.md](tuners.md), [metrics.md](metrics.md), [verification.md](verification.md) |
| いま放送中は予定であって観測ではない | [planning.md](planning.md) |
| 録画予約による中断予測 | [planning.md](planning.md) |
| チューナー状態の行 | [tuners.md](tuners.md) |
| 遅延・バッファの計器 | [metrics.md](metrics.md) |
| 実機確認について | [verification.md](verification.md) |
