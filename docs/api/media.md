# メディア配信（索引）

録画バイト、ライブ HLS、追っかけ再生、SPA アセットの配信判断を扱う。

## 節 → ファイル

メディア配信の本文は、録画の配信・ライブ視聴・追っかけ再生とブラウザ再生・temp 方式の測定・SPA アセットとロゴの 5 つのファイルに分けてある。節名は分割前のままなので、次の表から該当ファイルを引ける。

| 節 | 内容 | ファイル |
|---|---|---|
| メディア配信 / 録画済みファイルのストリーミング | Range 配信と X-Accel-Redirect | [media/recording.md](media/recording.md) |
| ライブ視聴の HLS | 資源同定・パッケージャ・音声・離脱ヒント | [media/live-hls.md](media/live-hls.md) |
| 録画中の追っかけ再生 / 録画原本のブラウザ再生 | 追っかけ HLS と offset seek | [media/playback.md](media/playback.md) |
| 録画中の temp を読む方式の測定 | temp の追記の遅れ・seek 引数・終わらせ方 | [media/playback-temp-measurement.md](media/playback-temp-measurement.md) |
| SPA アセット配信 / サービスロゴ: ドロップ | SPA とロゴ画像 | [media/assets.md](media/assets.md) |

## 外部から参照されている節名の対応

他の文書やコードは、節名でこの配下の節を名指しして参照している。次の表は節名から該当ファイルを引く対応で、参照を壊さないよう表の節名は変えない。

| 節名 | ファイル |
|---|---|
| 録画済みファイルのストリーミング | [media/recording.md](media/recording.md) |
| ライブ視聴の HLS / 遅延の目標は置かない / パッケージャは MediaMTX の LL-HLS を選べる形にする / 資源同定: セッション ID を持たない / 離脱は「ヒント」であって停止命令ではない / 一覧の契約 / 実装 / 音声（二重音声の主 / 副） | [media/live-hls.md](media/live-hls.md) |
| 録画中の追っかけ再生 / 録画原本のブラウザ再生 / offset seek の精度と開始時間の測定 | [media/playback.md](media/playback.md) |
| 録画中の temp を読む方式の測定 | [media/playback-temp-measurement.md](media/playback-temp-measurement.md) |
| SPA アセット配信 / サービスロゴ: ドロップ | [media/assets.md](media/assets.md) |

### 読む順の目安

話題ごとに、先に開くファイルは次の通り。

- 録画ファイルの Range 配信 → [media/recording.md](media/recording.md)
- ライブ視聴の URL・HLS・音声 → [media/live-hls.md](media/live-hls.md)
- 追っかけ再生・ブラウザ再生 → [media/playback.md](media/playback.md)
- 録画中の temp を読む方式の測定 → [media/playback-temp-measurement.md](media/playback-temp-measurement.md)
- SPA の配信やロゴ画像 → [media/assets.md](media/assets.md)
