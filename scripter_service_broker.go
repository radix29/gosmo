package gosmo

// Scripter — the seven Service Broker families: message types, contracts,
// queues, services, routes, remote service bindings and broker priorities.
//
// Every builder here emits CREATE or DROP and never ALTER, even for the six
// families that have one: an ALTER restates a change, not the object, so
// ScriptAlter falls back to the CREATE the way ScriptOptions.Verb documents.
//
// Two things about these statements are not what the rest of the scripters
// assume, and both were probed rather than recalled (majors 13 and 17):
//
//   - **None of the seven supports DROP … IF EXISTS.** All seven fail to
//     parse, Msg 156 "Incorrect syntax near the keyword 'IF'". So a drop is
//     guarded with a catalog lookup instead, which is re-runnable the same
//     way and is what makes these scripts safe to paste twice.
//   - A Service Broker CREATE *may* be guarded by an IF, unlike CREATE RULE
//     and CREATE DEFAULT, which the server refuses anywhere but first in
//     their batch. So IncludeIfNotExists is honoured here.
//
// CREATE QUEUE and CREATE BROKER PRIORITY take no AUTHORIZATION clause — a
// queue belongs to its schema's owner and a broker priority has no owner at
// all — so the other five are the only ones that carry the owner.

import (
	"context"
	"fmt"
	"strings"
)

// brokerCatalogGuard builds the existence test the drops and the guarded
// creates share: a lookup by name in the family's catalog view. sense is
// "EXISTS" or "NOT EXISTS".
//
// The name goes into a string literal, so it is escaped for one — it is a
// value here, not an identifier, and the bracket-quoted form would find
// nothing.
func brokerCatalogGuard(sense, view, name string) string {
	return fmt.Sprintf("IF %s (SELECT 1 FROM sys.%s WHERE name = N'%s')\n",
		sense, view, escapeSingle(name))
}

// brokerDropScript is the guarded drop for one of the six schemaless
// families. keyword is the DROP's object keyword ("MESSAGE TYPE", "CONTRACT",
// …) and view the catalog view its names live in.
func brokerDropScript(keyword, view, name string) string {
	return brokerCatalogGuard("EXISTS", view, name) +
		fmt.Sprintf("    DROP %s %s;\nGO\n", keyword, quoteIdent(name))
}

// authorizationClause is the AUTHORIZATION line the five owned families
// carry, or nothing for an object whose owner could not be read. An owner
// that no longer exists comes back empty rather than as a name the CREATE
// would be refused for.
func authorizationClause(owner string) string {
	if owner == "" {
		return ""
	}
	return "    AUTHORIZATION " + quoteIdent(owner) + "\n"
}

// ============================================================
// Message types
// ============================================================

// ScriptMessageType generates the CREATE (or DROP) script for one message
// type.
func (sc *Scripter) ScriptMessageType(ctx context.Context, name string) (string, error) {
	mt, err := sc.db.MessageTypeByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildMessageTypeScript(mt, sc.opts), nil
}

// buildMessageTypeScript assembles one message type's script.
//
// The VALIDATION clause is always emitted, including for NONE: the default
// is NONE today, but a script that says so cannot be read as one that lost
// the clause, and the schema-collection form is the one case where getting
// it wrong silently creates a message type that accepts anything.
func buildMessageTypeScript(mt *MessageType, opts ScriptOptions) string {
	drop := brokerDropScript("MESSAGE TYPE", "service_message_types", mt.Name)
	guard := brokerCatalogGuard("NOT EXISTS", "service_message_types", mt.Name)
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE MESSAGE TYPE %s\n", mt.FullName())
		sb.WriteString(authorizationClause(mt.Owner))
		fmt.Fprintf(sb, "    VALIDATION = %s;\nGO\n", messageTypeValidationClause(mt))
	})
}

// messageTypeValidationClause renders the VALIDATION clause, which is the
// one place the two XML validations have to be told apart.
func messageTypeValidationClause(mt *MessageType) string {
	if mt.Validation == MessageTypeValidationValidXML {
		return "VALID_XML WITH SCHEMA COLLECTION " + mt.SchemaCollection()
	}
	return string(mt.Validation)
}

// ============================================================
// Contracts
// ============================================================

// ScriptContract generates the CREATE (or DROP) script for one service
// contract.
func (sc *Scripter) ScriptContract(ctx context.Context, name string) (string, error) {
	c, err := sc.db.ContractByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildContractScript(c, sc.opts), nil
}

// buildContractScript assembles one contract's script.
//
// A contract with no message types cannot be created, and the only one in
// that state is the system DEFAULT contract, whose single message type is
// itself called DEFAULT. Rather than emit a CREATE the server refuses, the
// script falls back to that pair — the shape the server would accept, the
// same choice buildAssemblyScript makes for a payload it cannot read.
func buildContractScript(c *ServiceContract, opts ScriptOptions) string {
	drop := brokerDropScript("CONTRACT", "service_contracts", c.Name)
	guard := brokerCatalogGuard("NOT EXISTS", "service_contracts", c.Name)
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE CONTRACT %s\n", c.FullName())
		sb.WriteString(authorizationClause(c.Owner))

		messages := c.Messages
		if len(messages) == 0 {
			messages = []ContractMessage{{MessageType: "DEFAULT", SentBy: ContractSentByAny}}
		}
		sb.WriteString("    (")
		for i, m := range messages {
			if i > 0 {
				sb.WriteString(",\n     ")
			}
			fmt.Fprintf(sb, "%s SENT BY %s", quoteIdent(m.MessageType), m.SentBy)
		}
		sb.WriteString(");\nGO\n")
	})
}

// ============================================================
// Queues
// ============================================================

// ScriptBrokerQueue generates the CREATE (or DROP) script for one queue.
func (sc *Scripter) ScriptBrokerQueue(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script broker queue", schema, name); err != nil {
		return "", err
	}
	q, err := sc.db.BrokerQueueByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildBrokerQueueScript(q, sc.opts), nil
}

// buildBrokerQueueScript assembles one queue's script — the one script in
// this file with substance, since a queue is the only family here with
// settings rather than only structure.
//
// STATUS is is_receive_enabled: ALTER QUEUE … WITH STATUS sets the enqueue
// and receive halves together, and a queue taken out of service reads false
// on both. The ACTIVATION block is emitted whenever the queue names a
// procedure, including with STATUS = OFF — activation that is configured but
// switched off is a state a script has to preserve, not one to drop.
func buildBrokerQueueScript(q *BrokerQueue, opts ScriptOptions) string {
	drop := fmt.Sprintf("IF OBJECT_ID(N'%s', 'SQ') IS NOT NULL\n    DROP QUEUE %s;\nGO\n", escapeSingle(q.FullName()), q.FullName())
	guard := fmt.Sprintf("IF OBJECT_ID(N'%s', 'SQ') IS NULL\n", escapeSingle(q.FullName()))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE QUEUE %s\n", q.FullName())
		fmt.Fprintf(sb, "    WITH STATUS = %s,\n", onOff(q.IsReceiveEnabled))
		fmt.Fprintf(sb, "         RETENTION = %s", onOff(q.IsRetentionEnabled))
		if q.ActivationProcedure != "" {
			sb.WriteString(",\n         ACTIVATION\n         (  ")
			fmt.Fprintf(sb, "STATUS = %s,\n            PROCEDURE_NAME = %s,\n",
				onOff(q.IsActivationEnabled), q.ActivationProcedure)
			fmt.Fprintf(sb, "            MAX_QUEUE_READERS = %d,\n", q.MaxReaders)
			fmt.Fprintf(sb, "            EXECUTE AS %s\n         )", queueExecuteAsClause(q))
		}
		fmt.Fprintf(sb, ",\n         POISON_MESSAGE_HANDLING (STATUS = %s)",
			onOff(q.IsPoisonMessageHandlingEnabled))
		if q.FileGroup != "" {
			fmt.Fprintf(sb, "\n    ON %s", quoteIdent(q.FileGroup))
		}
		sb.WriteString(";\nGO\n")
	})
}

// queueExecuteAsClause renders the activation principal. OWNER is a keyword;
// anything else is a user name and goes in a string literal, which is the
// form EXECUTE AS N'user' takes and the form a queue created with
// EXECUTE AS SELF reads back as.
func queueExecuteAsClause(q *BrokerQueue) string {
	if q.ActivationExecuteAs == "" || q.ActivationExecuteAs == "OWNER" {
		return "OWNER"
	}
	return QuoteLiteral(q.ActivationExecuteAs)
}

// ============================================================
// Services
// ============================================================

// ScriptBrokerService generates the CREATE (or DROP) script for one service.
func (sc *Scripter) ScriptBrokerService(ctx context.Context, name string) (string, error) {
	s, err := sc.db.BrokerServiceByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildBrokerServiceScript(s, sc.opts), nil
}

// buildBrokerServiceScript assembles one service's script.
//
// The contract list is omitted for a service that has none, which is what
// CREATE SERVICE without one means: the service accepts the system DEFAULT
// contract only. Emitting an empty parenthesised list instead would not
// parse.
func buildBrokerServiceScript(s *BrokerService, opts ScriptOptions) string {
	drop := brokerDropScript("SERVICE", "services", s.Name)
	guard := brokerCatalogGuard("NOT EXISTS", "services", s.Name)
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE SERVICE %s\n", s.FullName())
		sb.WriteString(authorizationClause(s.Owner))
		fmt.Fprintf(sb, "    ON QUEUE %s", s.Queue())
		if len(s.Contracts) > 0 {
			sb.WriteString("\n    (")
			for i, c := range s.Contracts {
				if i > 0 {
					sb.WriteString(",\n     ")
				}
				sb.WriteString(quoteIdent(c))
			}
			sb.WriteString(")")
		}
		sb.WriteString(";\nGO\n")
	})
}

// ============================================================
// Routes
// ============================================================

// ScriptRoute generates the CREATE (or DROP) script for one route.
func (sc *Scripter) ScriptRoute(ctx context.Context, name string) (string, error) {
	r, err := sc.db.RouteByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildRouteScript(r, sc.opts), nil
}

// buildRouteScript assembles one route's script.
//
// LIFETIME is the *remaining* seconds, because that is all sys.routes keeps
// — see Route.LifetimeSeconds. A route that has already expired scripts
// without the clause rather than with a zero, which CREATE ROUTE rejects.
func buildRouteScript(r *Route, opts ScriptOptions) string {
	drop := brokerDropScript("ROUTE", "routes", r.Name)
	guard := brokerCatalogGuard("NOT EXISTS", "routes", r.Name)
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE ROUTE %s\n", r.FullName())
		sb.WriteString(authorizationClause(r.Owner))

		var clauses []string
		if r.RemoteService != "" {
			clauses = append(clauses, "SERVICE_NAME = "+QuoteLiteral(r.RemoteService))
		}
		if r.BrokerInstance != "" {
			clauses = append(clauses, "BROKER_INSTANCE = "+QuoteLiteral(r.BrokerInstance))
		}
		if secs := r.LifetimeSeconds(); secs > 0 {
			clauses = append(clauses, fmt.Sprintf("LIFETIME = %d", secs))
		}
		clauses = append(clauses, "ADDRESS = "+QuoteLiteral(r.Address))
		if r.MirrorAddress != "" {
			clauses = append(clauses, "MIRROR_ADDRESS = "+QuoteLiteral(r.MirrorAddress))
		}
		fmt.Fprintf(sb, "    WITH %s;\nGO\n", strings.Join(clauses, ",\n         "))
	})
}

// ============================================================
// Remote service bindings
// ============================================================

// ScriptRemoteServiceBinding generates the CREATE (or DROP) script for one
// remote service binding.
//
// The CREATE it emits is the one statement in this file an Azure SQL Managed
// Instance refuses — Msg 41906, at compile time, which aborts the whole
// batch before anything in it runs. A caller that emits scripts into a batch
// with other statements keeps this one in a batch of its own.
func (sc *Scripter) ScriptRemoteServiceBinding(ctx context.Context, name string) (string, error) {
	b, err := sc.db.RemoteServiceBindingByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildRemoteServiceBindingScript(b, sc.opts), nil
}

// buildRemoteServiceBindingScript assembles one binding's script.
func buildRemoteServiceBindingScript(b *RemoteServiceBinding, opts ScriptOptions) string {
	drop := brokerDropScript("REMOTE SERVICE BINDING", "remote_service_bindings", b.Name)
	guard := brokerCatalogGuard("NOT EXISTS", "remote_service_bindings", b.Name)
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE REMOTE SERVICE BINDING %s\n", b.FullName())
		sb.WriteString(authorizationClause(b.Owner))
		fmt.Fprintf(sb, "    TO SERVICE %s\n", QuoteLiteral(b.RemoteService))
		fmt.Fprintf(sb, "    WITH USER = %s, ANONYMOUS = %s;\nGO\n",
			quoteIdent(b.User), onOff(b.IsAnonymous))
	})
}

// ============================================================
// Broker priorities
// ============================================================

// ScriptBrokerPriority generates the CREATE (or DROP) script for one
// conversation priority.
func (sc *Scripter) ScriptBrokerPriority(ctx context.Context, name string) (string, error) {
	p, err := sc.db.BrokerPriorityByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildBrokerPriorityScript(p, sc.opts), nil
}

// buildBrokerPriorityScript assembles one conversation priority's script.
//
// Each of the three criteria is emitted as ANY when the catalog has none,
// which is what CREATE BROKER PRIORITY means by a criterion left out — and
// the form is written out rather than omitted so that a priority scripted
// from a narrow one cannot be misread as matching everything by accident.
func buildBrokerPriorityScript(p *BrokerPriority, opts ScriptOptions) string {
	drop := brokerDropScript("BROKER PRIORITY", "conversation_priorities", p.Name)
	guard := brokerCatalogGuard("NOT EXISTS", "conversation_priorities", p.Name)
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE BROKER PRIORITY %s\n", p.FullName())
		sb.WriteString("FOR CONVERSATION\n")
		fmt.Fprintf(sb, "SET (CONTRACT_NAME = %s,\n", anyOrIdent(p.Contract))
		fmt.Fprintf(sb, "     LOCAL_SERVICE_NAME = %s,\n", anyOrIdent(p.LocalService))
		fmt.Fprintf(sb, "     REMOTE_SERVICE_NAME = %s,\n", anyOrLiteral(p.RemoteService))
		fmt.Fprintf(sb, "     PRIORITY_LEVEL = %d);\nGO\n", p.Level)
	})
}

// anyOrIdent renders a criterion that names an object in this database.
func anyOrIdent(name string) string {
	if name == "" {
		return "ANY"
	}
	return quoteIdent(name)
}

// anyOrLiteral renders the remote service criterion, which is a name in
// another database or instance and so a string literal, not an identifier.
func anyOrLiteral(name string) string {
	if name == "" {
		return "ANY"
	}
	return QuoteLiteral(name)
}
