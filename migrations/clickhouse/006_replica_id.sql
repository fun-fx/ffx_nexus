-- Multi-node scaling: capture the gateway replica id on every trace so the
-- operator can group by replica (`SELECT count() FROM gateway_traces GROUP BY
-- replica_id`) and detect skew, hot pods, or LB misconfigurations. The column
-- is the value of NEXUS_REPLICA_ID, otherwise "<hostname>-<randhex>".

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS replica_id LowCardinality(String) DEFAULT '';
