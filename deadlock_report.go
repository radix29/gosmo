package gosmo

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// deadlock_report.go decodes a deadlock graph: the <deadlock> document an
// xml_deadlock_report event carries in its xml_report field (system_health
// records every deadlock with one), and the <deadlock-list> an .xdl file
// saved by SSMS holds. It is pure decoding; reading the events is
// XEvents.ReadFileEvents' or EventSession.ReadRingBuffer's.

// DeadlockReport is one deadlock: the processes in the cycle, the resources
// they held and waited on, and which of them the server killed.
type DeadlockReport struct {
	// Victims are the ids of the processes chosen as victims, in the order
	// the report lists them — more than one in a multi-victim deadlock, none
	// in an intra-query parallelism deadlock, which the server resolves
	// without killing anyone.
	Victims   []string
	Processes []DeadlockProcess
	Resources []DeadlockResource
}

// Process returns the process with the given id, and whether there is one.
func (r *DeadlockReport) Process(id string) (*DeadlockProcess, bool) {
	for i := range r.Processes {
		if r.Processes[i].ID == id {
			return &r.Processes[i], true
		}
	}
	return nil, false
}

// DeadlockProcess is one task in a deadlock. Several share a SessionID when
// a parallel plan's workers took part; ExecutionContextID tells them apart.
type DeadlockProcess struct {
	// ID is the report's own id for the task (process1d0b3b4e8c8), what
	// Victims and a resource's owners and waiters name.
	ID     string
	Victim bool

	SessionID          int // spid
	ExecutionContextID int // ecid: 0 the request's own task, > 0 a parallel worker
	Status             string

	Login     string
	Host      string
	ClientApp string
	HostPID   int

	// DatabaseID is the task's current database; Database its name, which
	// depends on the build, not the major: 2016 SP3 and 2025 write it, 2017
	// RTM does not (fixtures in testdata/deadlocks). Where it is empty,
	// resolve DatabaseID.
	DatabaseID int
	Database   string

	IsolationLevel string // "read committed (2)", as the report words it
	LockMode       string // the mode it was waiting to acquire
	WaitResource   string // KEY: 5:72057594043367424 (8194443284a0), …
	WaitTimeMS     int64

	TransactionName string
	TranCount       int
	LogUsed         int64 // bytes of log the transaction had written
	// DeadlockPriority is the session's DEADLOCK_PRIORITY, -10 to 10.
	DeadlockPriority int

	// The times are the server's local clock, carried in time.UTC; zero
	// when the report does not give one.
	LastTranStarted    time.Time
	LastBatchStarted   time.Time
	LastBatchCompleted time.Time

	// Frames is the execution stack, innermost first.
	Frames []DeadlockFrame
	// InputBuffer is the batch the client sent, as DBCC INPUTBUFFER shows
	// it.
	InputBuffer string
}

// Statement returns the text of the innermost frame that has one — what the
// task was running — or the input buffer when no frame does.
func (p *DeadlockProcess) Statement() string {
	for _, f := range p.Frames {
		if f.Text != "" {
			return f.Text
		}
	}
	return p.InputBuffer
}

// DeadlockFrame is one frame of a process's execution stack.
type DeadlockFrame struct {
	// ProcName is the module (db.schema.name), or "adhoc"/"unknown".
	ProcName string
	Line     int
	// StmtStart and StmtEnd are byte offsets into the batch, -1 when absent.
	StmtStart int
	StmtEnd   int
	SQLHandle string // 0x…
	// Text is the statement where the report includes it, empty otherwise
	// (the server writes "unknown" for a frame it cannot name, which reads as
	// empty here).
	Text string
}

// DeadlockResource is one resource in the cycle: a lock (keylock, ridlock,
// pagelock, objectlock, …) or a parallel exchange (exchangeEvent).
type DeadlockResource struct {
	// Kind is the report's element name: keylock, pagelock, ridlock,
	// objectlock, exchangeEvent, metadatalock, …
	Kind string
	ID   string
	// Mode is the lock mode the owner holds; empty for an exchangeEvent.
	Mode string

	DatabaseID int
	// ObjectName is db.schema.object for a lock on an object or its rows;
	// IndexName the index for a key or page lock. Either is empty where the
	// kind has none.
	ObjectName string
	IndexName  string
	// WaitType is an exchangeEvent's (e_waitPipeGetRow, …).
	WaitType string

	// Attributes holds every attribute of the element, the ones above
	// included, for the kinds' own (fileid, pageid, hobtid…).
	Attributes map[string]string

	Owners  []DeadlockLockHolder
	Waiters []DeadlockLockHolder
}

// DeadlockLockHolder is one process holding or waiting on a resource.
type DeadlockLockHolder struct {
	ProcessID string
	Mode      string
	// RequestType is a waiter's: wait, convert.
	RequestType string
}

type xmlDeadlock struct {
	Victim  string `xml:"victim,attr"` // the pre-2012 / .xdl single-victim form
	Victims []struct {
		ID string `xml:"id,attr"`
	} `xml:"victim-list>victimProcess"`
	Processes []struct {
		Attrs  []xml.Attr `xml:",any,attr"`
		Frames []struct {
			Attrs []xml.Attr `xml:",any,attr"`
			Text  string     `xml:",chardata"`
		} `xml:"executionStack>frame"`
		InputBuf string `xml:"inputbuf"`
	} `xml:"process-list>process"`
	Resources struct {
		Items []struct {
			XMLName xml.Name
			Attrs   []xml.Attr `xml:",any,attr"`
			Owners  []struct {
				Attrs []xml.Attr `xml:",any,attr"`
			} `xml:"owner-list>owner"`
			Waiters []struct {
				Attrs []xml.Attr `xml:",any,attr"`
			} `xml:"waiter-list>waiter"`
		} `xml:",any"`
	} `xml:"resource-list"`
}

// DecodeDeadlockReport decodes the first <deadlock> element in b, at any
// depth: an xml_report field's value (XEValue.Value), a whole event, or an
// .xdl file's <deadlock-list>. A document with no <deadlock> is an error.
func DecodeDeadlockReport(b []byte) (*DeadlockReport, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, invalidf("gosmo: decode deadlock report: no <deadlock> element")
		}
		if err != nil {
			return nil, fmt.Errorf("gosmo: decode deadlock report: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "deadlock" {
			continue
		}
		var raw xmlDeadlock
		if err := dec.DecodeElement(&raw, &se); err != nil {
			return nil, fmt.Errorf("gosmo: decode deadlock report: %w", err)
		}
		return raw.decode(), nil
	}
}

func (d *xmlDeadlock) decode() *DeadlockReport {
	r := &DeadlockReport{}
	for _, v := range d.Victims {
		r.Victims = append(r.Victims, v.ID)
	}
	if len(r.Victims) == 0 && d.Victim != "" {
		r.Victims = []string{d.Victim}
	}
	for _, xp := range d.Processes {
		a := attrMap(xp.Attrs)
		p := DeadlockProcess{
			ID:                 a["id"],
			SessionID:          atoiAttr(a["spid"]),
			ExecutionContextID: atoiAttr(a["ecid"]),
			Status:             a["status"],
			Login:              a["loginname"],
			Host:               a["hostname"],
			ClientApp:          a["clientapp"],
			HostPID:            atoiAttr(a["hostpid"]),
			DatabaseID:         atoiAttr(a["currentdb"]),
			Database:           a["currentdbname"],
			IsolationLevel:     a["isolationlevel"],
			LockMode:           a["lockMode"],
			WaitResource:       a["waitresource"],
			WaitTimeMS:         atoi64Attr(a["waittime"]),
			TransactionName:    a["transactionname"],
			TranCount:          atoiAttr(a["trancount"]),
			LogUsed:            atoi64Attr(a["logused"]),
			DeadlockPriority:   atoiAttr(a["priority"]),
			LastTranStarted:    deadlockTime(a["lasttranstarted"]),
			LastBatchStarted:   deadlockTime(a["lastbatchstarted"]),
			LastBatchCompleted: deadlockTime(a["lastbatchcompleted"]),
			InputBuffer:        strings.TrimSpace(xp.InputBuf),
		}
		for _, v := range r.Victims {
			if v == p.ID {
				p.Victim = true
			}
		}
		for _, xf := range xp.Frames {
			fa := attrMap(xf.Attrs)
			f := DeadlockFrame{
				ProcName:  fa["procname"],
				Line:      atoiAttr(fa["line"]),
				StmtStart: -1,
				StmtEnd:   -1,
				SQLHandle: fa["sqlhandle"],
				Text:      strings.TrimSpace(xf.Text),
			}
			if v, ok := fa["stmtstart"]; ok {
				f.StmtStart = atoiAttr(v)
			}
			if v, ok := fa["stmtend"]; ok {
				f.StmtEnd = atoiAttr(v)
			}
			if f.Text == "unknown" {
				f.Text = ""
			}
			p.Frames = append(p.Frames, f)
		}
		r.Processes = append(r.Processes, p)
	}
	for _, xr := range d.Resources.Items {
		a := attrMap(xr.Attrs)
		res := DeadlockResource{
			Kind:       xr.XMLName.Local,
			ID:         a["id"],
			Mode:       a["mode"],
			DatabaseID: atoiAttr(a["dbid"]),
			ObjectName: a["objectname"],
			IndexName:  a["indexname"],
			WaitType:   a["WaitType"],
			Attributes: a,
		}
		for _, o := range xr.Owners {
			oa := attrMap(o.Attrs)
			res.Owners = append(res.Owners, DeadlockLockHolder{ProcessID: oa["id"], Mode: oa["mode"], RequestType: oa["requestType"]})
		}
		for _, w := range xr.Waiters {
			wa := attrMap(w.Attrs)
			res.Waiters = append(res.Waiters, DeadlockLockHolder{ProcessID: wa["id"], Mode: wa["mode"], RequestType: wa["requestType"]})
		}
		r.Resources = append(r.Resources, res)
	}
	return r
}

func attrMap(attrs []xml.Attr) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Name.Local] = a.Value
	}
	return m
}

// atoiAttr and atoi64Attr read a numeric attribute, 0 when it is absent or
// not a number: a report is diagnostic text, and one odd attribute must not
// lose the rest of it.
func atoiAttr(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoi64Attr(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// deadlockTime reads a report's zoneless local time
// (2026-10-30T10:15:02.120), zero when absent or unreadable.
func deadlockTime(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", strings.TrimSpace(s), time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}
