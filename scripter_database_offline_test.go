package gosmo_test

import (
	"context"
	"testing"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// The database-scoped scripters that reached no offline test: the DML
// templates, external objects, full-text, CLR type, assembly, plan guide,
// roles, row-level security, Always Encrypted keys and the database's whole
// security script. Each read is keyed on its text and arguments and the
// whole script compared; see scripter_script_as_test.go for why.

// columnRow is a columnSelect row: the arguments are the columns that vary
// here, every later one its plain-column zero.
func columnRow(name string, id int64, typ string, maxLen, prec, scale int64, nullable, identity, computed bool) []any {
	return []any{name, id, typ, maxLen, prec, scale, nullable, identity, computed,
		"", "", "", false, "", nil, nil, false, "sys", false, false, false, false, false, "",
		int64(0), false, false, int64(0), false, "", "", "", "", "", false, int64(0), "", "", "", "", "", false}
}

// ordersColumns serves [sales].[Orders]'s columns, looked up by name: an
// identity, a plain int, an nvarchar(100) (200 bytes) and a computed money —
// the identity and computed ones are what INSERT and UPDATE leave out.
func ordersColumns(f *gosmotest.Fake) {
	f.OnFunc(read("WHERE  c.object_id = OBJECT_ID(@p1)", "[sales].[Orders]"), gosmotest.Rows(
		columnRow("OrderID", 1, "int", 4, 10, 0, false, true, false),
		columnRow("CustomerID", 2, "int", 4, 10, 0, false, false, false),
		columnRow("Note", 3, "nvarchar", 200, 0, 0, true, false, false),
		columnRow("Total", 4, "money", 8, 19, 4, true, false, true)))
}

// paramRow is a parameterQuery row of a built-in-typed parameter.
func paramRow(name string, id int64, typ string, maxLen, prec, scale int64, output, hasDefault bool) []any {
	return []any{name, id, typ, maxLen, prec, scale, output, hasDefault, "sys", false, "", "", false, int64(0), "", false}
}

// routineParams serves sales.<routine>'s parameters, looked up by schema
// and name: an input, a datetime2(7) and an OUTPUT.
func routineParams(f *gosmotest.Fake, routine string) {
	f.OnFunc(read("FROM   sys.parameters p", "sales", routine), gosmotest.Rows(
		paramRow("@CustomerID", 1, "int", 4, 10, 0, false, false),
		paramRow("@Since", 2, "datetime2", 8, 27, 7, false, true),
		paramRow("@Count", 3, "int", 4, 10, 0, true, false)))
}

func TestDMLScriptersGolden(t *testing.T) {
	runScriptCases(t, []scriptCase{{
		name:  "ScriptSelect",
		rules: ordersColumns,
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptSelect(ctx, "sales", "Orders")
		},
		want: "SELECT [OrderID]\n" +
			"     , [CustomerID]\n" +
			"     , [Note]\n" +
			"     , [Total]\n" +
			"FROM   [sales].[Orders];\n" +
			"GO\n",
	}, {
		name:  "ScriptInsert",
		rules: ordersColumns,
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptInsert(ctx, "sales", "Orders")
		},
		want: "INSERT INTO [sales].[Orders]\n" +
			"           ([CustomerID]\n" +
			"          , [Note])\n" +
			"VALUES     (<CustomerID, int,>\n" +
			"          , <Note, nvarchar(100),>);\n" +
			"GO\n",
	}, {
		name:  "ScriptUpdate",
		rules: ordersColumns,
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptUpdate(ctx, "sales", "Orders")
		},
		want: "UPDATE [sales].[Orders]\n" +
			"SET    [CustomerID] = <CustomerID, int,>\n" +
			"     , [Note] = <Note, nvarchar(100),>\n" +
			"WHERE  <Search Conditions,,>;\n" +
			"GO\n",
	}, {
		// DELETE needs no columns: it reads nothing.
		name:  "ScriptDelete",
		rules: func(*gosmotest.Fake) {},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptDelete(ctx, "sales", "Orders")
		},
		want: "DELETE FROM [sales].[Orders]\n" +
			"WHERE  <Search Conditions,,>;\n" +
			"GO\n",
	}, {
		name:  "ScriptExecute",
		rules: func(f *gosmotest.Fake) { routineParams(f, "GetOrders") },
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptExecute(ctx, "sales", "GetOrders")
		},
		want: "DECLARE @return_value int;\n" +
			"DECLARE @Count int;\n" +
			"\n" +
			"EXEC @return_value = [sales].[GetOrders]\n" +
			"     @CustomerID = <CustomerID, int,>,\n" +
			"     @Since = <Since, datetime2(7),>,\n" +
			"     @Count = @Count OUTPUT;\n" +
			"\n" +
			"SELECT @Count AS N'@Count';\n" +
			"SELECT 'Return Value' = @return_value;\n" +
			"GO\n",
	}, {
		name:  "ScriptFunctionCall",
		rules: func(f *gosmotest.Fake) { routineParams(f, "OrdersOf") },
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptFunctionCall(ctx, "sales", "OrdersOf", gosmo.FunctionType("TF"))
		},
		want: "SELECT *\n" +
			"FROM   [sales].[OrdersOf](<CustomerID, int,>, <Since, datetime2(7),>, <Count, int,>);\n" +
			"GO\n",
	}})
}

func TestExternalAndFullTextScriptersGolden(t *testing.T) {
	runScriptCases(t, []scriptCase{{
		name: "ScriptExternalDataSource",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.external_data_sources s", "lake"), gosmotest.Rows(
				[]any{"lake", int64(65536), "abs://data@acct.blob.core.windows.net", "BLOB_STORAGE", "", "lake_cred", "", "", "", ""}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptExternalDataSource(ctx, "lake")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.external_data_sources WHERE name = N'lake')\n" +
			"CREATE EXTERNAL DATA SOURCE [lake] WITH (\n" +
			"    LOCATION = N'abs://data@acct.blob.core.windows.net',\n" +
			"    TYPE = BLOB_STORAGE,\n" +
			"    CREDENTIAL = [lake_cred]\n" +
			");\n" +
			"GO\n",
	}, {
		name: "ScriptExternalFileFormat",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.external_file_formats f", "csv"), gosmotest.Rows(
				[]any{int64(1), "csv", "DELIMITEDTEXT", ",", `"`, "", false, "", "", "UTF8", "", int64(2), "2.0"}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptExternalFileFormat(ctx, "csv")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.external_file_formats WHERE name = N'csv')\n" +
			"CREATE EXTERNAL FILE FORMAT [csv] WITH (\n" +
			"    FORMAT_TYPE = DELIMITEDTEXT,\n" +
			"    FORMAT_OPTIONS (FIELD_TERMINATOR = N',', STRING_DELIMITER = N'\"', FIRST_ROW = 2, ENCODING = N'UTF8', PARSER_VERSION = N'2.0')\n" +
			");\n" +
			"GO\n",
	}, {
		name: "ScriptExternalLibrary",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.external_libraries l", "pkgx"), gosmotest.Rows(
				[]any{int64(1), "pkgx", "dbo", "Python", "PUBLIC"}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptExternalLibrary(ctx, "pkgx")
		},
		want: "/* The library's package content cannot be scripted — it is not text.\n" +
			"   Replace the placeholder below with the package bytes or its path. */\n" +
			"/* Scope: PUBLIC — set by ownership and package rights, not by this statement. */\n" +
			"CREATE EXTERNAL LIBRARY [pkgx] AUTHORIZATION [dbo]\n" +
			"FROM (CONTENT = 0x00 /* <replace with the package bytes, or a path literal> */)\n" +
			"WITH (LANGUAGE = N'Python');\n" +
			"GO\n",
	}, {
		// The catalog is picked from the whole list by name.
		name: "ScriptFullTextCatalog",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.fulltext_catalogs c\nORDER"), gosmotest.Rows(
				[]any{int64(4), "ft_archive", false, true, "dbo", false, int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)},
				[]any{int64(5), "ft_main", true, false, "dbo", false, int64(1), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptFullTextCatalog(ctx, "ft_main")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.fulltext_catalogs WHERE name = N'ft_main')\n" +
			"CREATE FULLTEXT CATALOG [ft_main] WITH ACCENT_SENSITIVITY = OFF AS DEFAULT AUTHORIZATION [dbo]\n" +
			"GO\n",
	}, {
		// The stopwords are read by the stoplist's id.
		name: "ScriptFullTextStoplist",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.fulltext_stoplists\n"), gosmotest.Rows([]any{int64(6), "sl_app", "dbo", scriptWhen, scriptWhen}))
			f.OnFunc(read("FROM   sys.fulltext_stopwords", int64(6)), gosmotest.Rows(
				[]any{"acme", "English", int64(1033)}, []any{"gmbh", "German", int64(1031)}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptFullTextStoplist(ctx, "sl_app")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.fulltext_stoplists WHERE name = N'sl_app')\n" +
			"CREATE FULLTEXT STOPLIST [sl_app] AUTHORIZATION [dbo];\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM sys.fulltext_stopwords w JOIN sys.fulltext_stoplists l ON l.stoplist_id = w.stoplist_id\n" +
			"               WHERE l.name = N'sl_app' AND w.stopword = N'acme' AND w.language_id = 1033)\n" +
			"ALTER FULLTEXT STOPLIST [sl_app] ADD N'acme' LANGUAGE 1033;\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM sys.fulltext_stopwords w JOIN sys.fulltext_stoplists l ON l.stoplist_id = w.stoplist_id\n" +
			"               WHERE l.name = N'sl_app' AND w.stopword = N'gmbh' AND w.language_id = 1031)\n" +
			"ALTER FULLTEXT STOPLIST [sl_app] ADD N'gmbh' LANGUAGE 1031;\n" +
			"GO\n",
	}, {
		name: "ScriptSearchPropertyList",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.registered_search_property_lists\n"), gosmotest.Rows(
				[]any{int64(7), "spl_docs", "dbo", scriptWhen, scriptWhen}))
			f.OnFunc(read("FROM   sys.registered_search_properties", int64(7)), gosmotest.Rows(
				[]any{"Title", "F29F85E0-4FF9-1068-AB91-08002B27B3D9", int64(2), "Doc title", int64(1)}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptSearchPropertyList(ctx, "spl_docs")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.registered_search_property_lists WHERE name = N'spl_docs')\n" +
			"CREATE SEARCH PROPERTY LIST [spl_docs] AUTHORIZATION [dbo];\n" +
			"GO\n" +
			"IF NOT EXISTS (SELECT 1 FROM sys.registered_search_properties sp JOIN sys.registered_search_property_lists l " +
			"ON l.property_list_id = sp.property_list_id\n" +
			"               WHERE l.name = N'spl_docs' AND sp.property_name = N'Title')\n" +
			"ALTER SEARCH PROPERTY LIST [spl_docs] ADD N'Title' WITH (PROPERTY_SET_GUID = N'F29F85E0-4FF9-1068-AB91-08002B27B3D9', " +
			"PROPERTY_INT_ID = 2, PROPERTY_DESCRIPTION = N'Doc title');\n" +
			"GO\n",
	}, {
		// A user stoplist (stoplist_id > 0) is named; the columns are a second
		// read by the index's object id.
		name: "ScriptFullTextIndex",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.fulltext_indexes fi", "[sales].[Orders]"), gosmotest.Rows(
				[]any{int64(901), "sales", "Orders", "PK_Orders", "ft_main", "", true,
					"MANUAL", int64(6), "sl_app", "spl_docs", int64(1), "", false, nil, nil,
					int64(0), int64(0), int64(0), int64(0), int64(0)}))
			f.OnFunc(read("FROM   sys.fulltext_index_columns ic", int64(901)), gosmotest.Rows(
				[]any{int64(901), "Note", "", int64(1033), "English", true}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptFullTextIndex(ctx, "sales", "Orders")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.fulltext_indexes WHERE object_id = OBJECT_ID(N'[sales].[Orders]'))\n" +
			"CREATE FULLTEXT INDEX ON [sales].[Orders] ([Note] LANGUAGE 1033 STATISTICAL_SEMANTICS) KEY INDEX [PK_Orders] " +
			"ON [ft_main] WITH (CHANGE_TRACKING = MANUAL, STOPLIST = [sl_app], SEARCH PROPERTY LIST = [spl_docs])\n" +
			"GO\n",
	}})
}

func TestProgrammabilityAndSecurityScriptersGolden(t *testing.T) {
	runScriptCases(t, []scriptCase{{
		// A type's extended properties are class 6, keyed by user_type_id.
		name: "ScriptClrType",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("WHERE  t.is_assembly_type = 1", "geo", "Point"), gosmotest.Rows(
				[]any{"Point", "geo", int64(260), int64(-1), int64(0), int64(0), true, "GeoAsm", "Geo.Point"}))
			noProps(f, 6, 260)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptClrType(ctx, "geo", "Point")
		},
		want: "/* The assembly named below must already be registered in the database. */\n" +
			"IF TYPE_ID(N'[geo].[Point]') IS NULL\n" +
			"CREATE TYPE [geo].[Point] EXTERNAL NAME [GeoAsm].[Geo.Point];\n" +
			"GO\n",
	}, {
		name: "ScriptAssembly",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.assemblies a", "GeoAsm"), gosmotest.Rows(
				[]any{"GeoAsm", int64(65536), "dbo", "geoasm, version=1.0.0.0", "SAFE_ACCESS", true, true, scriptWhen, scriptWhen}))
			noProps(f, 5, 65536)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptAssembly(ctx, "GeoAsm")
		},
		want: "/* The assembly binary cannot be scripted — it is not text. Replace the\n" +
			"   placeholder below with the assembly's bytes, or with a FROM '<path>' clause. */\n" +
			"CREATE ASSEMBLY [GeoAsm] AUTHORIZATION [dbo]\n" +
			"FROM 0x00 /* <replace with the assembly binary, or a FROM '<path>' clause> */\n" +
			"WITH PERMISSION_SET = SAFE;\n" +
			"GO\n",
	}, {
		// A disabled guide is created, then disabled.
		name: "ScriptPlanGuide",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.plan_guides g", "pg_orders"), gosmotest.Rows(
				[]any{int64(65540), "pg_orders", true, "SELECT * FROM sales.Orders WHERE CustomerID = @c", "OBJECT",
					"[sales].[GetOrders]", "sales", "GetOrders", "", "", "OPTION (MAXDOP 1)", scriptWhen, scriptWhen}))
			noProps(f, 27, 65540)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptPlanGuide(ctx, "pg_orders")
		},
		want: "EXEC sp_create_plan_guide\n" +
			"     @name = N'pg_orders',\n" +
			"     @stmt = N'SELECT * FROM sales.Orders WHERE CustomerID = @c',\n" +
			"     @type = N'OBJECT',\n" +
			"     @module_or_batch = N'[sales].[GetOrders]',\n" +
			"     @params = NULL,\n" +
			"     @hints = N'OPTION (MAXDOP 1)';\n" +
			"GO\n" +
			"\n" +
			"EXEC sp_control_plan_guide @operation = N'DISABLE', @name = N'pg_orders';\n" +
			"GO\n",
	}, {
		name: "ScriptDatabaseRole",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("WHERE  r.type = 'R' AND r.name = @p1", "app_readers"), gosmotest.Rows(
				[]any{"app_readers", int64(7), false, "dbo", []byte{9}, scriptWhen, scriptWhen, `[{"v":"alice"},{"v":"bob"}]`}))
			noProps(f, 4, 7)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptDatabaseRole(ctx, "app_readers")
		},
		want: "IF DATABASE_PRINCIPAL_ID(N'app_readers') IS NULL\n" +
			"CREATE ROLE [app_readers] AUTHORIZATION [dbo];\n" +
			"GO\n" +
			"ALTER ROLE [app_readers] ADD MEMBER [alice];\n" +
			"GO\n" +
			"ALTER ROLE [app_readers] ADD MEMBER [bob];\n" +
			"GO\n",
	}, {
		name: "ScriptApplicationRole",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("WHERE  type = 'A'\n   AND name = @p1", "reporting_app"), gosmotest.Rows(
				[]any{"reporting_app", "rpt", int64(9), scriptWhen, scriptWhen}))
			noProps(f, 4, 9)
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptApplicationRole(ctx, "reporting_app")
		},
		want: "IF DATABASE_PRINCIPAL_ID(N'reporting_app') IS NULL\n" +
			"CREATE APPLICATION ROLE [reporting_app] WITH PASSWORD = N'" + gosmo.PasswordPlaceholder +
			"', DEFAULT_SCHEMA = [rpt];  -- set the real password\n" +
			"GO\n",
	}, {
		// The predicates are a second read by the policy's object id; the
		// FILTER one has no operation.
		name: "ScriptSecurityPolicy",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.security_policies sp", "sec", "tenant_filter"), gosmotest.Rows(
				[]any{"tenant_filter", "sec", int64(1300), true, false, true}))
			f.OnFunc(read("FROM   sys.security_predicates spr", int64(1300)), gosmotest.Rows(
				[]any{int64(1300), "BLOCK", "([sec].[fn_tenant]([TenantID]))", "sales", "Orders", "AFTER INSERT"},
				[]any{int64(1300), "FILTER", "([sec].[fn_tenant]([TenantID]))", "sales", "Orders", nil}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptSecurityPolicy(ctx, "sec", "tenant_filter")
		},
		want: "IF OBJECT_ID(N'[sec].[tenant_filter]', N'SP') IS NULL\n" +
			"CREATE SECURITY POLICY [sec].[tenant_filter]\n" +
			"    ADD BLOCK PREDICATE [sec].[fn_tenant]([TenantID]) ON [sales].[Orders] AFTER INSERT,\n" +
			"    ADD FILTER PREDICATE [sec].[fn_tenant]([TenantID]) ON [sales].[Orders]\n" +
			"    WITH (STATE = ON, SCHEMABINDING = ON);\n" +
			"GO\n",
	}, {
		name: "ScriptColumnMasterKey",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.column_master_keys\n", "cmk1"), gosmotest.Rows(
				[]any{"cmk1", int64(1), "MSSQL_CERTIFICATE_STORE", "CurrentUser/My/ABC", true, []byte{0xDE, 0xAD}}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptColumnMasterKey(ctx, "cmk1")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.column_master_keys WHERE name = N'cmk1')\n" +
			"CREATE COLUMN MASTER KEY [cmk1]\n" +
			"WITH (\n" +
			"    KEY_STORE_PROVIDER_NAME = N'MSSQL_CERTIFICATE_STORE',\n" +
			"    KEY_PATH = N'CurrentUser/My/ABC',\n" +
			"    ENCLAVE_COMPUTATIONS (SIGNATURE = 0xDEAD)\n" +
			");\n" +
			"GO\n",
	}, {
		// One row per value (a key mid-rotation has two), grouped onto the key.
		name: "ScriptColumnEncryptionKey",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM   sys.column_encryption_keys cek", "cek1"), gosmotest.Rows(
				[]any{"cek1", int64(1), "cmk1", "RSA_OAEP", []byte{0x01, 0x70}},
				[]any{"cek1", int64(1), "cmk2", "RSA_OAEP", []byte{0x01, 0x71}}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptColumnEncryptionKey(ctx, "cek1")
		},
		want: "IF NOT EXISTS (SELECT 1 FROM sys.column_encryption_keys WHERE name = N'cek1')\n" +
			"CREATE COLUMN ENCRYPTION KEY [cek1]\n" +
			"WITH VALUES\n" +
			"(\n" +
			"    COLUMN_MASTER_KEY = [cmk1],\n" +
			"    ALGORITHM = 'RSA_OAEP',\n" +
			"    ENCRYPTED_VALUE = 0x0170\n" +
			"),\n" +
			"(\n" +
			"    COLUMN_MASTER_KEY = [cmk2],\n" +
			"    ALGORITHM = 'RSA_OAEP',\n" +
			"    ENCRYPTED_VALUE = 0x0171\n" +
			");\n" +
			"GO\n",
	}, {
		// The whole database's security from six list reads. dbo the user,
		// dbo the schema and db_datareader the fixed role are not created,
		// but the fixed role's membership is restored.
		name: "ScriptDatabaseSecurity",
		rules: func(f *gosmotest.Fake) {
			f.OnFunc(read("FROM sys.databases WHERE database_id = DB_ID()"), gosmotest.Rows([]any{"app_owner", []byte{0x01, 0x05}}))
			f.OnFunc(read("WHERE  r.type = 'R'\nORDER  BY r.name"), gosmotest.Rows(
				[]any{"app_readers", int64(7), false, "dbo", `[{"v":"app_user"}]`},
				[]any{"db_datareader", int64(16390), true, "dbo", `[{"v":"app_readers"}]`}))
			f.OnFunc(read("WHERE  type = 'A'\nORDER  BY name"), gosmotest.Rows([]any{"reporting_app", "rpt", int64(9), scriptWhen, scriptWhen}))
			f.OnFunc(read("ORDER  BY dp.name"), gosmotest.Rows(
				[]any{"app_user", int64(5), "SQL_USER", "sales", scriptWhen, scriptWhen, "INSTANCE", []byte{0xAB, 0x01}, "app_login", false, nil},
				[]any{"dbo", int64(1), "SQL_USER", "dbo", scriptWhen, scriptWhen, "INSTANCE", []byte{0x01}, "sa", false, nil}))
			f.OnFunc(read("FROM   sys.schemas s"), gosmotest.Rows([]any{"dbo", int64(1), "dbo"}, []any{"sales", int64(5), "app_readers"}))
			f.OnFunc(read("FROM   sys.database_permissions perm"), gosmotest.Rows(
				[]any{"app_readers", "G", "SELECT", int64(3), "SCHEMA", int64(5), nil, nil, "sales", nil},
				[]any{"app_user", "W", "EXECUTE", int64(1), "OBJECT_OR_COLUMN", int64(77), nil, "sales", "GetOrders", nil},
				[]any{"app_user", "D", "UPDATE", int64(1), "OBJECT_OR_COLUMN", int64(901), nil, "sales", "Orders", "Total"},
				[]any{"reporting_app", "G", "CONNECT", int64(0), "DATABASE", int64(0), nil, nil, nil, nil}))
		},
		run: func(ctx context.Context, srv *gosmo.Server) (string, error) {
			return app(srv).ScriptDatabaseSecurity(ctx)
		},
		want: "/* Security of database [app]. Every password below is a placeholder: the\n" +
			"   server does not expose them. Set the real one on each line marked\n" +
			"   \"set the real password\" before running this. */\n" +
			"\n" +
			"-- Database owner\n" +
			"DECLARE @db nvarchar(300) = QUOTENAME(DB_NAME());\n" +
			"EXEC (N'ALTER AUTHORIZATION ON DATABASE::' + @db + N' TO [app_owner];');\n" +
			"GO\n" +
			"\n" +
			"-- Database roles\n" +
			"IF DATABASE_PRINCIPAL_ID(N'app_readers') IS NULL\n" +
			"CREATE ROLE [app_readers] AUTHORIZATION [dbo];\n" +
			"GO\n" +
			"\n" +
			"-- Application roles\n" +
			"IF DATABASE_PRINCIPAL_ID(N'reporting_app') IS NULL\n" +
			"CREATE APPLICATION ROLE [reporting_app] WITH PASSWORD = N'" + gosmo.PasswordPlaceholder +
			"', DEFAULT_SCHEMA = [rpt];  -- set the real password\n" +
			"GO\n" +
			"\n" +
			"-- Database users\n" +
			"IF DATABASE_PRINCIPAL_ID(N'app_user') IS NULL\n" +
			"CREATE USER [app_user] FOR LOGIN [app_login] WITH DEFAULT_SCHEMA = [sales];\n" +
			"GO\n" +
			"\n" +
			"-- Schemas\n" +
			"IF SCHEMA_ID(N'sales') IS NULL\n" +
			"    EXEC(N'CREATE SCHEMA [sales] AUTHORIZATION [app_readers]');\n" +
			"GO\n" +
			"\n" +
			"-- Ownership\n" +
			"ALTER AUTHORIZATION ON SCHEMA::[sales] TO [app_readers];\n" +
			"GO\n" +
			"\n" +
			"-- Role memberships\n" +
			"ALTER ROLE [app_readers] ADD MEMBER [app_user];\n" +
			"ALTER ROLE [db_datareader] ADD MEMBER [app_readers];\n" +
			"GO\n" +
			"\n" +
			"-- Permissions\n" +
			"GRANT SELECT ON SCHEMA::[sales] TO [app_readers];\n" +
			"GRANT EXECUTE ON OBJECT::[sales].[GetOrders] TO [app_user] WITH GRANT OPTION;\n" +
			"DENY UPDATE ON OBJECT::[sales].[Orders] ([Total]) TO [app_user];\n" +
			"GRANT CONNECT TO [reporting_app];\n" +
			"GO\n",
	}})
}
