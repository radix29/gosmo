-- major 13

	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       CAST(0 AS bit)
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name
-- major 14

	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       CAST(0 AS bit)
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name
-- major 15

	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       ISNULL(l.is_distributed_network_name, 0)
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name
-- major 16

	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       ISNULL(l.is_distributed_network_name, 0)
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name
-- major 17

	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       ISNULL(l.is_distributed_network_name, 0)
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name
