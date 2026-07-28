-- +goose Up

-- 大量削除サーキットブレーカーの永続状態（issue #24 M2-5、docs/recording.md §3.2
-- 「大量削除サーキットブレーカー」）。
--
-- M1-4 で入れた骨格はパス内で完結していて、閾値超過を検出しても次のパスでは
-- 何も覚えていなかった。「超えたら削除を実行せず停止してアラート。手動確認後に
-- 再開」という設計には、**人間が確認するまで止まり続けるラッチ**が必要で、
-- それはプロセスをまたぐ永続状態である。レベルトリガー設計の中で数少ない
-- 「導出できない状態」— 誰かが確認したという事実は再取得できない。
--
-- 行の存在そのものが「発動中」を意味する。停止していない状態を表す行は無い
-- （00010 の program_overrides と同じ規律。意味を持たない行を作らない）。
-- 再開は行の DELETE。
CREATE TABLE circuit_breakers (
    site       text NOT NULL,
    -- ブレーカーの識別子。internal/breaker の定数に対応する（例: 'ruler_deletes'）。
    -- text + CHECK を置かないのは、ブレーカーの追加をマイグレーションなしで
    -- できるようにするため。値の権威は Go 側の定数（docs/schema.md §1「型の規律」の
    -- 状態は text + CHECK という規約の例外。ここは状態ではなく識別子）。
    name       text NOT NULL,
    tripped_at timestamptz NOT NULL DEFAULT now(),
    -- 発動時に止めた件数と、そのときの閾値。事後に「なぜ止まったか」を
    -- 説明するために両方持つ（閾値は設定変更で変わりうるので、発動時の値を焼く）。
    pending    integer NOT NULL,
    threshold  integer NOT NULL,
    -- 何が消されようとしていたかのサンプル。**手動確認のための材料**であり、
    -- ブレーカー自身のロジックはこの中身を一切使わない不透明なペイロード
    -- （UI が表示するだけ）なので jsonb でよく、内容の CHECK も置かない
    -- （docs/recording.md §4.2「jsonb を許す条件」）。
    detail     jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (site, name)
);

-- SSE ヒント。発動・再開はユーザーに即座に見せたいので専用トピックにする
-- （既存の reservations / rules / recordings とは関心事が違う。00005 のコメント
-- 「トピック名はテーブル名ではなくクライアントの関心事に揃える」）。
CREATE TRIGGER circuit_breakers_notify
    AFTER INSERT OR UPDATE OR DELETE ON circuit_breakers
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('breakers');

-- +goose Down

DROP TRIGGER IF EXISTS circuit_breakers_notify ON circuit_breakers;
DROP TABLE IF EXISTS circuit_breakers;
