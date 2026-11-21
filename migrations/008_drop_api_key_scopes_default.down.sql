ALTER TABLE api_keys
    ALTER COLUMN scopes SET DEFAULT ARRAY['links:read', 'links:write', 'stats:read'];
