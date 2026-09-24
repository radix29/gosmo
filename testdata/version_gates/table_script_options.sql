-- major 13

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       (SELECT pp.partition_number AS n, pp.data_compression_desc AS c
        FROM sys.partitions pp WHERE pp.object_id = t.object_id AND pp.index_id = 0
        ORDER BY pp.partition_number
        FOR JSON PATH),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       CAST(0 AS tinyint),
       CAST(0 AS bit),
       CAST('' AS sysname),
       CAST('' AS sysname),
       CAST(NULL AS nvarchar(max)),
       ISNULL(ft.directory_name, ''), ISNULL(ft.filename_collation_name, ''), ISNULL(ft.is_enabled, 0),
       (SELECT OBJECT_NAME(so.object_id) AS v
        FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = t.object_id
        FOR JSON PATH),
       ISNULL(eds.name, ''), ISNULL(eff.name, ''), ISNULL(et.location, ''),
       ISNULL(et.reject_type, ''), et.reject_value, et.reject_sample_value,
       ISNULL(et.remote_schema_name, ''), ISNULL(et.remote_object_name, ''),
       ISNULL(et.distribution_desc, ''), ISNULL(COL_NAME(et.object_id, et.sharding_col_id), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
LEFT   JOIN sys.filetables ft ON ft.object_id = t.object_id
LEFT   JOIN sys.external_tables et ON et.object_id = t.object_id
LEFT   JOIN sys.external_data_sources eds ON eds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats eff ON eff.file_format_id = et.file_format_id
WHERE  t.object_id = @p1
-- major 14

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       (SELECT pp.partition_number AS n, pp.data_compression_desc AS c
        FROM sys.partitions pp WHERE pp.object_id = t.object_id AND pp.index_id = 0
        ORDER BY pp.partition_number
        FOR JSON PATH),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       CAST(0 AS tinyint),
       CAST(0 AS bit),
       CAST('' AS sysname),
       CAST('' AS sysname),
       CAST(NULL AS nvarchar(max)),
       ISNULL(ft.directory_name, ''), ISNULL(ft.filename_collation_name, ''), ISNULL(ft.is_enabled, 0),
       (SELECT OBJECT_NAME(so.object_id) AS v
        FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = t.object_id
        FOR JSON PATH),
       ISNULL(eds.name, ''), ISNULL(eff.name, ''), ISNULL(et.location, ''),
       ISNULL(et.reject_type, ''), et.reject_value, et.reject_sample_value,
       ISNULL(et.remote_schema_name, ''), ISNULL(et.remote_object_name, ''),
       ISNULL(et.distribution_desc, ''), ISNULL(COL_NAME(et.object_id, et.sharding_col_id), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
LEFT   JOIN sys.filetables ft ON ft.object_id = t.object_id
LEFT   JOIN sys.external_tables et ON et.object_id = t.object_id
LEFT   JOIN sys.external_data_sources eds ON eds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats eff ON eff.file_format_id = et.file_format_id
WHERE  t.object_id = @p1
-- major 15

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       (SELECT pp.partition_number AS n, pp.data_compression_desc AS c
        FROM sys.partitions pp WHERE pp.object_id = t.object_id AND pp.index_id = 0
        ORDER BY pp.partition_number
        FOR JSON PATH),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       CAST(0 AS tinyint),
       CAST(0 AS bit),
       CAST('' AS sysname),
       CAST('' AS sysname),
       CAST(NULL AS nvarchar(max)),
       ISNULL(ft.directory_name, ''), ISNULL(ft.filename_collation_name, ''), ISNULL(ft.is_enabled, 0),
       (SELECT OBJECT_NAME(so.object_id) AS v
        FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = t.object_id
        FOR JSON PATH),
       ISNULL(eds.name, ''), ISNULL(eff.name, ''), ISNULL(et.location, ''),
       ISNULL(et.reject_type, ''), et.reject_value, et.reject_sample_value,
       ISNULL(et.remote_schema_name, ''), ISNULL(et.remote_object_name, ''),
       ISNULL(et.distribution_desc, ''), ISNULL(COL_NAME(et.object_id, et.sharding_col_id), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
LEFT   JOIN sys.filetables ft ON ft.object_id = t.object_id
LEFT   JOIN sys.external_tables et ON et.object_id = t.object_id
LEFT   JOIN sys.external_data_sources eds ON eds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats eff ON eff.file_format_id = et.file_format_id
WHERE  t.object_id = @p1
-- major 16

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       (SELECT pp.partition_number AS n, pp.data_compression_desc AS c
        FROM sys.partitions pp WHERE pp.object_id = t.object_id AND pp.index_id = 0
        ORDER BY pp.partition_number
        FOR JSON PATH),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       t.ledger_type,
       t.is_dropped_ledger_table,
       ISNULL(OBJECT_SCHEMA_NAME(t.ledger_view_id), ''),
       ISNULL(OBJECT_NAME(t.ledger_view_id), ''),
       (SELECT TOP (4) lv.name AS v FROM sys.columns lv WHERE lv.object_id = t.ledger_view_id ORDER BY lv.column_id DESC FOR JSON PATH),
       ISNULL(ft.directory_name, ''), ISNULL(ft.filename_collation_name, ''), ISNULL(ft.is_enabled, 0),
       (SELECT OBJECT_NAME(so.object_id) AS v
        FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = t.object_id
        FOR JSON PATH),
       ISNULL(eds.name, ''), ISNULL(eff.name, ''), ISNULL(et.location, ''),
       ISNULL(et.reject_type, ''), et.reject_value, et.reject_sample_value,
       ISNULL(et.remote_schema_name, ''), ISNULL(et.remote_object_name, ''),
       ISNULL(et.distribution_desc, ''), ISNULL(COL_NAME(et.object_id, et.sharding_col_id), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
LEFT   JOIN sys.filetables ft ON ft.object_id = t.object_id
LEFT   JOIN sys.external_tables et ON et.object_id = t.object_id
LEFT   JOIN sys.external_data_sources eds ON eds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats eff ON eff.file_format_id = et.file_format_id
WHERE  t.object_id = @p1
-- major 17

SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       (SELECT pp.partition_number AS n, pp.data_compression_desc AS c
        FROM sys.partitions pp WHERE pp.object_id = t.object_id AND pp.index_id = 0
        ORDER BY pp.partition_number
        FOR JSON PATH),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       t.ledger_type,
       t.is_dropped_ledger_table,
       ISNULL(OBJECT_SCHEMA_NAME(t.ledger_view_id), ''),
       ISNULL(OBJECT_NAME(t.ledger_view_id), ''),
       (SELECT TOP (4) lv.name AS v FROM sys.columns lv WHERE lv.object_id = t.ledger_view_id ORDER BY lv.column_id DESC FOR JSON PATH),
       ISNULL(ft.directory_name, ''), ISNULL(ft.filename_collation_name, ''), ISNULL(ft.is_enabled, 0),
       (SELECT OBJECT_NAME(so.object_id) AS v
        FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = t.object_id
        FOR JSON PATH),
       ISNULL(eds.name, ''), ISNULL(eff.name, ''), ISNULL(et.location, ''),
       ISNULL(et.reject_type, ''), et.reject_value, et.reject_sample_value,
       ISNULL(et.remote_schema_name, ''), ISNULL(et.remote_object_name, ''),
       ISNULL(et.distribution_desc, ''), ISNULL(COL_NAME(et.object_id, et.sharding_col_id), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
LEFT   JOIN sys.filetables ft ON ft.object_id = t.object_id
LEFT   JOIN sys.external_tables et ON et.object_id = t.object_id
LEFT   JOIN sys.external_data_sources eds ON eds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats eff ON eff.file_format_id = et.file_format_id
WHERE  t.object_id = @p1
