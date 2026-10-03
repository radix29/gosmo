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

// agentCategoryTarget maps an empty category name to what msdb accepts as
// "uncategorised" for class, returning both the @category_name to send and
// the name msdb.dbo.syscategories then reports for the object. A non-empty
// name is returned unchanged as both.
//
// Each class spells the default differently:
//
//   - ALERT, OPERATOR: sp_verify_category (shared by sp_update_alert and
//     sp_update_operator) rejects an empty name outright ("The specified
//     @category_name (”) does not exist"); [Uncategorized] is the real row
//     an alert or operator created with no category holds.
//   - JOB: sp_update_job accepts an empty name, but only by turning it into
//     NULL, which sp_verify_job resolves to category 0 — [Uncategorized
//     (Local)] even for a multi-server job. [DEFAULT] is sp_verify_job's own
//     "back to uncategorised" sentinel, resolved by job type to category 0
//     or 2, [Uncategorized (Multi-Server)]. (Read from msdb on 13.0.6500.1
//     and 17.0.1135.8, 2026-10-02.) stored assumes a local job — the only
//     kind CreateJob makes; Job carries no job type to tell otherwise.
func agentCategoryTarget(class CategoryClass, category string) (send, stored string) {
	if category != "" {
		return category, category
	}
	if class == CategoryClassJob {
		return "[DEFAULT]", "[Uncategorized (Local)]"
	}
	return "[Uncategorized]", "[Uncategorized]"
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
	server *Server
	ID     int
	Class  CategoryClass
	Name   string
}

// Server returns the server the category belongs to.
func (c *Category) Server() *Server { return c.server }

// CategoryRef returns a lightweight handle for a category of class by name,
// without querying msdb — the counterpart of Server.DatabaseRef. ID stays
// zero; CategoryByName is what populates it. Drop addresses the category by
// class and name alone, so this handle is enough for it.
func (s *Server) CategoryRef(class CategoryClass, name string) *Category {
	return &Category{server: s, Class: class, Name: name}
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
		c := &Category{server: s, Class: class}
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
	c := &Category{server: s, Class: class}
	err := s.queryRowScan(ctx, `
SELECT category_id, name
FROM   msdb.dbo.syscategories
WHERE  category_class = @p1 AND name = @p2`, []any{class.code(), name}, &c.ID, &c.Name)
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
	return createdObject(ctx, s.CategoryRef(req.Class, req.Name), func() (*Category, error) {
		return s.CategoryByName(ctx, req.Class, req.Name)
	})
}

// Drop deletes the category via sp_delete_category.
func (c *Category) Drop(ctx context.Context) error {
	if !validCategoryClass(c.Class) {
		return fmt.Errorf("gosmo: drop category %q: unrecognized category class %q", c.Name, c.Class)
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_category @class = N'%s', @name = N'%s'",
		string(c.Class), escapeSingle(c.Name))
	if err := c.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop category %q (%s): %w", c.Name, c.Class, err)
	}
	return nil
}
