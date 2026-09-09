package gosmo

import (
	"strings"
	"testing"
)

func TestBuildUserDefinedDataTypeScriptCarriesLengthNullabilityAndBindings(t *testing.T) {
	typ := &UserDefinedDataType{Schema: "dbo", Name: "Phone", BaseType: "nvarchar",
		MaxLength: 40, IsNullable: false, Rule: "PhoneRule", Default: "PhoneDefault"}

	got := buildUserDefinedDataTypeScript(typ, DefaultScriptOptions())
	// max_length is bytes: 40 there is nvarchar(20), and emitting 40 would
	// double the type's width on every rerun of the script.
	if !strings.Contains(got, "CREATE TYPE [dbo].[Phone] FROM nvarchar(20) NOT NULL;") {
		t.Errorf("alias type not scripted from its base type:\n%s", got)
	}
	if !strings.Contains(got, "EXEC sp_bindrule N'[PhoneRule]', N'[dbo].[Phone]';") {
		t.Errorf("bound rule missing — the type would accept values the original refuses:\n%s", got)
	}
	if !strings.Contains(got, "EXEC sp_bindefault N'[PhoneDefault]', N'[dbo].[Phone]';") {
		t.Errorf("bound default missing:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	drop := buildUserDefinedDataTypeScript(typ, opts)
	if !strings.Contains(drop, "DROP TYPE IF EXISTS [dbo].[Phone];") {
		t.Errorf("drop wrong:\n%s", drop)
	}
	// A drop is a drop: no binding statements ride along behind it.
	if strings.Contains(drop, "sp_bindrule") {
		t.Errorf("drop carries the bindings:\n%s", drop)
	}
}

func TestBuildUserDefinedDataTypeScriptStatesNullabilityExplicitly(t *testing.T) {
	nullable := &UserDefinedDataType{Schema: "dbo", Name: "Note", BaseType: "varchar",
		MaxLength: -1, IsNullable: true}
	got := buildUserDefinedDataTypeScript(nullable, DefaultScriptOptions())
	if !strings.Contains(got, "FROM varchar(MAX) NULL;") {
		t.Errorf("a nullable alias type must say NULL — the default depends on the connection:\n%s", got)
	}
}

func TestBuildUserDefinedTableTypeScriptColumnsAndMemoryOptimized(t *testing.T) {
	tt := &UserDefinedTableType{Schema: "dbo", Name: "IDList"}
	cols := []*Column{
		{Name: "id", DataType: DataTypeInt, IsNullable: false},
		{Name: "label", DataType: DataTypeNVarChar, MaxLength: 100, IsNullable: true},
	}

	got := buildUserDefinedTableTypeScript(tt, cols, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE TYPE [dbo].[IDList] AS TABLE (") {
		t.Errorf("table type not scripted AS TABLE:\n%s", got)
	}
	if !strings.Contains(got, "[id] int NOT NULL,\n") || !strings.Contains(got, "[label] nvarchar(50) NULL\n") {
		t.Errorf("columns wrong — the last one takes no comma:\n%s", got)
	}
	if strings.Contains(got, "MEMORY_OPTIMIZED") {
		t.Errorf("a disk table type must not be scripted memory-optimized:\n%s", got)
	}

	tt.IsMemoryOptimized = true
	if got := buildUserDefinedTableTypeScript(tt, cols, DefaultScriptOptions()); !strings.Contains(got, "WITH (MEMORY_OPTIMIZED = ON);") {
		t.Errorf("memory-optimized table type loses its clause:\n%s", got)
	}
}

func TestBuildClrTypeScriptNamesTheAssemblyAndClass(t *testing.T) {
	ct := &ClrType{Schema: "dbo", Name: "Point", Assembly: "Geometry", AssemblyClass: "Points.Point"}
	got := buildClrTypeScript(ct, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE TYPE [dbo].[Point] EXTERNAL NAME [Geometry].[Points.Point];") {
		t.Errorf("CLR type must name assembly and class:\n%s", got)
	}

	// A row with no assembly_types match has no class; the statement still
	// has to parse, so the trailing dot must not be emitted.
	bare := &ClrType{Schema: "dbo", Name: "Point", Assembly: "Geometry"}
	if got := buildClrTypeScript(bare, DefaultScriptOptions()); !strings.Contains(got, "EXTERNAL NAME [Geometry];") {
		t.Errorf("a class-less CLR type must not trail a dot:\n%s", got)
	}
}

func TestBuildXmlSchemaCollectionScriptGuardsTheDropAndCarriesTheDefinition(t *testing.T) {
	c := &XmlSchemaCollection{Schema: "dbo", Name: "OrderSchema"}
	def := `<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema" id="it's" />`

	got := buildXmlSchemaCollectionScript(c, def, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE XML SCHEMA COLLECTION [dbo].[OrderSchema] AS N'") {
		t.Errorf("collection not scripted:\n%s", got)
	}
	// The definition goes inside a literal, so its own quotes must double or
	// the batch ends in the middle of the schema.
	if !strings.Contains(got, "id=\"it''s\"") {
		t.Errorf("definition not escaped for a literal:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	drop := buildXmlSchemaCollectionScript(c, "", opts)
	// DROP XML SCHEMA COLLECTION has no IF EXISTS form.
	if !strings.Contains(drop, "IF EXISTS (SELECT 1 FROM sys.xml_schema_collections") ||
		!strings.Contains(drop, "DROP XML SCHEMA COLLECTION [dbo].[OrderSchema];") {
		t.Errorf("drop must be guarded by a catalog lookup:\n%s", drop)
	}
	if !strings.Contains(drop, "SCHEMA_NAME(x.schema_id) = N'dbo'") {
		t.Errorf("guard must match the schema too — two schemas can hold the same name:\n%s", drop)
	}
}

func TestBuildBoundObjectScriptEmitsTheStoredDefinitionUnguarded(t *testing.T) {
	def := "CREATE RULE [dbo].[PhoneRule]\nAS @value LIKE '[0-9][0-9][0-9]'"
	got := buildBoundObjectScript("RULE", "dbo", "PhoneRule", def, DefaultScriptOptions())

	if !strings.HasPrefix(got, "CREATE RULE") {
		t.Errorf("CREATE RULE must start its batch — an IF guard is a parse error:\n%s", got)
	}
	if !strings.Contains(got, "@value LIKE") || !strings.HasSuffix(got, "\nGO\n") {
		t.Errorf("definition not emitted verbatim and terminated:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	both := buildBoundObjectScript("DEFAULT", "dbo", "Zero", "CREATE DEFAULT [dbo].[Zero] AS 0", opts)
	drop, create := strings.Index(both, "DROP DEFAULT IF EXISTS [dbo].[Zero]"), strings.Index(both, "CREATE DEFAULT")
	if drop < 0 || create < 0 || drop > create {
		t.Errorf("DROP-and-CREATE out of order or incomplete:\n%s", both)
	}
}

func TestBuildBoundObjectScriptSaysSoWhenTheDefinitionIsEncrypted(t *testing.T) {
	got := buildBoundObjectScript("RULE", "dbo", "Secret", "", DefaultScriptOptions())
	if !strings.Contains(got, "encrypted") {
		t.Errorf("an encrypted rule must say so, not script as an empty batch:\n%s", got)
	}
	if strings.Contains(got, "CREATE RULE") {
		t.Errorf("nothing to create — a CREATE here would be a rule with no expression:\n%s", got)
	}
}

func TestBuildAssemblyScriptElidesThePayloadAndKeepsThePermissionSet(t *testing.T) {
	a := &Assembly{Name: "Geometry", Owner: "dbo", PermissionSet: AssemblyUnsafe, IsVisible: true}
	got := buildAssemblyScript(a, DefaultScriptOptions())

	if !strings.Contains(got, "CREATE ASSEMBLY [Geometry] AUTHORIZATION [dbo]") {
		t.Errorf("assembly not scripted with its owner:\n%s", got)
	}
	if !strings.Contains(got, assemblyBinaryPlaceholder) {
		t.Errorf("payload placeholder missing — the script must not look runnable as generated:\n%s", got)
	}
	// SAFE is the default, so an UNSAFE assembly that scripts without the
	// clause comes back running under a policy it was never given.
	if !strings.Contains(got, "WITH PERMISSION_SET = UNSAFE;") {
		t.Errorf("permission set lost:\n%s", got)
	}
	if strings.Contains(got, "VISIBILITY") {
		t.Errorf("a visible assembly needs no visibility statement:\n%s", got)
	}

	hidden := &Assembly{Name: "Dep", PermissionSet: AssemblySafe}
	if got := buildAssemblyScript(hidden, DefaultScriptOptions()); !strings.Contains(got, "WITH VISIBILITY = OFF;") {
		t.Errorf("a referenced-only assembly must stay invisible:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if got := buildAssemblyScript(a, opts); !strings.Contains(got, "DROP ASSEMBLY IF EXISTS [Geometry];") {
		t.Errorf("drop wrong:\n%s", got)
	}
}

func TestBuildPlanGuideScriptIsAnSpCreatePlanGuideCall(t *testing.T) {
	g := &PlanGuide{Name: "pg_orders", Scope: PlanGuideScopeObject,
		QueryText:   "SELECT * FROM dbo.Orders WHERE id = @id",
		ScopeObject: "[dbo].[GetOrder]", Hints: "OPTION (RECOMPILE)"}

	got := buildPlanGuideScript(g, DefaultScriptOptions())
	if !strings.Contains(got, "EXEC sp_create_plan_guide") {
		t.Errorf("a plan guide has no CREATE statement — it is an EXEC:\n%s", got)
	}
	if !strings.Contains(got, "@type = N'OBJECT'") || !strings.Contains(got, "@module_or_batch = N'[dbo].[GetOrder]'") {
		t.Errorf("OBJECT scope must name the routine in @module_or_batch:\n%s", got)
	}
	// Everything gosmo reads is ISNULL'd to '' at the catalog; '' is not the
	// argument sp_create_plan_guide expects where the guide had none.
	if !strings.Contains(got, "@params = NULL") {
		t.Errorf("an absent parameter list must go as NULL, not as an empty literal:\n%s", got)
	}
	if strings.Contains(got, "DISABLE") {
		t.Errorf("an enabled guide must not be scripted disabled:\n%s", got)
	}

	tmpl := &PlanGuide{Name: "pg_t", Scope: PlanGuideScopeTemplate, QueryText: "SELECT 1",
		ScopeBatch: "ignored", Parameters: "@p int", IsDisabled: true}
	got = buildPlanGuideScript(tmpl, DefaultScriptOptions())
	// @module_or_batch is the same parameter for two different things; a
	// TEMPLATE guide takes neither.
	if !strings.Contains(got, "@module_or_batch = NULL") {
		t.Errorf("TEMPLATE scope must pass NULL for @module_or_batch:\n%s", got)
	}
	if !strings.Contains(got, "@params = N'@p int'") {
		t.Errorf("parameter list lost:\n%s", got)
	}
	if !strings.Contains(got, "@operation = N'DISABLE'") {
		t.Errorf("a disabled guide recreated enabled starts applying hints again:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if got := buildPlanGuideScript(g, opts); !strings.Contains(got, "@operation = N'DROP', @name = N'pg_orders'") {
		t.Errorf("drop must be sp_control_plan_guide's:\n%s", got)
	}
}

func TestBuildPlanGuideScriptEscapesTheQueryText(t *testing.T) {
	g := &PlanGuide{Name: "pg", Scope: PlanGuideScopeSQL,
		QueryText: "SELECT * FROM t WHERE name = 'it''s'", ScopeBatch: "SELECT * FROM t"}
	got := buildPlanGuideScript(g, DefaultScriptOptions())
	if !strings.Contains(got, "name = ''it''''s''") {
		t.Errorf("query text not escaped — the literal ends inside the statement:\n%s", got)
	}
	if !strings.Contains(got, "@module_or_batch = N'SELECT * FROM t'") {
		t.Errorf("SQL scope must pass its batch text:\n%s", got)
	}
}
