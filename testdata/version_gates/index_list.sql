-- major 13

SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ISNULL(p.data_compression_desc, 'NONE'),
       ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, ''),
       ISNULL(st.no_recompute, CAST(0 AS bit)),
       CAST(0 AS bit),
       ISNULL(h.bucket_count, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
OUTER  APPLY (SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
              WHERE pp.object_id = i.object_id AND pp.index_id = i.index_id
              ORDER BY pp.partition_number) p
LEFT   JOIN sys.stats st ON st.object_id = i.object_id AND st.stats_id = i.index_id
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc
WHERE  i.object_id = @p1 AND i.type > 0
-- major 14

SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ISNULL(p.data_compression_desc, 'NONE'),
       ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, ''),
       ISNULL(st.no_recompute, CAST(0 AS bit)),
       CAST(0 AS bit),
       ISNULL(h.bucket_count, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
OUTER  APPLY (SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
              WHERE pp.object_id = i.object_id AND pp.index_id = i.index_id
              ORDER BY pp.partition_number) p
LEFT   JOIN sys.stats st ON st.object_id = i.object_id AND st.stats_id = i.index_id
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc
WHERE  i.object_id = @p1 AND i.type > 0
-- major 15

SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ISNULL(p.data_compression_desc, 'NONE'),
       ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, ''),
       ISNULL(st.no_recompute, CAST(0 AS bit)),
       i.optimize_for_sequential_key,
       ISNULL(h.bucket_count, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
OUTER  APPLY (SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
              WHERE pp.object_id = i.object_id AND pp.index_id = i.index_id
              ORDER BY pp.partition_number) p
LEFT   JOIN sys.stats st ON st.object_id = i.object_id AND st.stats_id = i.index_id
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc
WHERE  i.object_id = @p1 AND i.type > 0
-- major 16

SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ISNULL(p.data_compression_desc, 'NONE'),
       ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, ''),
       ISNULL(st.no_recompute, CAST(0 AS bit)),
       i.optimize_for_sequential_key,
       ISNULL(h.bucket_count, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
OUTER  APPLY (SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
              WHERE pp.object_id = i.object_id AND pp.index_id = i.index_id
              ORDER BY pp.partition_number) p
LEFT   JOIN sys.stats st ON st.object_id = i.object_id AND st.stats_id = i.index_id
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc
WHERE  i.object_id = @p1 AND i.type > 0
-- major 17

SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ISNULL(p.data_compression_desc, 'NONE'),
       ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, ''),
       ISNULL(st.no_recompute, CAST(0 AS bit)),
       i.optimize_for_sequential_key,
       ISNULL(h.bucket_count, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
OUTER  APPLY (SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
              WHERE pp.object_id = i.object_id AND pp.index_id = i.index_id
              ORDER BY pp.partition_number) p
LEFT   JOIN sys.stats st ON st.object_id = i.object_id AND st.stats_id = i.index_id
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc
WHERE  i.object_id = @p1 AND i.type > 0
