-- major 13

SELECT s.event_session_id, s.name, s.startup_state,
       CAST(CASE WHEN r.name IS NULL THEN 0 ELSE 1 END AS bit),
       s.event_retention_mode_desc, s.max_dispatch_latency, s.max_memory,
       s.max_event_size, s.memory_partition_mode_desc, s.track_causality,
       CAST(0 AS bigint), ISNULL(r.dropped_event_count, 0)
FROM   sys.server_event_sessions s
LEFT   JOIN sys.dm_xe_sessions r ON r.name = s.name
-- major 14

SELECT s.event_session_id, s.name, s.startup_state,
       CAST(CASE WHEN r.name IS NULL THEN 0 ELSE 1 END AS bit),
       s.event_retention_mode_desc, s.max_dispatch_latency, s.max_memory,
       s.max_event_size, s.memory_partition_mode_desc, s.track_causality,
       CAST(0 AS bigint), ISNULL(r.dropped_event_count, 0)
FROM   sys.server_event_sessions s
LEFT   JOIN sys.dm_xe_sessions r ON r.name = s.name
-- major 15

SELECT s.event_session_id, s.name, s.startup_state,
       CAST(CASE WHEN r.name IS NULL THEN 0 ELSE 1 END AS bit),
       s.event_retention_mode_desc, s.max_dispatch_latency, s.max_memory,
       s.max_event_size, s.memory_partition_mode_desc, s.track_causality,
       CAST(0 AS bigint), ISNULL(r.dropped_event_count, 0)
FROM   sys.server_event_sessions s
LEFT   JOIN sys.dm_xe_sessions r ON r.name = s.name
-- major 16

SELECT s.event_session_id, s.name, s.startup_state,
       CAST(CASE WHEN r.name IS NULL THEN 0 ELSE 1 END AS bit),
       s.event_retention_mode_desc, s.max_dispatch_latency, s.max_memory,
       s.max_event_size, s.memory_partition_mode_desc, s.track_causality,
       CAST(0 AS bigint), ISNULL(r.dropped_event_count, 0)
FROM   sys.server_event_sessions s
LEFT   JOIN sys.dm_xe_sessions r ON r.name = s.name
-- major 17

SELECT s.event_session_id, s.name, s.startup_state,
       CAST(CASE WHEN r.name IS NULL THEN 0 ELSE 1 END AS bit),
       s.event_retention_mode_desc, s.max_dispatch_latency, s.max_memory,
       s.max_event_size, s.memory_partition_mode_desc, s.track_causality,
       s.max_duration, ISNULL(r.dropped_event_count, 0)
FROM   sys.server_event_sessions s
LEFT   JOIN sys.dm_xe_sessions r ON r.name = s.name
