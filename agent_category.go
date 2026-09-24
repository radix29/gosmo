package gosmo

import (
	"context"
	"fmt"
)

// ============================================================
// SQL Server Agent -- Categories
// ============================================================

// CategoryClass is the msdb.dbo.syscategories.category_class value a
// Category belongs to — also the literal sp_add_category/sp_delete_category
// @class parameter.
type CategoryClass string

const (
	CategoryClassJob      CategoryClass = "JOB"
	CategoryClassAlert    CategoryClass = "ALERT"
	CategoryClassOperator CategoryClass = "OPERATOR"
)

var categoryClassNames = map[CategoryClass]bool{
	CategoryClassJob: true, CategoryClassAlert: true, CategoryClassOperator: true,
}

// agentCategoryTarget maps an empty category name to the real
// [Uncategorized] category. sp_verify_category — which sp_update_alert and
// sp_update_operator both go through — rejects an empty name outright ("The
// specified @category_name (”) does not exist"), and [Uncategorized] is
// what an alert or operator created with no category actually holds in
// msdb.dbo.syscategories.
func agentCategoryTarget(category string) string {
	if category == "" {
		return "[Uncategorized]"
	}
	return category
}

// validCategoryClass reports whether c is a recognized category class.
func validCategoryClass(c CategoryClass) bool { return categoryClassNames[c] }

// code returns the numeric syscategories.category_class this class filters to.
func (c CategoryClass) code() int {
	switch c {
	case CategoryClassJob:
		return 1
	case CategoryClassAlert:
		return 2
	case CategoryClassOperator:
		return 3
	default:
		return 0
	}
}

// Category represents a SQL Server Agent job, alert, or operator category
// (msdb.dbo.syscategories) — Class says which.
type Category struct {
	ID    int
	Class CategoryClass
	Name  string
}

// Categories returns every category of the given class.
func (s *Server) Categories(ctx context.Context, class CategoryClass) ([]*Category, error) {
	if !validCategoryClass(class) {
		return nil, fmt.Errorf("gosmo: list categories: unrecognized category class %q", class)
	}
	const q = `
SELECT category_id, name
FROM   msdb.dbo.syscategories
WHERE  category_class = @p1
ORDER  BY name`

	rows, err := s.query(ctx, q, class.code())
	return scanRows(rows, err, fmt.Sprintf("list %s categories", class), func(scan func(...any) error) (*Category, error) {
		c := &Category{Class: class}
		if err := scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		return c, nil
	})
}

// addCategoryType returns the @type sp_add_category requires for a class:
// LOCAL for JOB (multi-server administration is out of scope — see
// CLAUDE.md's SQL-only exclusions), NONE for ALERT and OPERATOR, the only
// value those classes accept ("The specified '@type' is invalid (valid
// values are: NONE)").
func addCategoryType(class CategoryClass) string {
	if class == CategoryClassJob {
		return "LOCAL"
	}
	return "NONE"
}

// CategoryByName returns one category of the given class by name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when there is no
// such category.
func (s *Server) CategoryByName(ctx context.Context, class CategoryClass, name string) (*Category, error) {
	if !validCategoryClass(class) {
		return nil, fmt.Errorf("gosmo: find category %q: unrecognized category class %q", name, class)
	}
	c := &Category{Class: class, Name: name}
	err := s.queryRowScan(ctx, `
SELECT category_id
FROM   msdb.dbo.syscategories
WHERE  category_class = @p1 AND name = @p2`, []any{class.code(), name}, &c.ID)
	return foundRow(c, err, notFoundf("gosmo: %s category %q not found", class, name), fmt.Sprintf("find %s category %q", class, name))
}

// CreateCategoryRequest describes a new Agent category.
type CreateCategoryRequest struct {
	Class CategoryClass
	Name  string
}

// CreateCategory creates a new category via sp_add_category, and returns it
// read back from msdb — or, under Scripting(ctx), one carrying only its class
// and name, since nothing ran.
func (s *Server) CreateCategory(ctx context.Context, req CreateCategoryRequest) (*Category, error) {
	if !validCategoryClass(req.Class) {
		return nil, fmt.Errorf("gosmo: create category: unrecognized category class %q", req.Class)
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_add_category @class = N'%s', @type = N'%s', @name = N'%s'",
		string(req.Class), addCategoryType(req.Class), escapeSingle(req.Name))
	if err := s.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create category %q (%s): %w", req.Name, req.Class, err)
	}
	return createdObject(ctx, &Category{Class: req.Class, Name: req.Name}, func() (*Category, error) {
		return s.CategoryByName(ctx, req.Class, req.Name)
	})
}

// DeleteCategory deletes a category via sp_delete_category.
func (s *Server) DeleteCategory(ctx context.Context, class CategoryClass, name string) error {
	if !validCategoryClass(class) {
		return fmt.Errorf("gosmo: delete category: unrecognized category class %q", class)
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_category @class = N'%s', @name = N'%s'",
		string(class), escapeSingle(name))
	if err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: delete category %q (%s): %w", name, class, err)
	}
	return nil
}
