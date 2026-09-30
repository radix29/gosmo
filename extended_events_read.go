package gosmo

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// extended_events_read.go reads what an event session collected: an
// event_file target through sys.fn_xe_file_target_read_file, a ring_buffer
// through sys.dm_xe_session_targets, and the event XML both return.
//
// Live data is read by polling a target, not from the XE stream SSMS's Watch
// Live Data uses: that goes through the undocumented
// sys.fn_MSxe_read_event_stream and an undocumented binary format.
//
// Timestamps come from each event's XML, never from the file function's
// timestamp_utc column, which SQL Server 2016 does not have.

// XEvent is one event as a target recorded it.
type XEvent struct {
	Name      string
	Package   string
	Timestamp time.Time // UTC

	// Seq is the package0.event_sequence action's value, 0 when the session
	// does not collect it. It is what tells two otherwise identical events
	// apart when a ring_buffer is re-read.
	Seq uint64

	Fields  []XEValue // the event's <data> elements
	Actions []XEValue // its <action> elements
}

// XEValue is one field or action of an event.
type XEValue struct {
	Name string

	// Type is the XE type name (uint64, unicode_string, a map name…). A
	// ring_buffer records it; an event_file read does not, and there it is
	// empty.
	Type string

	// Value is the raw value. A map-typed field's is the key, and Text holds
	// the key's text (a wait_type's 179 and "PAGEIOLATCH_SH").
	Value string
	Text  string

	// IsXML marks a value that is an XML fragment rather than text — an
	// xml-typed field such as xml_deadlock_report's xml_report, or
	// sp_server_diagnostics' data. Value then holds the fragment as markup.
	IsXML bool
}

// Field returns the named field and whether the event carries it.
func (e XEvent) Field(name string) (XEValue, bool) { return findXEValue(e.Fields, name) }

// Action returns the named action (by name alone, without its package) and
// whether the event carries it.
func (e XEvent) Action(name string) (XEValue, bool) { return findXEValue(e.Actions, name) }

func findXEValue(vs []XEValue, name string) (XEValue, bool) {
	for _, v := range vs {
		if v.Name == name {
			return v, true
		}
	}
	return XEValue{}, false
}

// -- Decoding -----------------------------------------------------------------------

type xmlXEEvent struct {
	Name      string       `xml:"name,attr"`
	Package   string       `xml:"package,attr"`
	Timestamp string       `xml:"timestamp,attr"`
	Data      []xmlXEValue `xml:"data"`
	Actions   []xmlXEValue `xml:"action"`
}

type xmlXEValue struct {
	Name    string `xml:"name,attr"`
	Package string `xml:"package,attr"`
	Type    struct {
		Name string `xml:"name,attr"`
	} `xml:"type"`
	Value struct {
		Inner string `xml:",innerxml"`
		Chars string `xml:",chardata"`
	} `xml:"value"`
	Text string `xml:"text"`
}

func (v xmlXEValue) decode() XEValue {
	out := XEValue{Name: v.Name, Type: v.Type.Name, Value: v.Value.Chars, Text: v.Text}
	// A value with element children is an XML fragment; its chardata is only
	// the whitespace between them. CDATA is text, however much it looks like
	// markup (callstack_rva's frames are CDATA).
	if inner := strings.TrimSpace(v.Value.Inner); strings.HasPrefix(inner, "<") && !strings.HasPrefix(inner, "<![CDATA[") {
		out.Value, out.IsXML = inner, true
	}
	return out
}

func (e xmlXEEvent) decode() (XEvent, error) {
	out := XEvent{Name: e.Name, Package: e.Package}
	if e.Timestamp != "" {
		ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil {
			return XEvent{}, fmt.Errorf("event %q: timestamp %q: %w", e.Name, e.Timestamp, err)
		}
		out.Timestamp = ts.UTC()
	}
	out.Fields = make([]XEValue, len(e.Data))
	for i, d := range e.Data {
		out.Fields[i] = d.decode()
	}
	out.Actions = make([]XEValue, len(e.Actions))
	for i, a := range e.Actions {
		out.Actions[i] = a.decode()
		if a.Name == "event_sequence" && (a.Package == "" || a.Package == "package0") {
			out.Seq, _ = strconv.ParseUint(strings.TrimSpace(out.Actions[i].Value), 10, 64)
		}
	}
	return out, nil
}

// DecodeEventXML decodes every <event> element in b, at any depth: one event
// as fn_xe_file_target_read_file returns it, a ring_buffer's whole
// target_data, or a document of events a caller saved. Events come back in
// document order.
func DecodeEventXML(b []byte) ([]XEvent, error) {
	events, _, err := decodeXE(b)
	return events, err
}

// RingBufferData is a ring_buffer target's contents: its events and the
// counters on the RingBufferTarget element.
type RingBufferData struct {
	Events []XEvent

	TotalEventsProcessed int64 // every event the target ever saw
	EventCount           int64 // events in the buffer now
	DroppedCount         int64
	MemoryUsed           int64 // bytes

	// Truncated is set when target_data was cut to fit its ~4 MB limit, the
	// oldest events going first; EventCount then exceeds len(Events).
	Truncated bool
}

// DecodeRingBuffer decodes a ring_buffer's target_data.
func DecodeRingBuffer(b []byte) (*RingBufferData, error) {
	events, root, err := decodeXE(b)
	if err != nil {
		return nil, err
	}
	rb := &RingBufferData{Events: events}
	if root != nil && root.Name.Local == "RingBufferTarget" {
		for _, a := range root.Attr {
			n, _ := strconv.ParseInt(a.Value, 10, 64)
			switch a.Name.Local {
			case "totalEventsProcessed":
				rb.TotalEventsProcessed = n
			case "eventCount":
				rb.EventCount = n
			case "droppedCount":
				rb.DroppedCount = n
			case "memoryUsed":
				rb.MemoryUsed = n
			case "truncated":
				rb.Truncated = n != 0
			}
		}
	}
	return rb, nil
}

// decodeXE streams b, decoding each <event> and returning the document's root
// element too.
func decodeXE(b []byte) ([]XEvent, *xml.StartElement, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var root *xml.StartElement
	var out []XEvent
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, root, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("gosmo: decode event XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if root == nil {
			root = new(se.Copy())
		}
		if se.Name.Local != "event" {
			continue
		}
		var raw xmlXEEvent
		if err := dec.DecodeElement(&raw, &se); err != nil {
			return nil, nil, fmt.Errorf("gosmo: decode event XML: %w", err)
		}
		ev, err := raw.decode()
		if err != nil {
			return nil, nil, fmt.Errorf("gosmo: decode event XML: %w", err)
		}
		out = append(out, ev)
	}
}

// -- event_file -----------------------------------------------------------------------

// EventFileCursor is a position in an event_file target's files: the file and
// buffer offset of the last events read. The zero value is the start.
type EventFileCursor struct {
	File   string // full path, as fn_xe_file_target_read_file reports it
	Offset int64
}

// IsZero reports whether the cursor is at the start.
func (c EventFileCursor) IsZero() bool { return c.File == "" }

// ReadEventFile reads the events of the .xel files matching pathPattern that
// come after cursor, and returns them with the cursor to pass next time.
//
// pathPattern is what fn_xe_file_target_read_file takes: a path with a
// wildcard (C:\x\s*.xel), relative to the error-log directory when it has no
// directory, or a blob URL on Azure. EventSession.EventFilePattern builds it
// from a session.
//
// The server skips every event of the cursor's buffer, not just those before
// the offset, and a buffer is written whole, so a cursor always lands between
// buffers and nothing is read twice or missed. max > 0 caps the events read —
// the first read of a large file set is otherwise all of it — and is honoured
// at a buffer boundary, so a read may return a little more than max, never a
// partial buffer the next read would skip.
//
// A cursor naming a file that rollover has since deleted is refused by the
// server, and the error is ErrEventFileGone (errors.Is); start again from the
// zero cursor, which reads from the oldest file still there.
func (s *Server) ReadEventFile(ctx context.Context, pathPattern string, cursor EventFileCursor, max int) ([]XEvent, EventFileCursor, error) {
	what := fmt.Sprintf("read event file %q", pathPattern)
	var file, offset any // NULL, NULL for the start
	if !cursor.IsZero() {
		file, offset = cursor.File, cursor.Offset
	}
	rows, err := s.query(ctx, `
SELECT file_name, file_offset, event_data
FROM   sys.fn_xe_file_target_read_file(@p1, NULL, @p2, @p3)`, pathPattern, file, offset)
	if err != nil {
		return nil, cursor, eventFileReadError(what, cursor, err)
	}
	defer rows.Close()

	var out, pending []XEvent
	next := cursor
	var bufFile string
	var bufOffset int64 = -1
	for rows.Next() {
		var f string
		var off int64
		var data sql.NullString
		if err := rows.Scan(&f, &off, &data); err != nil {
			return nil, cursor, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		if f != bufFile || off != bufOffset {
			// A new buffer: the previous one is complete.
			if bufOffset >= 0 {
				out = append(out, pending...)
				next = EventFileCursor{File: bufFile, Offset: bufOffset}
				pending = pending[:0]
				if max > 0 && len(out) >= max {
					return out, next, nil
				}
			}
			bufFile, bufOffset = f, off
		}
		evs, err := DecodeEventXML([]byte(data.String))
		if err != nil {
			return nil, cursor, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		pending = append(pending, evs...)
	}
	if err := rows.Err(); err != nil {
		return nil, cursor, eventFileReadError(what, cursor, err)
	}
	if bufOffset >= 0 {
		out = append(out, pending...)
		next = EventFileCursor{File: bufFile, Offset: bufOffset}
	}
	return out, next, nil
}

// ErrEventFileGone is ReadEventFile refusing a cursor whose file is no longer
// there — rollover deleted it (max_rollover_files) between two reads, or it
// was deleted by hand.
var ErrEventFileGone = errors.New("the event file the cursor points into no longer exists")

// msgXEInvalidOffset is the server's refusal of a cursor: "The offset %d is
// invalid for log file %s" — raised for an offset that is not a buffer
// boundary too, which a cursor ReadEventFile returned never is, so for gosmo's
// cursors it means the file is gone (verified on major 13: a missing file
// with a valid offset is refused with it).
const msgXEInvalidOffset = 25722

// msgXEFileNotFound is the other form of the same refusal, seen on major 17:
// "The operating system returned error 2 ... while reading from the file".
// The server reports the error once rows are being read, not at the query.
const msgXEFileNotFound = 25717

// eventFileReadError wraps a failed read, naming ErrEventFileGone when the
// server refused a cursor whose file is not there.
func eventFileReadError(what string, cursor EventFileCursor, err error) error {
	if se, ok := AsSQLError(err); ok && !cursor.IsZero() &&
		(se.Number == msgXEInvalidOffset || se.Number == msgXEFileNotFound) {
		return fmt.Errorf("gosmo: %s: %w: %w", what, ErrEventFileGone, err)
	}
	return fmt.Errorf("gosmo: %s: %w", what, err)
}

// EventFiles lists the .xel files on the server that pathPattern matches —
// ReadEventFile's pattern, EventFilePattern's form — oldest first, which is
// the order the server reads them in (the rollover number in each name grows
// with time). A relative pattern is resolved against the error-log directory,
// as the server resolves the target's filename.
//
// It lists through EnumFileSystem, so its caveats hold: on 2016 xp_dirtree
// answers a login that is not sysadmin with no rows, and a blob URL (Azure)
// cannot be listed at all — ErrUnsupported. An empty result therefore means
// "nothing known", not "no files"; a caller reading the files falls back to
// ReadEventFile with the pattern.
func (s *Server) EventFiles(ctx context.Context, pathPattern string) ([]string, error) {
	what := fmt.Sprintf("list event files %q", pathPattern)
	if strings.Contains(pathPattern, "://") {
		return nil, unsupportedf("gosmo: %s: a URL cannot be listed", what)
	}
	dir, glob := splitServerPath(pathPattern)
	if dir == "" {
		var errorLog sql.NullString
		if err := s.queryRowScan(ctx, "SELECT CAST(SERVERPROPERTY('ErrorLogFileName') AS nvarchar(4000))", nil, &errorLog); err != nil {
			return nil, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		dir, _ = splitServerPath(errorLog.String)
		if dir == "" {
			return nil, fmt.Errorf("gosmo: %s: the server reports no error-log directory", what)
		}
	}
	entries, err := s.EnumFileSystem(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDirectory && matchWildcardFold(glob, e.Name) {
			out = append(out, e.FullPath)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return out, nil
}

// splitServerPath splits a server-side path at its last separator, / or \
// (the server's convention is not the client's): "C:\x\s*.xel" is "C:\x",
// "s*.xel"; a bare name has no directory.
func splitServerPath(p string) (dir, name string) {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// matchWildcardFold matches name against a pattern whose only wildcard is *,
// ignoring case — how the file function matches its pattern on Windows. A
// Linux server matches case-sensitively, but the pattern comes from the
// target's own filename, so the case agrees there anyway.
func matchWildcardFold(pattern, name string) bool {
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(name, p)
		if i < 0 {
			return false
		}
		name = name[i+len(p):]
	}
	return strings.HasSuffix(name, last)
}

// EventFilePattern returns the pattern ReadEventFile takes for the session's
// event_file target: its filename with the extension replaced by *.xel, which
// matches the rollover files the server names <stem>_0_<n>.xel. A relative
// filename stays relative — the server resolves both the target's and the
// read's against the error-log directory. It needs the session's targets, so
// a Ref handle has none; read the session with EventSessionByName first.
func (es *EventSession) EventFilePattern() (string, error) {
	t, ok := es.Target(XETargetEventFile)
	if !ok {
		return "", fmt.Errorf("gosmo: event session %q has no event_file target", es.Name)
	}
	name, ok := t.Field("filename")
	if !ok || name == "" {
		return "", fmt.Errorf("gosmo: event session %q: event_file target has no filename", es.Name)
	}
	if strings.EqualFold(name[max(0, len(name)-4):], ".xel") {
		name = name[:len(name)-4]
	}
	return name + "*.xel", nil
}

// -- ring_buffer ------------------------------------------------------------------------

// ReadRingBuffer reads the session's ring_buffer target in full. There is no
// cursor: each read returns the whole buffer, and a poller tells new events
// from old by Seq (the event_sequence action) where the session collects it.
// A session that is not running, or has no ring_buffer, is a not-found error
// (errors.Is ErrNotFound).
func (es *EventSession) ReadRingBuffer(ctx context.Context) (*RingBufferData, error) {
	sc := es.scope()
	var data sql.NullString
	err := func() error {
		rows, err := es.query(ctx, fmt.Sprintf(`
SELECT t.target_data
FROM   %s s
JOIN   %s t ON t.event_session_address = s.address
WHERE  s.name = @p1 AND t.target_name = N'ring_buffer'`, sc.dmSessions, sc.dmTargets), es.Name)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		if err := rows.Scan(&data); err != nil {
			return err
		}
		return rows.Err()
	}()
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: event session %q has no running ring_buffer target", es.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read event session %q ring buffer: %w", es.Name, err)
	}
	rb, err := DecodeRingBuffer([]byte(data.String))
	if err != nil {
		return nil, fmt.Errorf("gosmo: read event session %q ring buffer: %w", es.Name, err)
	}
	return rb, nil
}
