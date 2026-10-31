-- +goose Up

-- transfer_backlog_snapshots は、storage.transfer.backlog_limit を設定した置き場ごとに、
-- encode reconcile が直近に数えた転送待ちの論理資産数と、数えた時刻を持つ。
--
-- counted_at は、数え始めた時刻（保存した時刻ではない）。DB の時計から取った値を呼び出し側が渡し、保存済みより新しいときだけ
-- 書き換える。並行するパスで、先に数えた古い件数が後から保存されて新しい件数を戻すのを防ぐ。
-- 読み手は、件数の古さを見る collector（rokuban_storage_transfer_backlog_counted_timestamp_seconds）。
--
-- 行の意味: 「この置き場は、上限付きの置き場として少なくとも 1 回数えた」。
-- 数えていない置き場の行は作らない。設定から上限を外した置き場の行は、
-- 次に数えるパスが消す（レベルトリガー）。
--
-- 止まっているか（assets >= 上限）は列にしない。上限は設定の値で、設定を変えれば
-- 同じ件数でも答えが変わるので、読む側が件数と今の上限から導出する（不変条件 9）。
-- 書き手は encode reconcile だけで、導出表なので、全件パスが書き直せる（不変条件 12）。
-- 常駐プロセスの scrape と、ScaledJob の --once で毎回空から始まる worker の
-- Warn / Info の判定が、プロセスをまたいで同じ値を見るために DB に置く。
CREATE TABLE public.transfer_backlog_snapshots (
    store      text PRIMARY KEY,
    assets     bigint NOT NULL CHECK (assets >= 0),
    counted_at timestamptz NOT NULL
);

-- +goose Down

DROP TABLE public.transfer_backlog_snapshots;
