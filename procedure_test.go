package gosmo

import (
	"database/sql"
	"testing"
)

func namedArg(t *testing.T, p ProcParam) sql.NamedArg {
	t.Helper()
	a, ok := p.arg().(sql.NamedArg)
	if !ok {
		t.Fatalf("arg() = %T, want sql.NamedArg", p.arg())
	}
	return a
}

func TestProcParamIn(t *testing.T) {
	a := namedArg(t, In("id", 42))
	if a.Name != "id" {
		t.Errorf("Name = %q, want id", a.Name)
	}
	if a.Value != 42 {
		t.Errorf("Value = %v, want 42", a.Value)
	}
}

func TestProcParamOut(t *testing.T) {
	var dest string
	a := namedArg(t, Out("result", &dest))
	out, ok := a.Value.(sql.Out)
	if !ok {
		t.Fatalf("Value = %T, want sql.Out", a.Value)
	}
	if out.Dest != &dest {
		t.Error("Out.Dest should be the caller's pointer")
	}
	if out.In {
		t.Error("Out.In should be false for a pure output parameter")
	}
}

func TestProcParamInOut(t *testing.T) {
	dest := 7
	a := namedArg(t, InOut("counter", &dest))
	out, ok := a.Value.(sql.Out)
	if !ok {
		t.Fatalf("Value = %T, want sql.Out", a.Value)
	}
	if out.Dest != &dest {
		t.Error("InOut.Dest should be the caller's pointer")
	}
	if !out.In {
		t.Error("InOut.In should be true so the current value is sent as input")
	}
}

// The name guard runs before any connection is acquired.
func TestExecProcRejectsEmptyName(t *testing.T) {
	d := &Database{}
	if _, err := d.ExecProc("dbo", ""); err == nil {
		t.Fatal("want error when procedure name is empty, got nil")
	}
}
