// Source of w7clr.dll, the SAFE assembly live_clr_function_test.go loads to
// get a CLR scalar (Twice) and table-valued (Seq; SeqS, one string column)
// function, a procedure with an OUTPUT parameter (Echo), a procedure taking
// one parameter of each type a default can have (Kinds) and a trigger (Noop)
// that binds as a DML or a DDL trigger. Built with the .NET Framework 4 compiler every Windows SQL
// Server host has:
//   csc.exe /nologo /target:library /out:w7clr.dll W7Clr.cs
using System;
using System.Collections;
using System.Data.SqlTypes;
using Microsoft.SqlServer.Server;
public static class W7Clr {
  [SqlFunction] public static SqlInt32 Twice(SqlInt32 x) { return x * 2; }
  [SqlFunction(FillRowMethodName = "Fill", TableDefinition = "n int")]
  public static IEnumerable Seq(SqlInt32 c) { ArrayList l = new ArrayList(); for (int i = 0; i < c.Value; i++) l.Add(i); return l; }
  public static void Fill(object o, out SqlInt32 n) { n = (int)o; }
  [SqlFunction(FillRowMethodName = "FillS", TableDefinition = "s nvarchar(20)")]
  public static IEnumerable SeqS(SqlInt32 c) { return Seq(c); }
  public static void FillS(object o, out SqlString s) { s = o.ToString(); }
  [SqlProcedure] public static void Echo(SqlInt32 x, SqlString s, out SqlInt32 y) { y = x * 2; }
  [SqlProcedure] public static void Kinds(SqlDouble f, SqlSingle r, SqlMoney m, SqlMoney sm,
    SqlDateTime dt, SqlDateTime sdt, DateTime d, TimeSpan t, DateTime dt2, DateTimeOffset dto,
    SqlBinary vb, SqlBinary b, SqlGuid g, SqlBoolean bit, SqlDecimal dec, SqlInt64 big,
    SqlByte tiny, SqlInt16 small, SqlString nc) { }
  [SqlTrigger] public static void Noop() { }
}
