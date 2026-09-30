-- major 13

SELECT g.group_id, g.name, g.pool_id, p.name, g.external_pool_id, ISNULL(ep.name, N''),
       g.importance,
       CAST(g.request_max_memory_grant_percent AS float),
       g.request_max_cpu_time_sec, g.request_memory_grant_timeout_sec,
       g.max_dop, g.group_max_requests,
       CAST(NULL AS float),
       CAST(NULL AS float)
FROM   sys.resource_governor_workload_groups g
JOIN   sys.resource_governor_resource_pools p ON p.pool_id = g.pool_id
LEFT   JOIN sys.resource_governor_external_resource_pools ep
       ON ep.external_pool_id = g.external_pool_id
-- major 14

SELECT g.group_id, g.name, g.pool_id, p.name, g.external_pool_id, ISNULL(ep.name, N''),
       g.importance,
       CAST(g.request_max_memory_grant_percent AS float),
       g.request_max_cpu_time_sec, g.request_memory_grant_timeout_sec,
       g.max_dop, g.group_max_requests,
       CAST(NULL AS float),
       CAST(NULL AS float)
FROM   sys.resource_governor_workload_groups g
JOIN   sys.resource_governor_resource_pools p ON p.pool_id = g.pool_id
LEFT   JOIN sys.resource_governor_external_resource_pools ep
       ON ep.external_pool_id = g.external_pool_id
-- major 15

SELECT g.group_id, g.name, g.pool_id, p.name, g.external_pool_id, ISNULL(ep.name, N''),
       g.importance,
       g.request_max_memory_grant_percent_numeric,
       g.request_max_cpu_time_sec, g.request_memory_grant_timeout_sec,
       g.max_dop, g.group_max_requests,
       CAST(NULL AS float),
       CAST(NULL AS float)
FROM   sys.resource_governor_workload_groups g
JOIN   sys.resource_governor_resource_pools p ON p.pool_id = g.pool_id
LEFT   JOIN sys.resource_governor_external_resource_pools ep
       ON ep.external_pool_id = g.external_pool_id
-- major 16

SELECT g.group_id, g.name, g.pool_id, p.name, g.external_pool_id, ISNULL(ep.name, N''),
       g.importance,
       g.request_max_memory_grant_percent_numeric,
       g.request_max_cpu_time_sec, g.request_memory_grant_timeout_sec,
       g.max_dop, g.group_max_requests,
       CAST(NULL AS float),
       CAST(NULL AS float)
FROM   sys.resource_governor_workload_groups g
JOIN   sys.resource_governor_resource_pools p ON p.pool_id = g.pool_id
LEFT   JOIN sys.resource_governor_external_resource_pools ep
       ON ep.external_pool_id = g.external_pool_id
-- major 17

SELECT g.group_id, g.name, g.pool_id, p.name, g.external_pool_id, ISNULL(ep.name, N''),
       g.importance,
       g.request_max_memory_grant_percent_numeric,
       g.request_max_cpu_time_sec, g.request_memory_grant_timeout_sec,
       g.max_dop, g.group_max_requests,
       g.group_max_tempdb_data_percent,
       g.group_max_tempdb_data_mb
FROM   sys.resource_governor_workload_groups g
JOIN   sys.resource_governor_resource_pools p ON p.pool_id = g.pool_id
LEFT   JOIN sys.resource_governor_external_resource_pools ep
       ON ep.external_pool_id = g.external_pool_id
