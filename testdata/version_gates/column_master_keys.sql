-- major 13

SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       CAST(0 AS bit),
       CAST(NULL AS varbinary(max))
FROM   sys.column_master_keys
-- major 14

SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       CAST(0 AS bit),
       CAST(NULL AS varbinary(max))
FROM   sys.column_master_keys
-- major 15

SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       allow_enclave_computations,
       signature
FROM   sys.column_master_keys
-- major 16

SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       allow_enclave_computations,
       signature
FROM   sys.column_master_keys
-- major 17

SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       allow_enclave_computations,
       signature
FROM   sys.column_master_keys
