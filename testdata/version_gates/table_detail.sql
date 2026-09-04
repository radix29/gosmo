-- major 13

SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc,
       CAST('NON_LEDGER_TABLE' AS nvarchar(60)),
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1
-- major 14

SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc,
       CAST('NON_LEDGER_TABLE' AS nvarchar(60)),
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1
-- major 15

SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc,
       CAST('NON_LEDGER_TABLE' AS nvarchar(60)),
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1
-- major 16

SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc,
       t.ledger_type_desc,
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1
-- major 17

SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc,
       t.ledger_type_desc,
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1
