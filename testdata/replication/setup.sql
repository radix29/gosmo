-- Replication fixture for the livedb replication tests (live_replication*_test.go).
--
-- Makes the instance its own distributor and publisher, then publishes from
-- and subscribes into throwaway gossms_p5_repl_* databases:
--
--   gossms_p5_repl_tran   transactional publication gossms_p5_tran_pub:
--                         dbo.Customer (row filter Region = N'EU'),
--                         dbo.OrderLine, dbo.GetCustomers (proc schema only)
--   gossms_p5_repl_merge  merge publication gossms_p5_merge_pub:
--                         dbo.Item (subset filter Category = N'A')
--   gossms_p5_repl_sub    a pull subscription to each publication
--
-- then runs both snapshot agents and both pull agents once, so every agent
-- type has history. Windows instances only (the snapshot folder is derived
-- from the instance's default data path). Needs sysadmin and SQL Server Agent
-- running; with xp_cmdshell on it makes its own snapshot folder the Agent
-- account can write (ReplData\gossms_p5_repl), which teardown.sql removes.
-- Takes about a minute.
--
-- The transactional publication uses @sync_method = 'native': 'concurrent'
-- runs SQLCLR that fails on win10cli ("LCID 8192 is not supported") because
-- the Agent account's Windows locale is a custom one.
--
-- Run once by hand, in sqlcmd (it uses GO; nothing else sqlcmd-specific):
--
--	sqlcmd -S host -U sa -P PASS -C -b -i testdata/replication/setup.sql
--
-- Undo with teardown.sql. It refuses to run over an existing distributor —
-- that is a server-level setting this script did not make.

SET NOCOUNT ON;
USE [master];
GO

-- sp_get_distributor's "installed" (its column count varies by version).
IF EXISTS (SELECT 1 FROM sys.servers WHERE is_distributor = 1)
    RAISERROR (N'A distributor is already configured on this instance; run teardown.sql first, or use another instance.', 16, 1);
IF EXISTS (SELECT 1 FROM sys.databases WHERE name LIKE N'gossms[_]p5[_]repl[_]%')
    RAISERROR (N'gossms_p5_repl_* databases exist already; run teardown.sql first.', 16, 1);
GO

-- ---------------------------------------------------------------------------
-- Distributor: this instance, distribution DB "distribution".
-- ---------------------------------------------------------------------------

DECLARE @server sysname = @@SERVERNAME;
EXEC sp_adddistributor @distributor = @server;
EXEC sp_adddistributiondb @database = N'distribution', @security_mode = 1;

DECLARE @data nvarchar(4000) = CONVERT(nvarchar(4000), SERVERPROPERTY('InstanceDefaultDataPath'));
-- ...\MSSQL\DATA\ -> ...\MSSQL\ReplData
DECLARE @repldata nvarchar(4000) =
    LEFT(@data, LEN(@data) - CHARINDEX(N'\', REVERSE(LEFT(@data, LEN(@data) - 1)))) + N'ReplData';

-- The snapshot agents run as the Agent service account, which a default
-- install does not let write to ReplData (Access to the path ... is denied).
-- With xp_cmdshell on, give the fixture its own snapshot folder the Agent
-- account may modify, which teardown.sql deletes; else use ReplData as is.
DECLARE @agent nvarchar(256) = (SELECT TOP (1) service_account FROM sys.dm_server_services
                                WHERE servicename LIKE N'SQL Server Agent%');
DECLARE @cmd nvarchar(4000);
IF (SELECT CONVERT(int, value_in_use) FROM sys.configurations WHERE name = N'xp_cmdshell') = 1
   AND @agent IS NOT NULL
BEGIN
    SET @repldata += N'\gossms_p5_repl';
    SET @cmd = N'mkdir "' + @repldata + N'"';
    EXEC xp_cmdshell @cmd, no_output;
    SET @cmd = N'icacls "' + @repldata + N'" /grant "' + @agent + N'":(OI)(CI)M';
    EXEC xp_cmdshell @cmd, no_output;
END
ELSE
    PRINT N'xp_cmdshell is off: snapshots go to ' + @repldata
        + N', which the Agent account may not be able to write.';

EXEC sp_adddistpublisher @publisher = @server, @distribution_db = N'distribution',
    @security_mode = 1, @working_directory = @repldata;
GO

-- ---------------------------------------------------------------------------
-- Databases and their objects.
-- ---------------------------------------------------------------------------

CREATE DATABASE [gossms_p5_repl_tran];
CREATE DATABASE [gossms_p5_repl_merge];
CREATE DATABASE [gossms_p5_repl_sub];
GO

USE [gossms_p5_repl_tran];
GO
CREATE TABLE dbo.Customer (
    Id     int           NOT NULL CONSTRAINT PK_Customer PRIMARY KEY,
    Name   nvarchar(100) NOT NULL,
    Region nvarchar(10)  NOT NULL
);
CREATE TABLE dbo.OrderLine (
    Id         int           NOT NULL CONSTRAINT PK_OrderLine PRIMARY KEY,
    CustomerId int           NOT NULL,
    Amount     decimal(10,2) NOT NULL
);
INSERT dbo.Customer VALUES (1, N'Ada', N'EU'), (2, N'Brian', N'US'), (3, N'Chen', N'EU');
INSERT dbo.OrderLine VALUES (1, 1, 10.50), (2, 2, 99.00), (3, 3, 7.25);
GO
CREATE PROCEDURE dbo.GetCustomers AS SELECT Id, Name, Region FROM dbo.Customer;
GO

USE [gossms_p5_repl_merge];
GO
CREATE TABLE dbo.Item (
    Id       int           NOT NULL CONSTRAINT PK_Item PRIMARY KEY,
    Name     nvarchar(100) NOT NULL,
    Category nvarchar(10)  NOT NULL
);
INSERT dbo.Item VALUES (1, N'Bolt', N'A'), (2, N'Nut', N'B'), (3, N'Washer', N'A');
GO

-- ---------------------------------------------------------------------------
-- Transactional publication.
-- ---------------------------------------------------------------------------

USE [master];
EXEC sp_replicationdboption @dbname = N'gossms_p5_repl_tran', @optname = N'publish', @value = N'true';
GO
USE [gossms_p5_repl_tran];
EXEC sp_addlogreader_agent @publisher_security_mode = 1;
EXEC sp_addpublication @publication = N'gossms_p5_tran_pub',
    @description = N'gossms fixture: transactional',
    @status = N'active', @repl_freq = N'continuous', @sync_method = N'native',
    @allow_push = N'true', @allow_pull = N'true', @allow_anonymous = N'false',
    @independent_agent = N'true', @immediate_sync = N'false', @retention = 0;
EXEC sp_addpublication_snapshot @publication = N'gossms_p5_tran_pub', @publisher_security_mode = 1;

EXEC sp_addarticle @publication = N'gossms_p5_tran_pub', @article = N'Customer',
    @source_owner = N'dbo', @source_object = N'Customer', @type = N'logbased',
    @destination_table = N'Customer', @destination_owner = N'dbo',
    @filter_clause = N'Region = N''EU''';
EXEC sp_articlefilter @publication = N'gossms_p5_tran_pub', @article = N'Customer',
    @filter_name = N'FLTR_Customer_EU', @filter_clause = N'Region = N''EU''';
EXEC sp_articleview @publication = N'gossms_p5_tran_pub', @article = N'Customer',
    @view_name = N'SYNC_Customer_EU', @filter_clause = N'Region = N''EU''';

EXEC sp_addarticle @publication = N'gossms_p5_tran_pub', @article = N'OrderLine',
    @source_owner = N'dbo', @source_object = N'OrderLine', @type = N'logbased',
    @destination_table = N'OrderLine', @destination_owner = N'dbo';

EXEC sp_addarticle @publication = N'gossms_p5_tran_pub', @article = N'GetCustomers',
    @source_owner = N'dbo', @source_object = N'GetCustomers', @type = N'proc schema only',
    @destination_table = N'GetCustomers', @destination_owner = N'dbo';
GO

-- ---------------------------------------------------------------------------
-- Merge publication.
-- ---------------------------------------------------------------------------

USE [master];
EXEC sp_replicationdboption @dbname = N'gossms_p5_repl_merge', @optname = N'merge publish', @value = N'true';
GO
USE [gossms_p5_repl_merge];
EXEC sp_addmergepublication @publication = N'gossms_p5_merge_pub',
    @description = N'gossms fixture: merge',
    @retention = 14, @sync_mode = N'native',
    @allow_push = N'true', @allow_pull = N'true', @allow_anonymous = N'false';
EXEC sp_addpublication_snapshot @publication = N'gossms_p5_merge_pub', @publisher_security_mode = 1;
EXEC sp_addmergearticle @publication = N'gossms_p5_merge_pub', @article = N'Item',
    @source_owner = N'dbo', @source_object = N'Item', @type = N'table',
    @subset_filterclause = N'Category = N''A''';
GO

-- ---------------------------------------------------------------------------
-- Pull subscriptions into gossms_p5_repl_sub: registered at the publisher,
-- created (with their agent jobs) at the subscriber.
-- ---------------------------------------------------------------------------

DECLARE @server sysname = @@SERVERNAME;

EXEC [gossms_p5_repl_tran].sys.sp_addsubscription @publication = N'gossms_p5_tran_pub',
    @subscriber = @server, @destination_db = N'gossms_p5_repl_sub',
    @subscription_type = N'pull', @sync_type = N'automatic', @article = N'all';
EXEC [gossms_p5_repl_merge].sys.sp_addmergesubscription @publication = N'gossms_p5_merge_pub',
    @subscriber = @server, @subscriber_db = N'gossms_p5_repl_sub',
    @subscription_type = N'pull', @subscriber_type = N'local', @sync_type = N'automatic';

EXEC [gossms_p5_repl_sub].sys.sp_addpullsubscription @publisher = @server,
    @publisher_db = N'gossms_p5_repl_tran', @publication = N'gossms_p5_tran_pub',
    @independent_agent = N'true', @subscription_type = N'pull';
EXEC [gossms_p5_repl_sub].sys.sp_addpullsubscription_agent @publisher = @server,
    @publisher_db = N'gossms_p5_repl_tran', @publication = N'gossms_p5_tran_pub',
    @distributor = @server, @distributor_security_mode = 1;

EXEC [gossms_p5_repl_sub].sys.sp_addmergepullsubscription @publisher = @server,
    @publisher_db = N'gossms_p5_repl_merge', @publication = N'gossms_p5_merge_pub',
    @subscriber_type = N'local';
EXEC [gossms_p5_repl_sub].sys.sp_addmergepullsubscription_agent @publisher = @server,
    @publisher_db = N'gossms_p5_repl_merge', @publication = N'gossms_p5_merge_pub',
    @distributor = @server, @distributor_security_mode = 1;
GO

-- ---------------------------------------------------------------------------
-- Snapshots, then one run of each pull agent, so every agent type has run
-- and the subscriber holds the published rows.
-- ---------------------------------------------------------------------------

EXEC [gossms_p5_repl_tran].sys.sp_startpublication_snapshot @publication = N'gossms_p5_tran_pub';
EXEC [gossms_p5_repl_merge].sys.sp_startpublication_snapshot @publication = N'gossms_p5_merge_pub';
GO

-- runstatus 2 = succeeded, 5 = retrying, 6 = failed (MSsnapshot_history).
DECLARE @wait int = 0, @done int, @failed nvarchar(4000);
WHILE @wait < 180
BEGIN
    SELECT @done = COUNT(*) FROM distribution.dbo.MSsnapshot_agents AS a
    WHERE EXISTS (SELECT 1 FROM distribution.dbo.MSsnapshot_history AS h
                  WHERE h.agent_id = a.id AND h.runstatus = 2);
    SELECT @failed = MIN(LEFT(h.comments, 2000)) FROM distribution.dbo.MSsnapshot_history AS h
    WHERE h.runstatus = 6;
    IF @done = 2 OR @failed IS NOT NULL BREAK;
    WAITFOR DELAY '00:00:02';
    SET @wait += 2;
END
IF @failed IS NOT NULL
    RAISERROR (N'A snapshot agent failed: %s', 16, 1, @failed);
ELSE IF @done < 2
    RAISERROR (N'The snapshot agents had not finished after 3 minutes; is SQL Server Agent running?', 16, 1);
GO

-- The pull agents' jobs are named for publisher, publication and subscriber.
DECLARE @job sysname;
DECLARE jobs CURSOR LOCAL FAST_FORWARD FOR
    SELECT name FROM msdb.dbo.sysjobs WHERE name LIKE N'%-gossms[_]p5[_]repl[_]sub-%';
OPEN jobs;
FETCH NEXT FROM jobs INTO @job;
WHILE @@FETCH_STATUS = 0
BEGIN
    EXEC msdb.dbo.sp_start_job @job_name = @job;
    FETCH NEXT FROM jobs INTO @job;
END
CLOSE jobs;
DEALLOCATE jobs;
GO

PRINT N'Replication fixture ready.';
