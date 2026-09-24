-- major 13

SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       CAST(0 AS int)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id  = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
LEFT   JOIN sys.computed_columns cc
       ON  cc.object_id  = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc
       ON  dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic
       ON  ic.object_id  = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN (
       SELECT ic2.object_id, ic2.column_id
       FROM   sys.index_columns ic2
       JOIN   sys.indexes i ON i.object_id = ic2.object_id AND i.index_id = ic2.index_id
       WHERE  i.is_primary_key = 1
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id
-- major 14

SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       ISNULL(c.graph_type, 0)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id  = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
LEFT   JOIN sys.computed_columns cc
       ON  cc.object_id  = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc
       ON  dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic
       ON  ic.object_id  = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN (
       SELECT ic2.object_id, ic2.column_id
       FROM   sys.index_columns ic2
       JOIN   sys.indexes i ON i.object_id = ic2.object_id AND i.index_id = ic2.index_id
       WHERE  i.is_primary_key = 1
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id
-- major 15

SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       ISNULL(c.graph_type, 0)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id  = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
LEFT   JOIN sys.computed_columns cc
       ON  cc.object_id  = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc
       ON  dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic
       ON  ic.object_id  = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN (
       SELECT ic2.object_id, ic2.column_id
       FROM   sys.index_columns ic2
       JOIN   sys.indexes i ON i.object_id = ic2.object_id AND i.index_id = ic2.index_id
       WHERE  i.is_primary_key = 1
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id
-- major 16

SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       ISNULL(c.graph_type, 0)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id  = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
LEFT   JOIN sys.computed_columns cc
       ON  cc.object_id  = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc
       ON  dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic
       ON  ic.object_id  = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN (
       SELECT ic2.object_id, ic2.column_id
       FROM   sys.index_columns ic2
       JOIN   sys.indexes i ON i.object_id = ic2.object_id AND i.index_id = ic2.index_id
       WHERE  i.is_primary_key = 1
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id
-- major 17

SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       ISNULL(c.graph_type, 0)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id  = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
LEFT   JOIN sys.computed_columns cc
       ON  cc.object_id  = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc
       ON  dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic
       ON  ic.object_id  = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN (
       SELECT ic2.object_id, ic2.column_id
       FROM   sys.index_columns ic2
       JOIN   sys.indexes i ON i.object_id = ic2.object_id AND i.index_id = ic2.index_id
       WHERE  i.is_primary_key = 1
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id
