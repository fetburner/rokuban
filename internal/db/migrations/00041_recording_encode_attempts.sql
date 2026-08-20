-- +goose Up

-- encode ジョブの直近の試行状態（issue #316）。
--
-- desired（recordings.encode_profiles）と observed（media_assets の active な
-- encoded）の差だけでは「まだ来ていない」としか言えず、「いま走っている」と
-- 「失敗して再試行待ち」を区別できない。この表はその区別だけを持つ。
--
-- ## なぜ recordings の列ではないのか（不変条件 13）
--
-- recordings は「試行の帰結の観測」だけを持つ脊椎で、書き手は watcher /
-- reconciler。EncodeWorker はそのどちらでもない別のループなので、
-- recording_id を FK に持つ衛星表にする（recording_ingest_progress と同じ
-- 立ち位置）。
--
-- ## River の river_job を読まない
--
-- docs/recording/ingest.md §5.6 と共通なのは「river_job を API 契約に露出
-- させない」ことだけ。§5.6 は「リトライ中」と「取り込み待ち」を API に持たず
-- 区別しないと決めたが、この表は逆に running/failed を持たせて区別する
-- （queued/running/failed が API に出る理由そのもの）。
--
-- river_job の state を直接引いて queued/running/failed に写す設計も検討したが
-- 採らなかった。EncodeReconcileWorker は discarded になった encode ジョブを
-- 「encoded が無い」という観測が続く限り 15 分ごとに再投入し続けるが、これは
-- 録画単位の恒久失敗（入力ファイルの破損など）に限る --- 恒久失敗の代表格
-- （設定から消えたプロファイル）は known_profiles の絞り込みで投入対象から
-- 除外しているので、そちらは一度失敗させたあとは再投入されない
-- （internal/worker/encode_reconcile.go）。どちらにしても river_job の「いま」の
-- state だけでは「一度も失敗していない」と「直前に失敗して再投入された直後」を
-- 見分けられない。この表は EncodeWorker が試行の開始・成功・失敗をそのタイミング
-- で明示的に書くので、river の内部状態（リトライ回数・バックオフ）を一切
-- 露出させずに済む。
--
-- ## 行の意味（不変条件 10）
--
-- 行が無い = このプロファイルの試行がまだ始まっていない（queued）か、既に
-- 完了している（encoded 資産の有無で見る）。「待っている」ことを表す行は
-- 作らない。行があるときだけ running / failed のどちらかを主張する。
--
-- 行を消すのは 2 経路だけ:
--
-- 1. runEncode（EncodeWorker）の defer が、成功（戻り値 err == nil）で
--    試行行を消す。DELETE は commitEncoded の直後ではない --- 間に webhook
--    通知（HTTP、タイムアウトまで待つ）が入り、同一トランザクションでもない。
--    「完了しているのに失敗中」という中間状態を読者に見せないのは、この DELETE
--    の速さではなく、API 側が encoded 資産のあるプロファイルを
--    encodeJobStatusesFromFields の対象から先に除外しているため（試行行が
--    多少遅れて残っていても読まれない）。
-- 2. recordings 行の削除（ON DELETE CASCADE）。
--
-- ## PK が (recording_id, profile) の複合である理由
--
-- 1 録画に複数プロファイルを事後追加できる（issue #133）ため、プロファイル
-- ごとに独立した試行状態を持つ。
CREATE TABLE recording_encode_attempts (
    recording_id bigint      NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    profile      text        NOT NULL,
    state        text        NOT NULL CHECK (state IN ('running', 'failed')),
    error        text,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recording_id, profile)
);

-- +goose Down

DROP TABLE IF EXISTS recording_encode_attempts;
