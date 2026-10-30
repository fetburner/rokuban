# メディア配信（索引）

録画ファイル、ライブ HLS、世代付き録画 HLS、SPA アセットとサービスロゴの配信を扱う。

## 節 → ファイル

各ファイルには、配信方式ごとの仕様を分けて記す。

| 内容 | ファイル |
|---|---|
| 録画済みファイルの Range 配信と X-Accel-Redirect | [media/recording.md](media/recording.md) |
| ライブ視聴の HLS・資源同定・セッション管理・音声 | [media/live-hls.md](media/live-hls.md) |
| 録画中・完了後の世代付き HLS 配信 | [media/playback.md](media/playback.md) |
| SPA アセットとサービスロゴ | [media/assets.md](media/assets.md) |

## 外部から参照されている節名

他の文書から参照する節名と、その所在を示す。

| 節名 | ファイル |
|---|---|
| 録画済みファイルのストリーミング | [media/recording.md](media/recording.md) |
| ライブ視聴の HLS / 遅延の目標は置かない / 資源同定 / 離脱ヒント / 音声 | [media/live-hls.md](media/live-hls.md) |
| 世代付き録画 HLS | [media/playback.md](media/playback.md) |
| SPA アセット配信 / サービスロゴ: ドロップ | [media/assets.md](media/assets.md) |

## 読む順の目安

目的に応じて、読むファイルを選べる。

- 録画ファイル配信 → [media/recording.md](media/recording.md)
- ライブ視聴 → [media/live-hls.md](media/live-hls.md)
- 録画 HLS → [media/playback.md](media/playback.md)
- SPA の配信やロゴ画像 → [media/assets.md](media/assets.md)
