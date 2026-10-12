// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// docs/workflow.md の「1 段落に主張を 4 つ以上乗せない」には判定手段が無い。主張の数は
// 機械で数えられないので、字数を代理指標にして明らかな詰め込みだけを拾う。
//
// textlint のルールにしたので、構文木は textlint が渡し、対象ファイルは docs:lint の
// グロブがそのまま決める。Markdown パーサを自分で読まないので依存は足していない。
//
// 数える単位: 段落ノード（Paragraph）1 つ。最上位の段落に加え、リスト項目と引用の中の
// 段落も 1 単位として数える。リスト項目 1 つに 400 字を詰めるのは、段落に詰めるのと同じ
// 読みにくさであり、除外すると「段落をリスト項目に変える」だけで判定をすり抜けられる。
// 見出し・表のセル・コードブロック・HTML ブロック・frontmatter は段落ノードにならないので
// 数えない。リスト項目が複数段落を持つときは段落ごとに数える。
// `<details>` の中は、開きタグの直後に空行を置いたときだけ段落になり数えられる。空行が
// 無いと中身ごと HTML ブロックになり、このルールも textlint の他のルールも見ない。
//
// 字数: 段落に見えている文字列（地の文とインラインコード）の Unicode コードポイント数。
// リンクの URL・インライン HTML・改行とその前後の空白は数えない。全角も半角も 1 字。
//
// 上限 400 字: 2026-10-12 時点の対象文書（75 ファイル・3,939 単位）で、この単位の
// 中央値 131 字・p90 333 字・最大 2,065 字、400 字超は 226 単位だった。400 字は p90 の上に
// あり、textlint の sentence-length の上限（1 文 120 字）いっぱいの文でも 4 つ並ばないと
// 届かない。通常の段落は拾わず、詰め込みだけを拾う値として選んだ。
//
// 許容件数: 既存の超過はファイルごとの件数で scripts/paragraph-length.allow.json に置く
// （キーはリポジトリのルートからの相対パス、0 件のファイルは書かない）。--rulesdir で読んだ
// ルールには .textlintrc.json のオプションが届かなかった（textlint 15.8.0 で options が {}
// だった）ので、オプションではなくファイルにした。--rulesdir がディレクトリ内のどの拡張子を
// 読むかはドキュメントに無いので、JSON はルールのディレクトリの外に置く。textlint の CLI には
// --max-warnings に当たるものが無く、error が 1 件でもあれば落ちるので、許容はルールの中で持つ。
//
// 実測と許容は一致させる（ラチェット）。多ければ超過した各段落を error にする。少なければ
// 許容を下げる指示を error にする。直した PR が許容を下げ忘れると、別の段落が黙って
// 伸びても件数が合ってしまうからである。
// allow.json にあってファイルが無いキーは、ルールの中からは検出できない（ルールは渡された
// ファイルしか見ない）。改名した場合は新しいパスの許容が 0 になって落ちるので気付ける。
// 削除した場合は検査対象が無いので害は無く、残ったキーの掃除は手作業の点検に任せる。
//
// ルール関数がファイルごとに呼ばれるかはドキュメントに無いので、ファイルをまたぐ状態を
// クロージャに持たない。Document ノードを受けたときに、その木を自分で辿って数える。
'use strict';

const path = require('node:path');
const allow = require('../paragraph-length.allow.json');

const MAX_CHARS = 400;
const ROOT = path.resolve(__dirname, '..', '..');
const ALLOW_FILE = 'scripts/paragraph-length.allow.json';

const text = (n) =>
  n.type === 'Html' ? '' : n.value ?? (n.children ?? []).map(text).join('');

// overlong は木の中の 400 字超の段落を文書順に返す。段落の中に段落は無いので、段落で止まる。
const overlong = (n, out = []) => {
  if (n.type === 'Paragraph') {
    const chars = [...text(n).replace(/[ \t]*\r?\n[ \t]*/g, '')].length;
    if (chars > MAX_CHARS) out.push({ node: n, chars });
  } else {
    for (const c of n.children ?? []) overlong(c, out);
  }
  return out;
};

module.exports = function (context) {
  const { Syntax, RuleError, report, getFilePath } = context;
  return {
    [Syntax.Document](node) {
      const over = overlong(node);
      const file = path.relative(ROOT, path.resolve(getFilePath())).split(path.sep).join('/');
      const allowed = allow[file] ?? 0;
      const n = over.length;
      if (n > allowed) {
        for (const o of over) {
          report(
            o.node,
            new RuleError(
              `段落が ${o.chars} 字（上限 ${MAX_CHARS} 字）。このファイルの ${MAX_CHARS} 字超は ${n} 件で、許容 ${allowed} 件を超えた。表か番号付きの短文に割る`,
            ),
          );
        }
      } else if (n < allowed) {
        report(
          node,
          new RuleError(
            `${MAX_CHARS} 字超の段落は ${n} 件で、許容 ${allowed} 件より少ない。${ALLOW_FILE} の "${file}" を ${n} に下げる（0 なら行ごと消す）`,
          ),
        );
      }
    },
  };
};
