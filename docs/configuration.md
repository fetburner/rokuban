# 設定（索引）

設定は YAML 1 ファイルを入口とし、パースの前に環境変数を展開する。この 1 本だけを設定の入口にする。

## 節 → ファイル

本文は分割してあり、節名は分割前のままである。コードコメントの「configuration.md §やらないこと」などは、この表で該当ファイルを引ける。

分割した本文は `docs/configuration/` にある。

| 節 | 内容 | ファイル |
|---|---|---|
| 方針、展開方式（機微情報の供給） | 設定の入口と環境変数の展開 | [configuration/basics.md](configuration/basics.md) |
| 規約、config と DB の境界、運用補助、やらないこと | キーの書式、設定と DB の分担、採らない仕組み | [configuration/basics.md](configuration/basics.md) |
| スキーマ構造: 関心事別、リファレンス、必須キー、db の運用ノブ、ingest.stall_timeout | キーの一覧と運用ノブ | [configuration/keys.md](configuration/keys.md) |
| スキーマ構造: サーキットブレーカーと検出器の閾値、worker.periodic_jobs と worker.queues、ffmpeg の存在検査、CM 区間の検出 | 閾値、ジョブ、検出 | [configuration/keys.md](configuration/keys.md) |
| スキーマ構造: live、encode/live の HW エンコード | ライブ視聴とエンコードの引数 | [configuration/live.md](configuration/live.md) |
| スキーマ構造: カット版（`encode.profiles[].cut`） | カット版の argv と切る時刻 | [configuration/cut.md](configuration/cut.md) |
| スキーマ構造: mirakc レジストリ（`mirakcs:`） | site の規則とプロセスの site 束縛 | [configuration/sites.md](configuration/sites.md) |
| スキーマ構造: server.allowed_hosts と server.trust_forwarded_host、db は構造化フィールド、webhook のイベントとペイロード | ホスト検査と webhook | [configuration/http.md](configuration/http.md) |

読む順の目安は次の通り。

- 設定の入口・env 展開・CLI フラグの範囲 → basics.md
- 新しいキーを足す、既定値を決める → keys.md（キーの網羅は `config.example.yml`）
- ライブ視聴・HW エンコードを触る → live.md。カット版は cut.md
- 複数 mirakc・site 束縛を触る → sites.md
- Host 検査・リバースプロキシ・webhook を触る → http.md
