// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// `<details><summary>…</summary>` の直後に空行が無いと、CommonMark では中身が空行まで続く
// 1 つの HTML ブロックになる。textlint はそれを Html ノード 1 つとして渡すので、
// sentence-length も段落長も根拠の置き場も中身を検査しない。空行を置けば中身は通常の段落・
// リストになり検査される。空行を忘れた書き方を落とすのがこのルールである。
//
// 数えるもの: ソースが `<details` で始まる Html ノードのうち、開きタグ・
// `<summary>…</summary>`・閉じタグ以外の本文を含むもの。本文のインラインコードで触れた
// `<details>` は Code ノードなので数えない。既存は 0 件なので許容は持たず、1 件でも error にする。
'use strict';

const OPEN = /^\s*<details\b[^>]*>\s*(<summary\b[^>]*>[\s\S]*?<\/summary>)?/i;

const htmlNodes = (n, out = []) => {
  if (n.type === 'Html') out.push(n);
  for (const c of n.children ?? []) htmlNodes(c, out);
  return out;
};

module.exports = function (context) {
  const { Syntax, RuleError, report, getSource } = context;
  return {
    [Syntax.Document](doc) {
      for (const n of htmlNodes(doc)) {
        const src = getSource(n);
        const m = OPEN.exec(src);
        if (!m) continue;
        const body = src.slice(m[0].length).replace(/<\/details>\s*$/i, '').trim();
        if (body !== '') {
          report(
            n,
            new RuleError(
              '<details> の開きタグ（と <summary>）の直後に空行が無く、中身が HTML ブロックになって検査されない。直後に空行を置く',
            ),
          );
        }
      }
    },
  };
};
