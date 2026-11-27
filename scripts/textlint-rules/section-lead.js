// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// docs/workflow.md の「仕様が先、理由はその直後」を節の単位で判定する。結論が節の冒頭に
// 無いと、読み手は節を最後まで読んで自分で組み立てることになる。冒頭が列名や関数名から
// 始まると、識別子を知らない読み手には結論にならない。そこで段落で始まる節では、冒頭に
// 識別子を使わない短い結論の段落を求める。
//
// 数える単位: 節 1 つ（割り方は scripts/textlint-sections.js）。冒頭が段落である節について、
// 次のどれかに当たる節を 1 件と数える。
// 1. その段落がインラインコードを含む
// 2. その段落が 200 字を超える（字数は scripts/textlint-chars.js）
//
// 見出しの直後が段落でない節は数えない。直後が表・リスト・コードブロック・`<details>` の
// 節は、その構造自体が見出しと組になって内容を伝える。下位の見出しが直後に来る節（中身を
// 持たない親節）も同じである。こうした節にまで導入段落を求めると、導入が本文や表の言い換え
// になり、同じ主張が 2 回出る。除外の線を引くより導入文を 1 文求める方が単純だという判断で
// 除外を設けなかった時期もあったが、重複の実害が上回ったので改めた。
// 見出しの深さは問わない。深さで絞ると見出しの段を変えるだけで判定をすり抜けられる。
//
// 上限 200 字: 2026-10-13 時点の対象文書（105 ファイル・754 節）で、冒頭が段落の節は
// 599 節、その段落の中央値 143 字・p75 206 字・p90 280 字だった。sentence-length の上限
// （1 文 120 字）の文なら 1〜2 文が入る。結論を 1〜2 文で言える長さとして選んだ。
// この時点で条件を全部満たす節は 194 節（26%）だった。残りは、冒頭が段落でない 155 節・
// インラインコードを含む 392 節・コードは無いが 200 字超の 13 節である。
//
// 許容件数: 既存の違反はファイルごとの件数で scripts/section-lead.allow.json に置き、
// 実測と一致させる。方式と理由は scripts/textlint-allow.js の冒頭にある。
'use strict';

const ratchet = require('../textlint-allow.js');
const chars = require('../textlint-chars.js');
const { sections } = require('../textlint-sections.js');

const MAX_CHARS = 200;
const check = ratchet('scripts/section-lead.allow.json', '冒頭に結論の段落が無い節');

const hasCode = (n) => n.type === 'Code' || (n.children ?? []).some(hasCode);

// 冒頭に置くだけで内容が伝わる構造。これらで始まる節には導入段落を求めない。
const SELF_CONTAINED = new Set(['Table', 'List', 'CodeBlock', 'Html']);

// problem は節の冒頭ブロック first の違反を文で返す。違反が無ければ undefined を返す。
// 中身を持たない節（first が無い）と、SELF_CONTAINED で始まる節は対象外。
const problem = (first) => {
  if (!first) return undefined;
  if (first.type !== 'Paragraph') {
    return SELF_CONTAINED.has(first.type) ? undefined : '見出しの直後が段落でない';
  }
  if (hasCode(first)) return '冒頭の段落がインラインコードを含む';
  const c = chars(first);
  if (c > MAX_CHARS) return `冒頭の段落が ${c} 字（上限 ${MAX_CHARS} 字）`;
  return undefined;
};

module.exports = function (context) {
  return {
    [context.Syntax.Document](node) {
      const found = [];
      for (const s of sections(node)) {
        const p = problem(s.blocks[0]);
        if (p) {
          found.push({
            node: s.heading,
            message: `${p}。節の冒頭に、識別子を使わない ${MAX_CHARS} 字以内の段落で結論を書く`,
          });
        }
      }
      check(context, node, found);
    },
  };
};
