// scripts/textlint-rules/ のルールが共有する字数の数え方。段落（paragraph-length.js）と
// 表のセル（table-cell-length.js）で数え方が違うと、同じ文を段落からセルへ移しただけで字数が
// 変わるので、ここに 1 つだけ置く。--rulesdir がディレクトリ内のどのファイルを読むかは
// ドキュメントに無いので、ルールのディレクトリの外に置く（scripts/textlint-allow.js と同じ）。
//
// 字数: ノードに見えている文字列（地の文とインラインコード）の Unicode コードポイント数。
// リンクの URL・インライン HTML・改行とその前後の空白は数えない。全角も半角も 1 字。
'use strict';

const text = (n) =>
  n.type === 'Html' ? '' : n.value ?? (n.children ?? []).map(text).join('');

// chars はノード n の字数を返す。
module.exports = (n) => [...text(n).replace(/[ \t]*\r?\n[ \t]*/g, '')].length;
