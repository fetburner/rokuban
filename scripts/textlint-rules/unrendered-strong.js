// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// 太字として解釈されない ** を落とす。CommonMark は ** を、両隣の文字（空白か・記号か）で
// 開き・閉じとして扱う。閉じの ** の直前が記号（」・。・バッククォート）で直後が和文の文字だと
// 閉じにならず、GitHub では ** がそのまま表示される（`**必須です。**詳しく` /
// `**「リンク」**で`）。開きも同じで、直前が和文の文字で直後が記号だと開きにならない
// （`が**「一覧」**`）。解釈されなかった ** は、textlint が渡す構文木でも地の文（Str）に残る。
// 2026-10-13 に全対象を `gh api markdown -f mode=markdown` で描画して突き合わせ、Str に残った
// ** と描画に残った ** が 16 ファイル・72 個で一致した。
//
// 数えるもの: Str ノードのソースに含まれる **。インラインコードの中は Code ノードなので
// 数えない。値ではなくソースを見るのは、`\*\*` と書いたエスケープ（値には ** が出る）を
// 数えないためである。既存は 0 件なので許容は持たず、1 件でも error にする。
// 数えないもの: 壊れた ** が別の ** と組になり、** は表示されないが太字の範囲がずれる形
// （`**A。**B**C**`）。構文木の上では正しい Strong なので、このルールからは見えない。
'use strict';

module.exports = function (context) {
  const { Syntax, RuleError, report, getSource, locator } = context;
  return {
    [Syntax.Str](node) {
      const src = getSource(node);
      for (let i = src.indexOf('**'); i !== -1; i = src.indexOf('**', i + 2)) {
        report(
          node,
          new RuleError(
            '** が太字として解釈されず、そのまま表示される。かっこごと太字ならかっこの内側だけを太字にし、句点で終わるなら句点を太字の外に出す。どちらもできなければ ** の外側の、文字に接する側に半角空白を入れる',
            { padding: locator.range([i, i + 2]) },
          ),
        );
      }
    },
  };
};
