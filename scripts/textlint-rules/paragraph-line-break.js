// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// 段落の中で改行しない。段落内の改行（soft line break）は描画で空白になり、和文の途中に
// 空白が入る。2026-10-13 の実測:
// - GitHub のファイル表示と同じ描画（`gh api markdown -f mode=markdown`）は、段落内の改行を
//   HTML でも改行のまま出す（`<br>` にはしない）
// - Chrome 155 はその改行を空白として描画する。「どちらも確認したい。旧ホームで」の幅は、
//   改行なしで 239.2px、改行ありで 244.5px だった
// 原文を読む人（エージェント・エディタ）にも細切れに見え、1 語の修正でも diff が段落全体に
// 広がる。さらに sentence-length の文分割は、かっこの中の改行より後ろを文として数えない。
// 折り返した段落では 120 字超の文が検出されず、このルールを足したときに 70 文が表に出た。
//
// 数えるもの: 段落ノード（Paragraph。リスト項目・引用の中の段落を含む）の中の、地の文・
// インラインコード・インライン HTML に含まれる改行。行末の 2 空白やバックスラッシュによる
// hard break は Break ノードなので数えない。見出し・表・コードブロック・HTML ブロックは
// 段落ノードにならないので数えない。既存は 0 件なので許容は持たず、1 件でも error にする。
'use strict';

const leaves = (n, out = []) => {
  if (!n.children) out.push(n);
  else for (const c of n.children) leaves(c, out);
  return out;
};

module.exports = function (context) {
  const { Syntax, RuleError, report, getSource, locator } = context;
  return {
    [Syntax.Paragraph](p) {
      for (const l of leaves(p)) {
        if (l.type === Syntax.Break) continue;
        const src = getSource(l);
        for (let i = src.indexOf('\n'); i !== -1; i = src.indexOf('\n', i + 1)) {
          report(
            l,
            new RuleError(
              '段落の中に改行がある。描画で空白になるので 1 行につなげる（両側が和文なら詰め、かな・漢字と英数字の間は空白 1 つ、全角の句読点・かっこと英数字の間は詰める）',
              { padding: locator.at(i) },
            ),
          );
        }
      }
    },
  };
};
