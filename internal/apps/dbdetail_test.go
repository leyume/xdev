package apps

import (
	"reflect"
	"testing"
)

func TestParseDescribe(t *testing.T) {
	all := "P\t'laravel'@'%'\t"
	out := "S\tlaravel\n" +
		"T\tusers\t42\t0.30\n" +
		"T\tposts\t8\t1.20\n" +
		"C\tusers\tid\tbigint(20) unsigned\tNO\tPRI\t\tauto_increment\n" +
		"C\tusers\temail\tvarchar(255)\tNO\tUNI\t\t\n" +
		"C\tposts\ttitle\tvarchar(255)\tYES\t\tNULL\t\n" +
		"P\t'reader'@'%'\tSELECT\n" +
		"G\t'root'@'localhost'\tALL\n" +
		"G\t'root'@'%'\tALL\n"
	for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "REFERENCES", "INDEX",
		"ALTER", "CREATE TEMPORARY TABLES", "LOCK TABLES", "EXECUTE", "CREATE VIEW", "SHOW VIEW",
		"CREATE ROUTINE", "ALTER ROUTINE", "EVENT", "TRIGGER"} {
		out += all + p + "\n"
	}
	d := SharedDBDetail{Name: "laravel"}
	parseDescribe(out, &d)

	if !d.Found || len(d.Tables) != 2 || d.TotalRows != 50 || d.SizeMB != "1.5" {
		t.Fatalf("summary wrong: found=%v tables=%d rows=%d size=%s", d.Found, len(d.Tables), d.TotalRows, d.SizeMB)
	}
	if got := d.Tables[0].Columns; len(got) != 2 || got[0] != (DBColumn{"id", "bigint(20) unsigned", "NO", "PRI", "", "auto_increment"}) {
		t.Errorf("users columns = %+v", got)
	}
	if len(d.Tables[1].Columns) != 1 {
		t.Errorf("posts columns = %+v", d.Tables[1].Columns)
	}
	want := []DBGrantee{
		{User: "reader", Host: "%", Privs: []string{"SELECT"}},
		{User: "laravel", Host: "%", Privs: []string{"ALL"}},
		{User: "root", Host: "localhost, %", Privs: []string{"ALL"}, Global: true},
	}
	if !reflect.DeepEqual(d.Users, want) {
		t.Errorf("users = %+v\nwant %+v", d.Users, want)
	}
}
