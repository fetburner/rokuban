# ライブ視聴（索引）

ライブ視聴 UI の判断を扱う。配信経路は [api.md](../api.md) のメディア配信を参照する。

## 節 → ファイル

ライブ視聴の文書は、画面とセッションの基本と、操作・選択・再生・状態表示の各判断の 2 つに分けてある。

| 節 | 内容 | ファイル |
|---|---|---|
| 実装方式 / セッション管理 / 録画中の追っかけ再生 | 画面とセッションの基本 | [live/overview.md](live/overview.md) |
| フロントエンド実装 | 操作・選択・再生・状態表示の各判断 | [live/ui.md](live/ui.md) |

## 外部から参照されている節名の対応

次の表は、コードや他の文書が名指しで参照している節名と、その節があるファイルの対応である。

| 節名 | ファイル |
|---|---|
| ライブ視聴 | [live/overview.md](live/overview.md) |
| フロントエンド実装 | [live/ui.md](live/ui.md) |
| 実機確認について | [live/verification.md](live/verification.md) |
| 既定は URL に書き戻さない | [live/quality.md](live/quality.md) |
| いま放送中は予定であって観測ではない | [live/planning.md](live/planning.md) |
| 遅延・バッファの計器 | [live/metrics.md](live/metrics.md) |

読む順の目安は次の通り。

- ライブ画面の実装 → [live/ui.md](live/ui.md)
- HLS 配信・資源同定・音声 rendition → [api.md](../api.md) のメディア配信
- 録画追っかけ再生 → [live/overview.md](live/overview.md) とメディア配信の追っかけ再生
