# フロントエンド（索引）

Vite + React + TypeScript の SPA。go:embed で単一バイナリに同梱し、同じ `dist/` を S3+CDN 配信にも使う。

**本文は `docs/frontend/` に分割してある。** コードコメント・他 doc の「docs/frontend.md「節名」」参照は、下の表で該当ファイルを引ける。

| 内容 | ファイル |
|---|---|
| デザイン言語（トークン / 状態色 / 原則）。**UI を触るなら最初に読む** | [frontend/design.md](frontend/design.md) |
| 前提条件 / 採用スタック / 決め手（技術選定の経緯） | [frontend/stack.md](frontend/stack.md) |
| 共通シェル: ナビゲーション / サイトの扱い（`<SiteGate>`・多サイト時に何が見えるか） / 生成クライアントとエラー運搬 / SSE 通知 / `<html lang>` / PWA | [frontend/shell.md](frontend/shell.md) |
| ホーム（`/`）: 集約するセクション・窓と上限・空セクションの扱い | [frontend/home.md](frontend/home.md) |
| 番組リスト・番組表グリッド（`/programs`） / 日付・チャンネル絞り込み / 容量超過の表示 | [frontend/programs.md](frontend/programs.md) |
| 進行方向の読み込み（時間窓の継ぎ足し） | [frontend/scroll.md](frontend/scroll.md) |
| 予約の操作・予約詳細・録られない理由の表示 | [frontend/reservations.md](frontend/reservations.md) |
| 検索 `/search` とルール条件の共有 | [frontend/search.md](frontend/search.md) |
| 録画一覧・録画検索・録画単体の着地先・ブラウザ再生・ドロップ統計 | [frontend/recordings.md](frontend/recordings.md) |
| ライブ視聴 | [frontend/live.md](frontend/live.md) |
| アセット配信（go:embed / S3+CDN・キャッシュ規約） | [frontend/assets.md](frontend/assets.md) |
| ファビコン・走査線 | [frontend/branding.md](frontend/branding.md) |

外部（コード・他 doc）から参照されている節名の対応:

| 節名 | ファイル |
|---|---|
| 「色は信号のみ」/「トークン外の生の色値を書かない」/「地は「イ」の 3 値」/「合否は画素で測る」 | [frontend/design.md](frontend/design.md) |
| 「番組リスト」/「リストを第一級に置く。グリッドはその上に足す」/「グリッドは lg 以上でのみ出し、モバイルは常にリスト」/「仮想化はライブラリを入れず自前」/「セルの高さに下限を設けない」/「容量超過は番組ではなく区間に描く」/「リスト・予約一覧・モバイル: 同じ文言のバッジ」/「受け入れは実機で行う」 | [frontend/programs.md](frontend/programs.md) |
| 「エラーの本文も UI まで運ぶ」/「サイトの扱い」 | [frontend/shell.md](frontend/shell.md) |
| 「検索とルールは同じ条件 UI を双方向に共有する」 | [frontend/search.md](frontend/search.md) |
| 「録画検索は `/recordings` に同居する」/「debounce と URL 同期で履歴を汚さない」/「ごみ箱タブと検索条件は直交させる」 | [frontend/recordings.md](frontend/recordings.md) |
| §ライブ視聴 / §フロントエンド実装 / §実機確認について | [frontend/live.md](frontend/live.md) |
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
