-- +goose Up

-- reconcile_pass_snapshots は desired−observed の全件パス（encode / thumbnail / ts_scan）が
-- 完走した最後の時刻を持つ。pass が主キーなので、行の存在は「一度は完走した」という事実である。
-- 未完走の行は作らない（「未成功」の印を入れると、存在を主張として読む規則が崩れる）。
--
-- 書き手は各パスの全件実行だけで、targeted pass では書かない。継続ジョブを持つパスは
-- ページごとに書くので、値の意味は「定期パスが回っている」であり「全候補を見終えた」ではない。
-- 読み手は scrape ごとに読む collector。ScaledJob の --once で動く worker のプロセス内ゲージは
-- 常駐プロセスから読めないので、この表で補う。
-- 置き場を持たないパスだけを入れる。置き場ごとの削除パスは delete_reconcile_snapshots に分ける。
CREATE TABLE public.reconcile_pass_snapshots (
    pass            text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

-- delete_reconcile_snapshots は置き場ごとの削除 reconcile が完走した最後の時刻を持つ。
-- 置き場の名前が主キーなので、置き場を持たない pass と同じ表に混ぜると番兵値が要る。
-- 書き手は FS の置き場の削除 reconcile だけ。S3 の置き場を足すときは、同じ表に行を足す。
CREATE TABLE public.delete_reconcile_snapshots (
    store           text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE public.delete_reconcile_snapshots;
DROP TABLE public.reconcile_pass_snapshots;
