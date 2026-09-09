-- major 13

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(CAST('' AS nvarchar(4000)), ''),
       CASE WHEN CAST('' AS nvarchar(4)) = 'ON'
            THEN CAST(1 AS bit) ELSE CAST(0 AS bit) END
FROM   sys.external_data_sources s
-- major 14

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(CAST('' AS nvarchar(4000)), ''),
       CASE WHEN CAST('' AS nvarchar(4)) = 'ON'
            THEN CAST(1 AS bit) ELSE CAST(0 AS bit) END
FROM   sys.external_data_sources s
-- major 15

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(s.connection_options, ''),
       CASE WHEN s.pushdown = 'ON'
            THEN CAST(1 AS bit) ELSE CAST(0 AS bit) END
FROM   sys.external_data_sources s
-- major 16

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(s.connection_options, ''),
       CASE WHEN s.pushdown = 'ON'
            THEN CAST(1 AS bit) ELSE CAST(0 AS bit) END
FROM   sys.external_data_sources s
-- major 17

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(s.connection_options, ''),
       CASE WHEN s.pushdown = 'ON'
            THEN CAST(1 AS bit) ELSE CAST(0 AS bit) END
FROM   sys.external_data_sources s
