-- Undoes setup.sql: drops the subscriptions, publications and
-- gossms_p5_repl_* databases, then the distributor — the latter only when the
-- distribution DB is left with no other publisher's publications, so it never
-- removes replication someone else configured.
--
--	sqlcmd -S host -U sa -P PASS -C -i testdata/replication/teardown.sql
--
-- Safe to re-run, and after a setup.sql that failed part-way: every step
-- checks for what it removes. Run without -b, so one failed step does not
-- stop the rest.

SET NOCOUNT ON;
USE [master];
GO

-- Stop the agents first: a running Log Reader holds the publication DB's log
-- (sp_replcmds), and every drop below then fails Msg 18752.
DECLARE @job sysname, @spid int, @sql nvarchar(200), @wait int = 0;
DECLARE jobs CURSOR LOCAL FAST_FORWARD FOR
    SELECT j.name FROM msdb.dbo.sysjobs AS j
    JOIN msdb.dbo.sysjobactivity AS a ON a.job_id = j.job_id
    WHERE a.session_id = (SELECT MAX(session_id) FROM msdb.dbo.syssessions)
      AND a.start_execution_date IS NOT NULL AND a.stop_execution_date IS NULL
      AND j.name LIKE N'%gossms[_]p5[_]repl[_]%';
OPEN jobs;
FETCH NEXT FROM jobs INTO @job;
WHILE @@FETCH_STATUS = 0
BEGIN
    EXEC msdb.dbo.sp_stop_job @job_name = @job;
    FETCH NEXT FROM jobs INTO @job;
END
CLOSE jobs;
DEALLOCATE jobs;

-- An agent's session can outlive its job step for a moment; end what is left.
WHILE @wait < 30
BEGIN
    SET @spid = NULL;
    SELECT TOP (1) @spid = session_id FROM sys.dm_exec_sessions
    WHERE is_user_process = 1 AND session_id <> @@SPID
      AND DB_NAME(database_id) LIKE N'gossms[_]p5[_]repl[_]%';
    IF @spid IS NULL BREAK;
    SET @sql = N'KILL ' + CONVERT(nvarchar(11), @spid);
    BEGIN TRY EXEC (@sql); END TRY BEGIN CATCH END CATCH;
    WAITFOR DELAY '00:00:01';
    SET @wait += 1;
END
GO

DECLARE @server sysname = @@SERVERNAME;

-- Subscriber side: the pull subscriptions and their agent jobs.
IF DB_ID(N'gossms_p5_repl_sub') IS NOT NULL
BEGIN
    IF OBJECT_ID(N'gossms_p5_repl_sub.dbo.MSreplication_subscriptions') IS NOT NULL
        EXEC [gossms_p5_repl_sub].sys.sp_droppullsubscription @publisher = @server,
            @publisher_db = N'gossms_p5_repl_tran', @publication = N'gossms_p5_tran_pub';
    IF OBJECT_ID(N'gossms_p5_repl_sub.dbo.sysmergesubscriptions') IS NOT NULL
        EXEC [gossms_p5_repl_sub].sys.sp_dropmergepullsubscription @publisher = @server,
            @publisher_db = N'gossms_p5_repl_merge', @publication = N'gossms_p5_merge_pub';
END
GO

DECLARE @server sysname = @@SERVERNAME;

-- Publisher side: subscriptions, then publications, then the publish options.
IF OBJECT_ID(N'gossms_p5_repl_tran.dbo.syspublications') IS NOT NULL
BEGIN
    EXEC [gossms_p5_repl_tran].sys.sp_dropsubscription @publication = N'all',
        @article = N'all', @subscriber = N'all';
    EXEC [gossms_p5_repl_tran].sys.sp_droppublication @publication = N'all';
END
IF DB_ID(N'gossms_p5_repl_tran') IS NOT NULL
    EXEC sp_replicationdboption @dbname = N'gossms_p5_repl_tran', @optname = N'publish', @value = N'false';

IF OBJECT_ID(N'gossms_p5_repl_merge.dbo.sysmergepublications') IS NOT NULL
BEGIN
    EXEC [gossms_p5_repl_merge].sys.sp_dropmergesubscription @publication = N'gossms_p5_merge_pub',
        @subscriber = N'all', @subscriber_db = N'all';
    EXEC [gossms_p5_repl_merge].sys.sp_dropmergepublication @publication = N'all';
END
IF DB_ID(N'gossms_p5_repl_merge') IS NOT NULL
    EXEC sp_replicationdboption @dbname = N'gossms_p5_repl_merge', @optname = N'merge publish', @value = N'false';
GO

-- Whatever the drops above missed (a half-built setup), then the databases.
DECLARE @db sysname, @sql nvarchar(max);
DECLARE dbs CURSOR LOCAL FAST_FORWARD FOR
    SELECT name FROM sys.databases WHERE name LIKE N'gossms[_]p5[_]repl[_]%';
OPEN dbs;
FETCH NEXT FROM dbs INTO @db;
WHILE @@FETCH_STATUS = 0
BEGIN
    EXEC sp_removedbreplication @dbname = @db, @type = N'both';
    SET @sql = N'ALTER DATABASE ' + QUOTENAME(@db) + N' SET SINGLE_USER WITH ROLLBACK IMMEDIATE; '
             + N'DROP DATABASE ' + QUOTENAME(@db) + N';';
    EXEC (@sql);
    FETCH NEXT FROM dbs INTO @db;
END
CLOSE dbs;
DEALLOCATE dbs;
GO

-- The distributor, unless another publisher still uses it.
DECLARE @server sysname = @@SERVERNAME, @others int = 0;
-- sp_get_distributor's "installed" (its column count varies by version).
DECLARE @installed int = CASE WHEN EXISTS (SELECT 1 FROM sys.servers WHERE is_distributor = 1) THEN 1 ELSE 0 END;

IF @installed = 1 AND DB_ID(N'distribution') IS NOT NULL
    SELECT @others = COUNT(*) FROM distribution.dbo.MSpublications
    WHERE publisher_db NOT LIKE N'gossms[_]p5[_]repl[_]%';

IF @installed = 1 AND @others > 0
    PRINT N'Distributor kept: the distribution DB holds other publishers'' publications.';
ELSE IF @installed = 1
BEGIN
    -- @no_checks = 1 drops the distribution publisher and DB too;
    -- sp_dropdistpublisher on its own refuses (Msg 21047) while the
    -- instance is still registered as its own subscriber.
    EXEC sp_dropdistributor @no_checks = 1;
END
GO

-- setup.sql's own snapshot folder, when it made one (xp_cmdshell on).
IF (SELECT CONVERT(int, value_in_use) FROM sys.configurations WHERE name = N'xp_cmdshell') = 1
BEGIN
    DECLARE @data nvarchar(4000) = CONVERT(nvarchar(4000), SERVERPROPERTY('InstanceDefaultDataPath'));
    DECLARE @cmd nvarchar(4000) = N'if exist "'
        + LEFT(@data, LEN(@data) - CHARINDEX(N'\', REVERSE(LEFT(@data, LEN(@data) - 1))))
        + N'ReplData\gossms_p5_repl" rmdir /s /q "'
        + LEFT(@data, LEN(@data) - CHARINDEX(N'\', REVERSE(LEFT(@data, LEN(@data) - 1))))
        + N'ReplData\gossms_p5_repl"';
    EXEC xp_cmdshell @cmd, no_output;
END
GO

-- Agent jobs the procs above should have removed; listed, not deleted, so a
-- leftover is noticed rather than silently cleaned up.
SELECT name AS leftover_job FROM msdb.dbo.sysjobs
WHERE name LIKE N'%gossms[_]p5[_]%' OR name LIKE N'%distribution%'
   OR name LIKE N'Replication%' OR name LIKE N'%subscription%';
GO

PRINT N'Replication fixture removed.';
