// Package tsql is gosmo's T-SQL text layer: quoting identifiers and
// literals, keeping names from breaking out of comments, and splitting
// script text into the runs the server lexes differently. It imports only
// the standard library, so a consumer that builds or reads T-SQL text
// without connecting to a server need not link the mssql driver and its
// dependencies, as importing gosmo does.
//
// gosmo calls it for all of these; its own QuoteName, QualifiedName,
// QuoteLiteral, QuoteAnsiLiteral and UnquoteName forward here, so either import gives the
// same text. TestImportsOnlyStdlib keeps the package a leaf.
package tsql
