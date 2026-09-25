-- major 13

SELECT p.name, p.parameter_id, tp.name,
       p.max_length, p.precision, p.scale,
       p.is_output, p.has_default_value,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), p.is_xml_document,
       CAST(0 AS int),
       CAST('' AS nvarchar(60))
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(p.xml_collection_id, 0)
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.parameter_id > 0
ORDER  BY p.parameter_id
-- major 14

SELECT p.name, p.parameter_id, tp.name,
       p.max_length, p.precision, p.scale,
       p.is_output, p.has_default_value,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), p.is_xml_document,
       CAST(0 AS int),
       CAST('' AS nvarchar(60))
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(p.xml_collection_id, 0)
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.parameter_id > 0
ORDER  BY p.parameter_id
-- major 15

SELECT p.name, p.parameter_id, tp.name,
       p.max_length, p.precision, p.scale,
       p.is_output, p.has_default_value,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), p.is_xml_document,
       CAST(0 AS int),
       CAST('' AS nvarchar(60))
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(p.xml_collection_id, 0)
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.parameter_id > 0
ORDER  BY p.parameter_id
-- major 16

SELECT p.name, p.parameter_id, tp.name,
       p.max_length, p.precision, p.scale,
       p.is_output, p.has_default_value,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), p.is_xml_document,
       CAST(0 AS int),
       CAST('' AS nvarchar(60))
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(p.xml_collection_id, 0)
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.parameter_id > 0
ORDER  BY p.parameter_id
-- major 17

SELECT p.name, p.parameter_id, tp.name,
       p.max_length, p.precision, p.scale,
       p.is_output, p.has_default_value,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), p.is_xml_document,
       ISNULL(p.vector_dimensions, 0),
       ISNULL(p.vector_base_type_desc, '')
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(p.xml_collection_id, 0)
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.parameter_id > 0
ORDER  BY p.parameter_id
