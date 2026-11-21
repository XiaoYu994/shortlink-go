-- 007 用 DEFAULT 给存量密钥回填全部 scope，回填已经完成，这里把 DEFAULT 去掉。
-- 留着的话，任何漏写 scopes 的 INSERT 都会悄悄拿到全部权限；
-- 去掉后漏写会因为 NOT NULL 直接报错，权限只能显式授予。
ALTER TABLE api_keys ALTER COLUMN scopes DROP DEFAULT;
