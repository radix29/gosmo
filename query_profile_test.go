package gosmo

import (
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestQueryProfilesScanEveryColumn(t *testing.T) {
	row := []driver.Value{int64(1), []byte{0x06, 0x01}, int64(46), int64(-1),
		int64(9), int64(2), "Table Spool",
		int64(10), int64(11), int64(12), int64(13), int64(14),
		int64(15), int64(16), int64(17), int64(18), int64(19), int64(20),
		int64(21), int64(22),
		int64(23), int64(24), int64(25),
		int64(26), int64(27), int64(28), int64(29), int64(30),
		int64(31), int64(32), int64(33),
		int64(34), int64(35), int64(36), int64(37)}
	s, c := activityServer(t, activityReply{key: "dm_exec_query_profiles", cols: cols(len(row)), rows: [][]driver.Value{row}})
	got, err := s.QueryProfiles(t.Context(), 61)
	if err != nil {
		t.Fatal(err)
	}
	want := QueryProfile{RequestID: 1, PlanHandle: []byte{0x06, 0x01}, StatementStart: 46, StatementEnd: -1,
		NodeID: 9, ThreadID: 2, PhysicalOperator: "Table Spool",
		RowCount: 10, EstimateRowCount: 11, RewindCount: 12, RebindCount: 13, EndOfScanCount: 14,
		FirstActiveTime: 15, LastActiveTime: 16, OpenTime: 17, FirstRowTime: 18, LastRowTime: 19, CloseTime: 20,
		ElapsedMs: 21, CPUMs: 22,
		DatabaseID: 23, ObjectID: 24, IndexID: 25,
		ScanCount: 26, LogicalReads: 27, PhysicalReads: 28, ReadAheads: 29, WritePages: 30,
		LobLogicalReads: 31, LobPhysicalReads: 32, LobReadAheads: 33,
		SegmentReads: 34, SegmentSkips: 35, ActualReadRowCount: 36, EstimatedReadRowCount: 37}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("QueryProfiles = %+v, want [%+v]", got, want)
	}
	// The session goes as a parameter, never formatted into the text.
	if len(c.args) != 1 || c.args[0].Value != int64(61) {
		t.Errorf("args = %v, want the session id alone", c.args)
	}
	if !strings.Contains(c.query, "p.session_id = @p1") {
		t.Errorf("session filter not parameterised:\n%s", c.query)
	}
}

func TestQueryProfilesOfAnIdleSessionAreEmpty(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_exec_query_profiles", cols: cols(35)})
	got, err := s.QueryProfiles(t.Context(), 61)
	if err != nil || len(got) != 0 {
		t.Errorf("QueryProfiles = %v, %v; want none and no error", got, err)
	}
}

func TestInFlightPlanScansEveryColumn(t *testing.T) {
	s, c := activityServer(t, activityReply{key: "dm_exec_query_statistics_xml", cols: cols(5), rows: [][]driver.Value{
		{int64(3), []byte{0x06}, int64(46), int64(362), "<ShowPlanXML/>"},
	}})
	got, err := s.InFlightPlan(t.Context(), 61)
	if err != nil {
		t.Fatal(err)
	}
	want := &InFlightPlan{RequestID: 3, PlanHandle: []byte{0x06}, StatementStart: 46, StatementEnd: 362, XML: "<ShowPlanXML/>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InFlightPlan = %+v, want %+v", got, want)
	}
	if len(c.args) != 1 || c.args[0].Value != int64(61) {
		t.Errorf("args = %v, want the session id alone", c.args)
	}
}

func TestInFlightPlanOfAnIdleSessionIsNotFound(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_exec_query_statistics_xml", cols: cols(5)})
	got, err := s.InFlightPlan(t.Context(), 61)
	if got != nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("InFlightPlan = %v, %v; want nil and ErrNotFound", got, err)
	}
	if err != nil && !strings.Contains(err.Error(), "session 61") {
		t.Errorf("error %q does not name the session", err)
	}
}
