-- major 13

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(CAST('' AS nvarchar(4000)), ''),
       ISNULL(CAST('' AS nvarchar(4)), '')
FROM   sys.external_data_sources s
-- major 14

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(CAST('' AS nvarchar(4000)), ''),
       ISNULL(CAST('' AS nvarchar(4)), '')
FROM   sys.external_data_sources s
-- major 15

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(s.connection_options, ''),
       ISNULL(s.pushdown, '')
FROM   sys.external_data_sources s
-- major 16

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(s.connection_options, ''),
       ISNULL(s.pushdown, '')
FROM   sys.external_data_sources s
-- major 17

SELECT s.name, s.data_source_id, s.location,
       ISNULL(s.type_desc, ''),
       ISNULL(s.resource_manager_location, ''),
       ISNULL((SELECT c.name FROM sys.database_scoped_credentials c
               WHERE c.credential_id = s.credential_id), ''),
       ISNULL(s.database_name, ''), ISNULL(s.shard_map_name, ''),
       ISNULL(s.connection_options, ''),
       ISNULL(s.pushdown, '')
FROM   sys.external_data_sources s
