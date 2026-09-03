# メディアストレージ（索引）

字幕を有効にしたエンコードは、encoded ファイルの隣に同じ basename の `.vtt`
サイドカーを置く。字幕は `media_assets.kind` を増やさず、
`/file?profile=X&track=subtitles` で encoded アセットから解決する。ARIB 外字・ルビは
libaribcaption のテキスト化により Unicode 私用領域または近似字へ落ちるため、画像字幕と
同じ見た目を保証しない。

録画ファイル・エンコード成果物・サムネイルの保存先は、アプリからは**ただのディレクトリ**として扱う。バックエンドに S3 SDK を組み込まず、`BlobStore` のような抽象化レイヤーも作らない（`os.File` とディレクトリ規約だけ）。

- **ミニ PC / 自宅サーバー**: ローカルディスクのディレクトリをそのまま使う。追加設定なし
- **クラウド / k8s**: CSI ドライバで S3 をマウントし、アプリには透過的に S3 保存させる

**本文は `docs/storage/` に分割してある。節番号は分割前のまま**なので、コードコメントや他 doc の「storage.md §6」等はこの表で該当ファイルを引ける。

| 節 | 内容 | ファイル |
|---|---|---|
| §1〜5 §5.1 | S3 SDK を持たない / FUSE 越し S3 の制約 / **ストレージ契約（4 つのルール）** / クラウド側のマウント選択肢 / 2 階層（録画バッファとアーカイブ・サイジング指針）/ rel_path の名前空間 / サムネイル | [storage/contract.md](storage/contract.md) |
| §6 §7 | **原本 TS の保持ポリシー**（encode policy の凍結とその例外の権威はここ）/ **削除エンジン**（削除可否の述語 / ごみ箱 / 孤児回収の安全弁 / その逆方向の実体無し検出） | [storage/retention.md](storage/retention.md) |
| §8 | catalog エクスポートと rescue（災害復旧） | [storage/rescue.md](storage/rescue.md) |

読む順の目安:

- **ingest / エンコード / サムネイルの出力先を触る** → §3（ストレージ契約）と §5
- **原本削除・ごみ箱・cleanup を触る** → §6 §7（前提として §3 のルール 3「公開 = DB 登録」）
- **災害復旧 / import を触る** → §8

> 関連ドキュメント: [recording.md](recording.md)（ingest パイプライン）/ [data.md](data.md)（データ層）/ [schema.md](schema.md)（`media_assets` / `recording_encode_policy` の DDL）/ [operations.md](operations.md)（運用）
