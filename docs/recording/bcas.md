> [recording.md](../recording.md) §6「B-CAS 復号の責務境界」。索引から辿る。

## 6. B-CAS 復号の責務境界

B-CAS の復号はエッジの責務である。スクランブルが残っていれば、原本解析で検出する。

### 6.1 復号はエッジ（mirakc パイプライン）の責務

復号は、mirakc が編成する外部ツールがエッジで行う。mirakc から出る TS は復号済みなので、Rokuban のシステム内に暗号化された TS は現れない。

MULTI2 スクランブルの復号（B-CAS カードによる鍵処理 + デスクランブル）を実行するのは、mirakc 本体でなく、mirakc が編成する外部ツールである。構成は次の 2 通りである。

1. チューナーコマンド段で復号する。`recpt1 -b25` 等がチューナー読み出しと同時に libaribb25 + PC/SC カードリーダーで復号する。mirakc には `tuners[].decoded: true` を設定する
2. mirakc のフィルタで復号する。`filters.decode-filter` に arib-b25-stream-test（libaribb25 系）等を指定する

どちらでも mirakc の録画パイプラインと Web API（ライブ・records `/stream`）から出る TS は復号済みである。したがって **Rokuban のシステム内に暗号化された TS は一切現れない**。B-CAS カード・カードリーダー（pcscd）・libaribb25 の用意は、ISDBScanner と同様、エッジ環境構築時のセットアップ事項としてインストールドキュメントで扱う（アーキテクチャの構成要素ではない）。

ドキュメントでは、正規の B-CAS カード + PC/SC リーダー構成のみを扱う。カードエミュレーション類は法的にグレーなため扱わない。

### 6.2 scrambled カウントは復号障害の検出器

原本解析で数える scrambling_control ビットは、復号が正常なら常にゼロのはずである。**scrambled > 0 は放送品質でなくエッジ環境の異常**（B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味する。そのためドロップ数とは別枠のアラート対象とする（EPGStation ドロップログの scramble 列と同じ役割）。
