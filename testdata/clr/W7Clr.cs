// Source of w7clr.dll, the SAFE assembly live_clr_function_test.go loads to
// get a CLR scalar (Twice) and table-valued (Seq) function. Built with the
// .NET Framework 4 compiler every Windows SQL Server host has:
//   csc.exe /nologo /target:library /out:w7clr.dll W7Clr.cs
using System.Collections;
using System.Data.SqlTypes;
using Microsoft.SqlServer.Server;
public static class W7Clr {
  [SqlFunction] public static SqlInt32 Twice(SqlInt32 x) { return x * 2; }
  [SqlFunction(FillRowMethodName = "Fill", TableDefinition = "n int")]
  public static IEnumerable Seq(SqlInt32 c) { ArrayList l = new ArrayList(); for (int i = 0; i < c.Value; i++) l.Add(i); return l; }
  public static void Fill(object o, out SqlInt32 n) { n = (int)o; }
}
