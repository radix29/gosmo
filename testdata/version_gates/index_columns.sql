-- major 13

SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column,
       CAST(0 AS tinyint)
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1
-- major 14

SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column,
       CAST(0 AS tinyint)
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1
-- major 15

SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column,
       CAST(0 AS tinyint)
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1
-- major 16

SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column,
       ic.column_store_order_ordinal
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1
-- major 17

SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column,
       ic.column_store_order_ordinal
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1
