-- major 13

SELECT fi.object_id, OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id),
       ISNULL(ki.name, N''), ISNULL(c.name, N''), ISNULL(FILEGROUP_NAME(fi.data_space_id), N''), fi.is_enabled,
       fi.change_tracking_state_desc, fi.stoplist_id, ISNULL(sl.name, N''), ISNULL(pl.name, N''),
       CAST(1 AS int),
       ISNULL(fi.crawl_type_desc, N''), fi.has_crawl_completed, fi.crawl_start_date, fi.crawl_end_date,
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextItemCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextDocsProcessed') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextFailCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPendingChanges') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPopulateStatus') AS int), 0)
FROM   sys.fulltext_indexes fi
LEFT   JOIN sys.indexes ki ON ki.object_id = fi.object_id AND ki.index_id = fi.unique_index_id
LEFT   JOIN sys.fulltext_catalogs c ON c.fulltext_catalog_id = fi.fulltext_catalog_id
LEFT   JOIN sys.fulltext_stoplists sl ON sl.stoplist_id = fi.stoplist_id
LEFT   JOIN sys.registered_search_property_lists pl ON pl.property_list_id = fi.property_list_id

ORDER  BY OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id)
-- major 14

SELECT fi.object_id, OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id),
       ISNULL(ki.name, N''), ISNULL(c.name, N''), ISNULL(FILEGROUP_NAME(fi.data_space_id), N''), fi.is_enabled,
       fi.change_tracking_state_desc, fi.stoplist_id, ISNULL(sl.name, N''), ISNULL(pl.name, N''),
       CAST(1 AS int),
       ISNULL(fi.crawl_type_desc, N''), fi.has_crawl_completed, fi.crawl_start_date, fi.crawl_end_date,
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextItemCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextDocsProcessed') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextFailCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPendingChanges') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPopulateStatus') AS int), 0)
FROM   sys.fulltext_indexes fi
LEFT   JOIN sys.indexes ki ON ki.object_id = fi.object_id AND ki.index_id = fi.unique_index_id
LEFT   JOIN sys.fulltext_catalogs c ON c.fulltext_catalog_id = fi.fulltext_catalog_id
LEFT   JOIN sys.fulltext_stoplists sl ON sl.stoplist_id = fi.stoplist_id
LEFT   JOIN sys.registered_search_property_lists pl ON pl.property_list_id = fi.property_list_id

ORDER  BY OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id)
-- major 15

SELECT fi.object_id, OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id),
       ISNULL(ki.name, N''), ISNULL(c.name, N''), ISNULL(FILEGROUP_NAME(fi.data_space_id), N''), fi.is_enabled,
       fi.change_tracking_state_desc, fi.stoplist_id, ISNULL(sl.name, N''), ISNULL(pl.name, N''),
       CAST(1 AS int),
       ISNULL(fi.crawl_type_desc, N''), fi.has_crawl_completed, fi.crawl_start_date, fi.crawl_end_date,
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextItemCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextDocsProcessed') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextFailCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPendingChanges') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPopulateStatus') AS int), 0)
FROM   sys.fulltext_indexes fi
LEFT   JOIN sys.indexes ki ON ki.object_id = fi.object_id AND ki.index_id = fi.unique_index_id
LEFT   JOIN sys.fulltext_catalogs c ON c.fulltext_catalog_id = fi.fulltext_catalog_id
LEFT   JOIN sys.fulltext_stoplists sl ON sl.stoplist_id = fi.stoplist_id
LEFT   JOIN sys.registered_search_property_lists pl ON pl.property_list_id = fi.property_list_id

ORDER  BY OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id)
-- major 16

SELECT fi.object_id, OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id),
       ISNULL(ki.name, N''), ISNULL(c.name, N''), ISNULL(FILEGROUP_NAME(fi.data_space_id), N''), fi.is_enabled,
       fi.change_tracking_state_desc, fi.stoplist_id, ISNULL(sl.name, N''), ISNULL(pl.name, N''),
       CAST(1 AS int),
       ISNULL(fi.crawl_type_desc, N''), fi.has_crawl_completed, fi.crawl_start_date, fi.crawl_end_date,
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextItemCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextDocsProcessed') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextFailCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPendingChanges') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPopulateStatus') AS int), 0)
FROM   sys.fulltext_indexes fi
LEFT   JOIN sys.indexes ki ON ki.object_id = fi.object_id AND ki.index_id = fi.unique_index_id
LEFT   JOIN sys.fulltext_catalogs c ON c.fulltext_catalog_id = fi.fulltext_catalog_id
LEFT   JOIN sys.fulltext_stoplists sl ON sl.stoplist_id = fi.stoplist_id
LEFT   JOIN sys.registered_search_property_lists pl ON pl.property_list_id = fi.property_list_id

ORDER  BY OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id)
-- major 17

SELECT fi.object_id, OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id),
       ISNULL(ki.name, N''), ISNULL(c.name, N''), ISNULL(FILEGROUP_NAME(fi.data_space_id), N''), fi.is_enabled,
       fi.change_tracking_state_desc, fi.stoplist_id, ISNULL(sl.name, N''), ISNULL(pl.name, N''),
       fi.index_version,
       ISNULL(fi.crawl_type_desc, N''), fi.has_crawl_completed, fi.crawl_start_date, fi.crawl_end_date,
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextItemCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextDocsProcessed') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextFailCount') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPendingChanges') AS int), 0),
       ISNULL(CAST(OBJECTPROPERTYEX(fi.object_id, 'TableFulltextPopulateStatus') AS int), 0)
FROM   sys.fulltext_indexes fi
LEFT   JOIN sys.indexes ki ON ki.object_id = fi.object_id AND ki.index_id = fi.unique_index_id
LEFT   JOIN sys.fulltext_catalogs c ON c.fulltext_catalog_id = fi.fulltext_catalog_id
LEFT   JOIN sys.fulltext_stoplists sl ON sl.stoplist_id = fi.stoplist_id
LEFT   JOIN sys.registered_search_property_lists pl ON pl.property_list_id = fi.property_list_id

ORDER  BY OBJECT_SCHEMA_NAME(fi.object_id), OBJECT_NAME(fi.object_id)
