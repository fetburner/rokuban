-- +goose Up

CREATE TRIGGER recording_ingest_progress_notify
    AFTER INSERT OR DELETE ON public.recording_ingest_progress
    FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('recordings');

CREATE TRIGGER recording_ingest_progress_generation_notify
    AFTER UPDATE OF temp_generation ON public.recording_ingest_progress
    FOR EACH ROW WHEN (OLD.temp_generation IS DISTINCT FROM NEW.temp_generation)
    EXECUTE FUNCTION public.rokuban_notify('recordings');

-- +goose Down

DROP TRIGGER recording_ingest_progress_generation_notify ON public.recording_ingest_progress;
DROP TRIGGER recording_ingest_progress_notify ON public.recording_ingest_progress;
