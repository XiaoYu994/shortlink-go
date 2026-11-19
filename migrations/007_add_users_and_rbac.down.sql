-- 索引先于列删除，避免删列带走索引后 DROP INDEX 失败。
DROP INDEX idx_api_keys_created_by;

ALTER TABLE api_keys
    DROP COLUMN created_by,
    DROP COLUMN scopes;

DROP TABLE invitations;
DROP TABLE sessions;
DROP TABLE memberships;
DROP TABLE user_identities;
DROP TABLE users;
