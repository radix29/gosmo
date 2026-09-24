-- major 13

SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name, SCHEMA_NAME(tp.schema_id), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       CAST(NULL AS nvarchar(40))
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id
-- major 14

SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name, SCHEMA_NAME(tp.schema_id), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       CONVERT(nvarchar(40), s.last_used_value)
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id
-- major 15

SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name, SCHEMA_NAME(tp.schema_id), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       CONVERT(nvarchar(40), s.last_used_value)
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id
-- major 16

SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name, SCHEMA_NAME(tp.schema_id), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       CONVERT(nvarchar(40), s.last_used_value)
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id
-- major 17

SELECT s.name, SCHEMA_NAME(s.schema_id), s.object_id,
       tp.name, SCHEMA_NAME(tp.schema_id), s.precision, s.scale,
       CONVERT(nvarchar(40), s.start_value),
       CONVERT(nvarchar(40), s.increment),
       CONVERT(nvarchar(40), s.minimum_value),
       CONVERT(nvarchar(40), s.maximum_value),
       s.is_cycling, s.is_cached, ISNULL(s.cache_size, 0),
       CONVERT(nvarchar(40), s.current_value),
       CONVERT(nvarchar(40), s.last_used_value)
FROM   sys.sequences s
JOIN   sys.types tp ON tp.user_type_id = s.user_type_id
