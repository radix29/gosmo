-- major 13
SELECT p.plan_id,
       p.query_id,
       p.is_forced_plan,
       CAST('' AS nvarchar(60)),
       p.force_failure_count,
       COALESCE(p.last_force_failure_reason_desc, ''),
       p.compatibility_level,
       p.is_trivial_plan,
       p.is_parallel_plan,
       p.last_compile_start_time,
       p.last_execution_time,
       COALESCE(p.query_plan, ''),
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       CAST(0 AS float) AS value
FROM   sys.query_store_plan AS p
LEFT JOIN sys.query_store_runtime_stats AS rs
       ON rs.plan_id = p.plan_id
LEFT JOIN sys.query_store_runtime_stats_interval AS rsi
       ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id
      AND rsi.start_time >= @p1 AND rsi.start_time < @p2
WHERE  p.query_id = @p3
GROUP BY p.plan_id, p.query_id, p.is_forced_plan,
         p.force_failure_count, p.last_force_failure_reason_desc,
         p.compatibility_level, p.is_trivial_plan, p.is_parallel_plan,
         p.last_compile_start_time, p.last_execution_time, p.query_plan
ORDER BY p.plan_id
-- major 14
SELECT p.plan_id,
       p.query_id,
       p.is_forced_plan,
       COALESCE(p.plan_forcing_type_desc, ''),
       p.force_failure_count,
       COALESCE(p.last_force_failure_reason_desc, ''),
       p.compatibility_level,
       p.is_trivial_plan,
       p.is_parallel_plan,
       p.last_compile_start_time,
       p.last_execution_time,
       COALESCE(p.query_plan, ''),
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       CAST(0 AS float) AS value
FROM   sys.query_store_plan AS p
LEFT JOIN sys.query_store_runtime_stats AS rs
       ON rs.plan_id = p.plan_id
LEFT JOIN sys.query_store_runtime_stats_interval AS rsi
       ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id
      AND rsi.start_time >= @p1 AND rsi.start_time < @p2
WHERE  p.query_id = @p3
GROUP BY p.plan_id, p.query_id, p.is_forced_plan, p.plan_forcing_type_desc,
         p.force_failure_count, p.last_force_failure_reason_desc,
         p.compatibility_level, p.is_trivial_plan, p.is_parallel_plan,
         p.last_compile_start_time, p.last_execution_time, p.query_plan
ORDER BY p.plan_id
-- major 15
SELECT p.plan_id,
       p.query_id,
       p.is_forced_plan,
       COALESCE(p.plan_forcing_type_desc, ''),
       p.force_failure_count,
       COALESCE(p.last_force_failure_reason_desc, ''),
       p.compatibility_level,
       p.is_trivial_plan,
       p.is_parallel_plan,
       p.last_compile_start_time,
       p.last_execution_time,
       COALESCE(p.query_plan, ''),
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       CAST(0 AS float) AS value
FROM   sys.query_store_plan AS p
LEFT JOIN sys.query_store_runtime_stats AS rs
       ON rs.plan_id = p.plan_id
LEFT JOIN sys.query_store_runtime_stats_interval AS rsi
       ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id
      AND rsi.start_time >= @p1 AND rsi.start_time < @p2
WHERE  p.query_id = @p3
GROUP BY p.plan_id, p.query_id, p.is_forced_plan, p.plan_forcing_type_desc,
         p.force_failure_count, p.last_force_failure_reason_desc,
         p.compatibility_level, p.is_trivial_plan, p.is_parallel_plan,
         p.last_compile_start_time, p.last_execution_time, p.query_plan
ORDER BY p.plan_id
-- major 16
SELECT p.plan_id,
       p.query_id,
       p.is_forced_plan,
       COALESCE(p.plan_forcing_type_desc, ''),
       p.force_failure_count,
       COALESCE(p.last_force_failure_reason_desc, ''),
       p.compatibility_level,
       p.is_trivial_plan,
       p.is_parallel_plan,
       p.last_compile_start_time,
       p.last_execution_time,
       COALESCE(p.query_plan, ''),
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       CAST(0 AS float) AS value
FROM   sys.query_store_plan AS p
LEFT JOIN sys.query_store_runtime_stats AS rs
       ON rs.plan_id = p.plan_id
LEFT JOIN sys.query_store_runtime_stats_interval AS rsi
       ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id
      AND rsi.start_time >= @p1 AND rsi.start_time < @p2
WHERE  p.query_id = @p3
GROUP BY p.plan_id, p.query_id, p.is_forced_plan, p.plan_forcing_type_desc,
         p.force_failure_count, p.last_force_failure_reason_desc,
         p.compatibility_level, p.is_trivial_plan, p.is_parallel_plan,
         p.last_compile_start_time, p.last_execution_time, p.query_plan
ORDER BY p.plan_id
-- major 17
SELECT p.plan_id,
       p.query_id,
       p.is_forced_plan,
       COALESCE(p.plan_forcing_type_desc, ''),
       p.force_failure_count,
       COALESCE(p.last_force_failure_reason_desc, ''),
       p.compatibility_level,
       p.is_trivial_plan,
       p.is_parallel_plan,
       p.last_compile_start_time,
       p.last_execution_time,
       COALESCE(p.query_plan, ''),
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       CAST(0 AS float) AS value
FROM   sys.query_store_plan AS p
LEFT JOIN sys.query_store_runtime_stats AS rs
       ON rs.plan_id = p.plan_id
LEFT JOIN sys.query_store_runtime_stats_interval AS rsi
       ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id
      AND rsi.start_time >= @p1 AND rsi.start_time < @p2
WHERE  p.query_id = @p3
GROUP BY p.plan_id, p.query_id, p.is_forced_plan, p.plan_forcing_type_desc,
         p.force_failure_count, p.last_force_failure_reason_desc,
         p.compatibility_level, p.is_trivial_plan, p.is_parallel_plan,
         p.last_compile_start_time, p.last_execution_time, p.query_plan
ORDER BY p.plan_id
