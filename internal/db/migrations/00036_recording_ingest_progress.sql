-- +goose Up

-- ingest 転送の途中経過（issue #212）。
--
-- 「録画は finished なのに再生も事後エンコードもできない」時間帯（原本の
-- 転送中）に、止まっているのか進んでいるのかを UI から判別するための観測。
--
-- ## なぜ recordings の列ではないのか（不変条件 13）
--
-- recordings は「試行の帰結の観測」だけを持つ脊椎で、書き手は watcher /
-- reconciler。ingest ワーカーはそのどちらでもない別のループなので、
-- recording_id を FK に持つ衛星表にする（media_assets と同じ立ち位置）。
--
-- ## なぜ record_sync に載せないのか
--
-- record_sync は mirakc 側の観測（書き手は watcher）で、キーは
-- (site, record_id)。転送の進捗は Rokuban 側のファイルに何バイト書けたかで
-- あって mirakc の観測ではない。同じ表に載せると 1 表 2 書き手になる
-- （不変条件 12）。
--
-- ## 行の意味（不変条件 10）
--
-- **行がある = この録画の原本転送を開始し、最後の観測時点でまだコミットして
-- いない。** 行が無い = 転送が始まっていないか、既に終わっている。
-- 「転送していないこと」を表す行は作らない。
--
-- 行を消すのは 2 経路だけ:
--
-- 1. 原本 media_asset を INSERT する tx（IngestWorker.commit）。コミット =
--    DB 行（不変条件 3）なので、原本行が生まれる瞬間に進捗行が消える ---
--    「原本があるのに取り込み中」という中間状態が読者から見えない。
-- 2. recordings 行の削除（ON DELETE CASCADE）。
--
-- ジョブが失敗して River のバックオフ待ちに入った場合、行は observed_at が
-- 古いまま残る。これは意図した挙動で、「最後に進捗を観測した時刻」から停滞を
-- 読ませるための唯一の材料になる（River の river_job を API 契約に露出させ
-- ないという判断の裏返し。docs/recording/ingest.md §5.6）。
--
-- ## written_bytes は単調増加しない
--
-- ジョブ内リトライ（層 1、Range 再開）は書き込み済みオフセットから追記する
-- ので増える一方だが、ジョブ再試行（層 2）は部分ファイルを truncate して
-- ゼロから作り直す（docs/recording/ingest.md §5.3）。そのとき written_bytes も
-- 0 に戻る。**この戻りを隠さない** --- 隠すと「進んでいるのに終わらない」に
-- 見えて、実際に起きているやり直しが観測できなくなる。この列は「いまファイルに
-- 書けているバイト数」の観測であって、累積の転送実績ではない。
--
-- ## expected_bytes は record_sync.content_length のコピー
--
-- 分母は watcher が観測済みの record_sync.content_length（mirakc record の
-- content.length）を転送開始時に読んで写す。HEAD の Content-Length は転送
-- 完了後の照合（層 3）にしか取っておらず転送中には使えず、ファイル stat は
-- api ロールがファイルシステムに触れない（不変条件 1）ので分母にできない。
-- mirakc が length を返さない record では NULL のままにし、%
-- を出さない（でっち上げた分母を置かない）。
CREATE TABLE recording_ingest_progress (
    recording_id   bigint      PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    written_bytes  bigint      NOT NULL CHECK (written_bytes >= 0),
    expected_bytes bigint      CHECK (expected_bytes IS NULL OR expected_bytes >= 0),
    observed_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE IF EXISTS recording_ingest_progress;
