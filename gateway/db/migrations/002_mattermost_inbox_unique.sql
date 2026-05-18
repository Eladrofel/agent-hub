-- 002_mattermost_inbox_unique.sql
-- v0.1.3 Component C: inbox-webhook idempotency.
--
-- The receiver needs ON CONFLICT DO NOTHING semantics so Mattermost's
-- at-least-once outgoing-webhook delivery doesn't double-insert rows when
-- the same post hits the receiver twice (network retry, MM redelivery on
-- 5xx, etc.). The natural dedupe key is (source_post_id, target_agent_id):
-- one row per addressed agent per Mattermost post.
--
-- source_post_id can be NULL (e.g., test rows seeded without a Mattermost
-- post id), so the index is partial — NULLs aren't deduped (callers that
-- emit NULL post_id accept duplicate-handling at the app layer).

CREATE UNIQUE INDEX IF NOT EXISTS uniq_mattermost_inbox_post_target
  ON mattermost_inbox (source_post_id, target_agent_id)
  WHERE source_post_id IS NOT NULL;
