-- 003_discovery_search.sql — Phase 4 discovery: tsvector + HNSW (docs/07-discovery.md)
BEGIN;

ALTER TABLE services
    ADD COLUMN IF NOT EXISTS search_document tsvector;

CREATE INDEX IF NOT EXISTS idx_services_search_document
    ON services USING gin (search_document);

CREATE INDEX IF NOT EXISTS idx_embeddings_vec_hnsw
    ON embeddings USING hnsw (vec vector_cosine_ops);

CREATE INDEX IF NOT EXISTS idx_services_kind_status
    ON services (kind, status)
    WHERE status = 'active';

COMMIT;
