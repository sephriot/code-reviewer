DROP TRIGGER trg_relationship_complete_end_generation_insert;
DROP TRIGGER trg_relationship_complete_end_generation_update;

CREATE TRIGGER trg_relationship_complete_end_generation_insert
BEFORE INSERT ON pr_relationships
WHEN NEW.ended_by_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    JOIN connections AS connection ON connection.id = generation.connection_id
    WHERE generation.id = NEW.ended_by_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
      AND connection.account_database_id = NEW.subject_database_id
      AND (
        (NEW.relationship_kind = 'review_requested'
         AND generation.scope_kind = 'review_requested_search'
         AND generation.scope_key = connection.account_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'authored_by_me'
         AND generation.scope_kind = 'authored_search'
         AND generation.scope_key = connection.account_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'watched'
         AND generation.scope_kind = 'watched_repository')
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'relationship end requires complete reconciliation generation');
END;

CREATE TRIGGER trg_relationship_complete_end_generation_update
BEFORE UPDATE OF ended_by_generation_id ON pr_relationships
WHEN NEW.ended_by_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    JOIN connections AS connection ON connection.id = generation.connection_id
    WHERE generation.id = NEW.ended_by_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
      AND connection.account_database_id = NEW.subject_database_id
      AND (
        (NEW.relationship_kind = 'review_requested'
         AND generation.scope_kind = 'review_requested_search'
         AND generation.scope_key = connection.account_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'authored_by_me'
         AND generation.scope_kind = 'authored_search'
         AND generation.scope_key = connection.account_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'watched'
         AND generation.scope_kind = 'watched_repository')
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'relationship end requires complete reconciliation generation');
END;
