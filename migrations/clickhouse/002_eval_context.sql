-- RAG eval context columns on gateway traces (client nexus_eval block).

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS retrieval_contexts String DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS eval_reference String DEFAULT '';
