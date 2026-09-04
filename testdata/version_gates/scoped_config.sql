-- major 13

SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)),
       CAST(0 AS bit)
FROM   sys.database_scoped_configurations
ORDER  BY name
-- major 14

SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)),
       is_value_default
FROM   sys.database_scoped_configurations
ORDER  BY name
-- major 15

SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)),
       is_value_default
FROM   sys.database_scoped_configurations
ORDER  BY name
-- major 16

SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)),
       is_value_default
FROM   sys.database_scoped_configurations
ORDER  BY name
-- major 17

SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)),
       is_value_default
FROM   sys.database_scoped_configurations
ORDER  BY name
