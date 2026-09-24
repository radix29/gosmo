-- major 13

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ISNULL((SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
               WHERE pp.object_id = t.object_id AND pp.index_id = 0
               ORDER BY pp.partition_number), ''),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       CAST(0 AS tinyint),
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.columns ec
                              WHERE ec.object_id = t.object_id
                                AND ec.encryption_type IS NOT NULL)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
WHERE  t.object_id = @p1
-- major 14

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ISNULL((SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
               WHERE pp.object_id = t.object_id AND pp.index_id = 0
               ORDER BY pp.partition_number), ''),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       CAST(0 AS tinyint),
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.columns ec
                              WHERE ec.object_id = t.object_id
                                AND ec.encryption_type IS NOT NULL)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
WHERE  t.object_id = @p1
-- major 15

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ISNULL((SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
               WHERE pp.object_id = t.object_id AND pp.index_id = 0
               ORDER BY pp.partition_number), ''),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       CAST(0 AS tinyint),
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.columns ec
                              WHERE ec.object_id = t.object_id
                                AND ec.encryption_type IS NOT NULL)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
WHERE  t.object_id = @p1
-- major 16

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ISNULL((SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
               WHERE pp.object_id = t.object_id AND pp.index_id = 0
               ORDER BY pp.partition_number), ''),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       t.ledger_type,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.columns ec
                              WHERE ec.object_id = t.object_id
                                AND ec.encryption_type IS NOT NULL)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
WHERE  t.object_id = @p1
-- major 17

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ISNULL((SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
               WHERE pp.object_id = t.object_id AND pp.index_id = 0
               ORDER BY pp.partition_number), ''),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       t.ledger_type,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.columns ec
                              WHERE ec.object_id = t.object_id
                                AND ec.encryption_type IS NOT NULL)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
WHERE  t.object_id = @p1
