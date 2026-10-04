-- major 13
CASE WHEN CAST(NULL AS nvarchar(60)) NOT IN ('DATABASE_DEFAULT', 'NOT_APPLICABLE') THEN CAST(NULL AS nvarchar(60))
	            WHEN containment = 1 THEN N'Latin1_General_100_CI_AS_KS_WS_SC'
	            ELSE collation_name END
-- major 14
CASE WHEN CAST(NULL AS nvarchar(60)) NOT IN ('DATABASE_DEFAULT', 'NOT_APPLICABLE') THEN CAST(NULL AS nvarchar(60))
	            WHEN containment = 1 THEN N'Latin1_General_100_CI_AS_KS_WS_SC'
	            ELSE collation_name END
-- major 15
CASE WHEN catalog_collation_type_desc NOT IN ('DATABASE_DEFAULT', 'NOT_APPLICABLE') THEN catalog_collation_type_desc
	            WHEN containment = 1 THEN N'Latin1_General_100_CI_AS_KS_WS_SC'
	            ELSE collation_name END
-- major 16
CASE WHEN catalog_collation_type_desc NOT IN ('DATABASE_DEFAULT', 'NOT_APPLICABLE') THEN catalog_collation_type_desc
	            WHEN containment = 1 THEN N'Latin1_General_100_CI_AS_KS_WS_SC'
	            ELSE collation_name END
-- major 17
CASE WHEN catalog_collation_type_desc NOT IN ('DATABASE_DEFAULT', 'NOT_APPLICABLE') THEN catalog_collation_type_desc
	            WHEN containment = 1 THEN N'Latin1_General_100_CI_AS_KS_WS_SC'
	            ELSE collation_name END
