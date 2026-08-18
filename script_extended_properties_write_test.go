package gosmo

import (
	"context"
	"testing"
)

// TestScriptExtendedPropertyWrites pins the sp_addextendedproperty family.
// See script_write_common_test.go.
//
// These are the sharpest case for a whole-statement assertion in the package.
// Every one of the eight level arguments is a *literal*, not an identifier, so
// nothing brackets them and the only thing standing between a table named
// O'Brien and a syntax error is escapeSingle. The level pairs also carry a
// second rule: an unused level is the NULL keyword, not an empty string
// literal — nullableStr maps an empty string to NULL for exactly that, and a
// build that quoted it instead would look right and address the wrong object
// level.
func TestScriptExtendedPropertyWrites(t *testing.T) {
	tableLevel := ExtendedPropertyLevel{
		Level0Type: "SCHEMA", Level0Name: "dbo",
		Level1Type: "TABLE", Level1Name: "Sales'Archive",
	}
	columnLevel := ExtendedPropertyLevel{
		Level0Type: "SCHEMA", Level0Name: "dbo",
		Level1Type: "TABLE", Level1Name: "Sales'Archive",
		Level2Type: "COLUMN", Level2Name: "O'Brien",
	}

	runScriptCases(t, []scriptCase{
		{"AddExtendedProperty", func(c context.Context) error {
			return scriptTestDB().AddExtendedPropertyContext(c, "MS_Description", "o'brien", tableLevel)
		}, scriptUsePrefix + `
EXEC sp_addextendedproperty
    @name = N'MS_Description', @value = N'o''brien',
    @level0type = N'SCHEMA', @level0name = N'dbo',
    @level1type = N'TABLE', @level1name = N'Sales''Archive',
    @level2type = NULL, @level2name = NULL`},
		{"AddExtendedProperty at column level", func(c context.Context) error {
			return scriptTestDB().AddExtendedPropertyContext(c, "MS_Description", "v", columnLevel)
		}, scriptUsePrefix + `
EXEC sp_addextendedproperty
    @name = N'MS_Description', @value = N'v',
    @level0type = N'SCHEMA', @level0name = N'dbo',
    @level1type = N'TABLE', @level1name = N'Sales''Archive',
    @level2type = N'COLUMN', @level2name = N'O''Brien'`},
		{"SetExtendedProperty", func(c context.Context) error {
			return scriptTestDB().SetExtendedPropertyContext(c, "MS_Description", "v'2", tableLevel)
		}, scriptUsePrefix + `
EXEC sp_updateextendedproperty
    @name = N'MS_Description', @value = N'v''2',
    @level0type = N'SCHEMA', @level0name = N'dbo',
    @level1type = N'TABLE', @level1name = N'Sales''Archive',
    @level2type = NULL, @level2name = NULL`},
		{"DropExtendedProperty", func(c context.Context) error {
			return scriptTestDB().DropExtendedPropertyContext(c, "MS_Description", tableLevel)
		}, scriptUsePrefix + `
EXEC sp_dropextendedproperty
    @name = N'MS_Description',
    @level0type = N'SCHEMA', @level0name = N'dbo',
    @level1type = N'TABLE', @level1name = N'Sales''Archive',
    @level2type = NULL, @level2name = NULL`},
	})
}
