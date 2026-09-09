-- major 13
AND t.is_ms_shipped = 0 AND t.is_filetable = 0 AND t.is_external = 0 AND NOT (CAST(0 AS bit) = 1 OR CAST(0 AS bit) = 1)
AND t.is_ms_shipped = 0 AND (CAST(0 AS bit) = 1 OR CAST(0 AS bit) = 1)
-- major 14
AND t.is_ms_shipped = 0 AND t.is_filetable = 0 AND t.is_external = 0 AND NOT (t.is_node = 1 OR t.is_edge = 1)
AND t.is_ms_shipped = 0 AND (t.is_node = 1 OR t.is_edge = 1)
-- major 15
AND t.is_ms_shipped = 0 AND t.is_filetable = 0 AND t.is_external = 0 AND NOT (t.is_node = 1 OR t.is_edge = 1)
AND t.is_ms_shipped = 0 AND (t.is_node = 1 OR t.is_edge = 1)
-- major 16
AND t.is_ms_shipped = 0 AND t.is_filetable = 0 AND t.is_external = 0 AND NOT (t.is_node = 1 OR t.is_edge = 1)
AND t.is_ms_shipped = 0 AND (t.is_node = 1 OR t.is_edge = 1)
-- major 17
AND t.is_ms_shipped = 0 AND t.is_filetable = 0 AND t.is_external = 0 AND NOT (t.is_node = 1 OR t.is_edge = 1)
AND t.is_ms_shipped = 0 AND (t.is_node = 1 OR t.is_edge = 1)
