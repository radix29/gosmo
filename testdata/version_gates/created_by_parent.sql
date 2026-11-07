-- major 13

SELECT SCHEMA_NAME(t.schema_id), t.name,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.tables p
                              WHERE p.history_table_id = t.object_id AND CAST(0 AS bit) = 1)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables t
WHERE  t.temporal_type = 1 OR CAST(0 AS tinyint) = 1
UNION ALL
SELECT SCHEMA_NAME(v.schema_id), v.name, CAST(0 AS bit)
FROM   sys.views v
WHERE  v.object_id IN (SELECT CAST(NULL AS int) FROM sys.tables t)
-- major 14

SELECT SCHEMA_NAME(t.schema_id), t.name,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.tables p
                              WHERE p.history_table_id = t.object_id AND CAST(0 AS bit) = 1)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables t
WHERE  t.temporal_type = 1 OR CAST(0 AS tinyint) = 1
UNION ALL
SELECT SCHEMA_NAME(v.schema_id), v.name, CAST(0 AS bit)
FROM   sys.views v
WHERE  v.object_id IN (SELECT CAST(NULL AS int) FROM sys.tables t)
-- major 15

SELECT SCHEMA_NAME(t.schema_id), t.name,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.tables p
                              WHERE p.history_table_id = t.object_id AND CAST(0 AS bit) = 1)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables t
WHERE  t.temporal_type = 1 OR CAST(0 AS tinyint) = 1
UNION ALL
SELECT SCHEMA_NAME(v.schema_id), v.name, CAST(0 AS bit)
FROM   sys.views v
WHERE  v.object_id IN (SELECT CAST(NULL AS int) FROM sys.tables t)
-- major 16

SELECT SCHEMA_NAME(t.schema_id), t.name,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.tables p
                              WHERE p.history_table_id = t.object_id AND p.is_dropped_ledger_table = 1)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables t
WHERE  t.temporal_type = 1 OR t.ledger_type = 1
UNION ALL
SELECT SCHEMA_NAME(v.schema_id), v.name, v.is_dropped_ledger_view
FROM   sys.views v
WHERE  v.object_id IN (SELECT t.ledger_view_id FROM sys.tables t)
-- major 17

SELECT SCHEMA_NAME(t.schema_id), t.name,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.tables p
                              WHERE p.history_table_id = t.object_id AND p.is_dropped_ledger_table = 1)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables t
WHERE  t.temporal_type = 1 OR t.ledger_type = 1
UNION ALL
SELECT SCHEMA_NAME(v.schema_id), v.name, v.is_dropped_ledger_view
FROM   sys.views v
WHERE  v.object_id IN (SELECT t.ledger_view_id FROM sys.tables t)
