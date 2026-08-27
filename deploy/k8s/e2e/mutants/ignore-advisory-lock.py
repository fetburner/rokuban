"""判定 4 の変異: `pg_try_advisory_lock` の戻り値を無視する。

リポジトリの**複製**（rsync したツリー）に当てる。元のツリーは触らない ---
イメージのビルド中にワークツリーが変異した状態になっていると、並行して走る
他の作業がその状態を読む。

**コンパイルが通る形で壊す。** `acquired` を単に使わないと未使用変数で
ビルドが落ち、判定が「FAIL になった」ではなく「イメージが無い」で赤くなる
（CLAUDE.md §テスト規律「落ち方を確認する」）。

使い方: python3 ignore-advisory-lock.py <ツリーのルート>
"""

import pathlib
import sys

TARGET = "internal/role/leader.go"

OLD = """\t\tif !acquired {
\t\t\tconn.Release()
\t\t\tsleepWithJitter(ctx, cfg.PollInterval)
\t\t\tcontinue
\t\t}
"""

NEW = """\t\t// MUTANT: 取得できなかったことを無視して先へ進む。
\t\t_ = acquired
"""


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 64
    path = pathlib.Path(sys.argv[1]) / TARGET
    # LC_ALL=C の環境でも読めるように明示する（leader.go は日本語を含む）。
    src = path.read_text(encoding="utf-8")
    if OLD not in src:
        print(
            f"mutation target not found in {path}. "
            "RunSingleton のロック取得分岐が変わっている --- 変異を当て直すこと",
            file=sys.stderr,
        )
        return 1
    path.write_text(src.replace(OLD, NEW, 1), encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
