// scripts/textlint-rules/ のルールが共有する、ファイルごとの許容件数との照合。
//
// 許容件数: 既存の違反はファイルごとの件数で scripts/<ルール名>.allow.json に置く（キーは
// リポジトリのルートからの相対パス、0 件のファイルは書かない）。--rulesdir で読んだルールには
// .textlintrc.json のオプションが届かなかった（textlint 15.8.0 で options が {} だった）ので、
// オプションではなくファイルにした。--rulesdir がディレクトリ内のどの拡張子を読むかは
// ドキュメントに無いので、JSON もこのファイルもルールのディレクトリの外に置く。textlint の CLI
// には --max-warnings に当たるものが無く、error が 1 件でもあれば落ちるので、許容はルールの
// 中で持つ。
//
// 実測と許容は一致させる（ラチェット）。多ければ各違反を error にする。少なければ許容を
// 下げる指示を error にする。直した PR が許容を下げ忘れると、別の箇所が黙って悪化しても
// 件数が合ってしまうからである。照合の方針を変えるときは全ルールで同時に変えるべきなので、
// ルールごとに書かずここに置く。
// allow.json にあってファイルが無いキーは、ルールの中からは検出できない（ルールは渡された
// ファイルしか見ない）。改名した場合は新しいパスの許容が 0 になって落ちるので気付ける。
// 削除した場合は検査対象が無いので害は無く、残ったキーの掃除は手作業の点検に任せる。
//
// ルール関数がファイルごとに呼ばれるかはドキュメントに無いので、ファイルをまたぐ状態を
// クロージャに持たない。各ルールは Document ノードを受けたときに、その木を自分で辿って数え、
// 見つけた違反をここに渡す。
'use strict';

const path = require('node:path');

const ROOT = path.resolve(__dirname, '..');

// ratchet は allowFile（ルートからの相対パス）を読み、照合関数を返す。what は件数の主語
// （例: "400 字超の段落"）。照合関数は違反 found（{ node, message, padding? } の配列）を
// 許容と比べて report する。
module.exports = function ratchet(allowFile, what) {
  const allow = require(path.join(ROOT, allowFile));
  return (context, doc, found) => {
    const { RuleError, report, getFilePath } = context;
    const file = path.relative(ROOT, path.resolve(getFilePath())).split(path.sep).join('/');
    const allowed = allow[file] ?? 0;
    const n = found.length;
    if (n > allowed) {
      for (const f of found) {
        report(
          f.node,
          new RuleError(
            `${f.message}。このファイルの${what}は ${n} 件で、許容 ${allowed} 件を超えた`,
            f.padding ? { padding: f.padding } : undefined,
          ),
        );
      }
    } else if (n < allowed) {
      report(
        doc,
        new RuleError(
          `${what.trim()}は ${n} 件で、許容 ${allowed} 件より少ない。${allowFile} の "${file}" を ${n} に下げる（0 なら行ごと消す）`,
        ),
      );
    }
  };
};
