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
func (s *Server) Categories(class CategoryClass) ([]*Category, error) {
	return s.CategoriesContext(context.Background(), class)
}

// CategoriesContext is the context-aware variant of Categories.
func (s *Server) CategoriesContext(ctx context.Context, class CategoryClass) ([]*Category, error) {
	const q = `
SELECT category_id, name
FROM   msdb.dbo.syscategories
WHERE  category_class = @p1
ORDER  BY name`

	rows, err := s.db.QueryContext(ctx, q, class.code())
	if err != nil {
		return nil, fmt.Errorf("gosmo: list %s categories: %w", class, err)
	}
	defer rows.Close()

	var out []*Category
	for rows.Next() {
		c := &Category{Class: class}
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// addCategoryType returns the @type sp_add_category requires for a class:
// LOCAL for JOB (multi-server administration is out of scope — see
// CLAUDE.md's SQL-only exclusions), NONE for ALERT and OPERATOR — the only
// value those classes accept ("The specified '@type' is invalid (valid
// values are: NONE)", live-verified).
func addCategoryType(class CategoryClass) string {
	if class == CategoryClassJob {
		return "LOCAL"
	}
	return "NONE"
}

// CreateCategory creates a new category via sp_add_category.
func (s *Server) CreateCategory(class CategoryClass, name string) error {
	return s.CreateCategoryContext(context.Background(), class, name)
}

// CreateCategoryContext is the context-aware variant of CreateCategory.
func (s *Server) CreateCategoryContext(ctx context.Context, class CategoryClass, name string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_add_category @class = N'%s', @type = N'%s', @name = N'%s'",
		string(class), addCategoryType(class), escapeSingle(name))
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: create category %q (%s): %w", name, class, err)
	}
	return nil
}

// DeleteCategory deletes a category via sp_delete_category.
func (s *Server) DeleteCategory(class CategoryClass, name string) error {
	return s.DeleteCategoryContext(context.Background(), class, name)
}

// DeleteCategoryContext is the context-aware variant of DeleteCategory.
func (s *Server) DeleteCategoryContext(ctx context.Context, class CategoryClass, name string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_category @class = N'%s', @name = N'%s'",
		string(class), escapeSingle(name))
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: delete category %q (%s): %w", name, class, err)
	}
	return nil
}
