# 録画一覧・検索・再生（索引）

録画一覧、詳細、ブラウザ再生、検索、シリーズ、ストレージ残高の判断を扱う。

## 節 → ファイル

録画まわりの判断は話題ごとにファイルが分かれている。読みたい話題の節を、次の表からファイルへ辿る。

| 節 | 内容 | ファイル |
|---|---|---|
| 録画単体の遷移先 / 録画詳細の面積配分と構成 / 次のエピソードとシリーズ | 単体 URL・詳細画面・次のエピソード | [recordings/detail.md](recordings/detail.md) |
| 録画のブラウザ再生 / 視聴状態（続きから・視聴済み） | 原本・派生物の再生と視聴状態 | [recordings/playback.md](recordings/playback.md) |
| CM 検出のロゴ位置 / チャプター（CM の目盛り・自動スキップ・修正） / カット版 / 操作点にサイズを常置する（値札） / 一覧の行は詳細へのリンクにする / キー操作 | ロゴ候補・チャプター・カット版・操作点の寸法・一覧の行・キー操作 | [recordings/chapters.md](recordings/chapters.md) |
| 出自ラベル / 取り込み中 / エンコード状態 / 視聴可否 / ドロップ統計 | 一覧と詳細に出す状態 | [recordings/status.md](recordings/status.md) |
| 録画検索は `/recordings` に同居する / 一括操作 | 検索条件と編集操作 | [recordings/search.md](recordings/search.md) |
| 録画 → ルール / シリーズ一覧 / シリーズの入口 / 番組ハブ | ルール・シリーズ間の移動 | [recordings/series.md](recordings/series.md) |
| ストレージ残高と満杯見込み | 空き容量・消費見込み | [recordings/storage.md](recordings/storage.md) |

## 外部から参照されている節名の対応

次の表は、コードや他の文書が名指しで参照している節名と、その節があるファイルの対応である。

| 節名 | ファイル |
|---|---|
| 録画単体の遷移先 / 録画詳細の面積配分と構成 / 次のエピソードとシリーズ | [recordings/detail.md](recordings/detail.md) |
| 録画のブラウザ再生 / 視聴状態（続きから・視聴済み） | [recordings/playback.md](recordings/playback.md) |
| CM 検出のロゴ位置 / チャプター（CM の目盛り・自動スキップ・修正） / キー操作 | [recordings/chapters.md](recordings/chapters.md) |
| 録画検索は `/recordings` に同居する / 条件は URL に持つ / debounce と URL 同期 / validateSearch / ごみ箱タブ | [recordings/search.md](recordings/search.md) |
| 取り込み中であることを画面に出す / エンコード状態 / 録画が見られるか / ドロップ統計はバッジ + PID 表 | [recordings/status.md](recordings/status.md) |
| ルール名の解決 / シリーズ一覧 / シリーズの入口 / 番組ハブの宛先 | [recordings/series.md](recordings/series.md) |

読む順の目安は次の通り。録画の遷移先と詳細画面は [recordings/detail.md](recordings/detail.md)、再生とチャプター操作は [recordings/playback.md](recordings/playback.md) と [recordings/chapters.md](recordings/chapters.md)、検索と一括操作は [recordings/search.md](recordings/search.md) を読む。シリーズとルールの入口は [recordings/series.md](recordings/series.md)、録画状態と保存量は [recordings/status.md](recordings/status.md) と [recordings/storage.md](recordings/storage.md) を読む。
