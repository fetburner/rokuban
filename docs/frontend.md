# フロントエンド（索引）

Vite + React + TypeScript の SPA。go:embed で単一バイナリに同梱し、同じ `dist/` を S3+CDN 配信にも使う。

| 内容 | ファイル |
|---|---|
| デザイン言語（トークン / 状態色 / 原則）。**UI を触るなら最初に読む** | [frontend/design.md](frontend/design.md) |
| 前提条件 / 採用スタック / 技術選定の決め手 | [frontend/stack.md](frontend/stack.md) |
| 共通シェル: ナビゲーション / サイトの扱い（多サイト時に何が見えるか） / 生成クライアントとエラー運搬 / SSE 通知 / `<html lang>` / PWA | [frontend/shell.md](frontend/shell.md) |
| ホーム（`/`）: 集約するセクション・窓と上限・空セクションの扱い | [frontend/home.md](frontend/home.md) |
| 番組リスト・番組表グリッド（`/programs`） / 日付・チャンネル絞り込み / 容量超過の表示 | [frontend/programs.md](frontend/programs.md) |
| 進行方向の読み込み（時間窓の継ぎ足し） / ボトムタブの裏に隠れる行（到達可能性のみ保証） | [frontend/scroll.md](frontend/scroll.md) |
| 予約の操作・予約詳細・録られない理由の表示 | [frontend/reservations.md](frontend/reservations.md) |
| 検索 `/search` とルール条件の共有 | [frontend/search.md](frontend/search.md) |
| 録画一覧・詳細・ブラウザ再生・検索・シリーズ・ストレージ残高 | [frontend/recordings.md](frontend/recordings.md) |
| ライブ視聴・録画中の追っかけ再生 UI | [frontend/live.md](frontend/live.md) |
| アセット配信（go:embed / S3+CDN・キャッシュ規約） | [frontend/assets.md](frontend/assets.md) |
| ファビコン・走査線 | [frontend/branding.md](frontend/branding.md) |

**本文は `docs/frontend/` に分割してある。** コードコメントや他 doc にある「docs/frontend.md「節名」」形式の参照は、下の表で該当ファイルを引ける。

外部（コード・他 doc）から参照されている節名の対応:

| 節名 | ファイル |
|---|---|
| 「色は信号のみ」/「トークン外の生の色値を書かない」/「地は「イ」の 3 値」/「合否は画素で測る」 | [frontend/design.md](frontend/design.md) |
| 「番組リスト」/「リストを第一級に置く。グリッドはその上に足す」/「グリッドは lg 以上でのみ出し、モバイルは常にリスト」/「仮想化はライブラリを入れず自前」/「セルの高さに下限を設けない」/「容量超過は番組ではなく区間に描く」/「リスト・予約一覧・モバイル: 同じ文言のバッジ」/「受け入れは実機で行う」 | [frontend/programs.md](frontend/programs.md) |
| 「エラーの本文も UI まで運ぶ」/「サイトの扱い」 | [frontend/shell.md](frontend/shell.md) |
| 「検索とルールは同じ条件 UI を双方向に共有する」 | [frontend/search.md](frontend/search.md) |
| 録画単体の着地先 / 録画詳細の面積配分と構成 / 次のエピソードとシリーズ | [frontend/recordings/detail.md](frontend/recordings/detail.md) |
| 録画のブラウザ再生 / 視聴状態（続きから・視聴済み） | [frontend/recordings/playback.md](frontend/recordings/playback.md) |
| CM 検出のロゴ位置 / チャプター（CM の目盛り・自動スキップ・修正） / キー操作 | [frontend/recordings/chapters.md](frontend/recordings/chapters.md) |
| 録画検索は /recordings に同居する / 条件は URL に持つ / debounce と URL 同期 / validateSearch / ごみ箱タブ | [frontend/recordings/search.md](frontend/recordings/search.md) |
| 取り込み中であることを画面に出す / エンコード状態 / 録画が見られるか / ドロップ統計はバッジ + PID 表 | [frontend/recordings/status.md](frontend/recordings/status.md) |
| ルール名の解決 / シリーズ一覧 / シリーズの導線 / 番組ハブの宛先 | [frontend/recordings/series.md](frontend/recordings/series.md) |
| §ライブ視聴 | [frontend/live.md](frontend/live.md) |
| §フロントエンド実装 | [frontend/live/ui.md](frontend/live/ui.md) |
| 画質選択と自動降格 | [frontend/live/quality.md#画質選択と自動降格](frontend/live/quality.md#画質選択と自動降格) |
| いま放送中は予定であって観測ではない / 録画予約による中断予測 | [frontend/live/planning.md](frontend/live/planning.md) |
| 遅延・バッファの計器 | [frontend/live/metrics.md](frontend/live/metrics.md) |
| §実機確認について | [frontend/live/verification.md](frontend/live/verification.md) |
| アセット配信 | [frontend/assets.md](frontend/assets.md) |

読む順の目安:

- **色・余白・状態表示を触る（= UI を触るなら常に）** → [frontend/design.md](frontend/design.md)
- **ホームを触る** → [frontend/home.md](frontend/home.md)
- **番組表・番組リストを触る** → [frontend/programs.md](frontend/programs.md)（時間窓の継ぎ足しは [frontend/scroll.md](frontend/scroll.md)）
- **録画一覧を触る** → [frontend/recordings.md](frontend/recordings.md)
- **ライブ視聴を触る** → [frontend/live.md](frontend/live.md)（資源同定は [api.md](api.md) §ライブ視聴の HLS が権威）
- **配信・キャッシュ・CDN を触る** → [frontend/assets.md](frontend/assets.md)
- **新しい画面・共通 UI を足す** → [frontend/shell.md](frontend/shell.md) と [frontend/stack.md](frontend/stack.md)

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [api.md](api.md)（REST・SSE・メディア配信）/ [data.md](data.md)（データ層。SSE = invalidate の対称性）/ [runbook.md](runbook.md)（実機確認の手順）
