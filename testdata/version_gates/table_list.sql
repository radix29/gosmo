-- major 13

SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       CAST(0 AS bit),
       CAST(0 AS bit)
FROM   sys.tables t
-- major 14

SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       t.is_node,
       t.is_edge
FROM   sys.tables t
-- major 15

SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       t.is_node,
       t.is_edge
FROM   sys.tables t
-- major 16

SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       t.is_node,
       t.is_edge
FROM   sys.tables t
-- major 17

SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       t.is_node,
       t.is_edge
FROM   sys.tables t
