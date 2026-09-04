-- major 13

SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days,
       CAST('' AS nvarchar(60)),
       CAST(NULL AS int),
       CAST(NULL AS bigint),
       CAST(NULL AS bigint),
       CAST(NULL AS int)
FROM   sys.database_query_store_options
-- major 14

SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days,
       wait_stats_capture_mode_desc,
       CAST(NULL AS int),
       CAST(NULL AS bigint),
       CAST(NULL AS bigint),
       CAST(NULL AS int)
FROM   sys.database_query_store_options
-- major 15

SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days,
       wait_stats_capture_mode_desc,
       capture_policy_execution_count,
       capture_policy_total_compile_cpu_time_ms,
       capture_policy_total_execution_cpu_time_ms,
       capture_policy_stale_threshold_hours
FROM   sys.database_query_store_options
-- major 16

SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days,
       wait_stats_capture_mode_desc,
       capture_policy_execution_count,
       capture_policy_total_compile_cpu_time_ms,
       capture_policy_total_execution_cpu_time_ms,
       capture_policy_stale_threshold_hours
FROM   sys.database_query_store_options
-- major 17

SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days,
       wait_stats_capture_mode_desc,
       capture_policy_execution_count,
       capture_policy_total_compile_cpu_time_ms,
       capture_policy_total_execution_cpu_time_ms,
       capture_policy_stale_threshold_hours
FROM   sys.database_query_store_options
