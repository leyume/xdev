package apps

import (
	"reflect"
	"strings"
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

func TestParseDBEnv(t *testing.T) {
	out := "PATH=/usr/bin\nMARIADB_VERSION=1:11.4\nMYSQL_ROOT_PASSWORD=root\nMYSQL_USER=laravel\nMYSQL_PASSWORD=a=b\nMYSQL_DATABASE=laravel\n"
	env := parseDBEnv(out)
	want := []DBEnvVar{
		{"MYSQL_DATABASE", "laravel", false},
		{"MYSQL_USER", "laravel", false},
		{"MYSQL_PASSWORD", "a=b", true},
		{"MYSQL_ROOT_PASSWORD", "root", true},
	}
	if len(env) != len(want) {
		t.Fatalf("got %+v", env)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %+v, want %+v", i, env[i], want[i])
		}
	}
}

func TestComposePasswordRewrite(t *testing.T) {
	yaml := `services:
  app:
    environment:
      DB_PASSWORD: ${DB_PASSWORD:-secret}
  db:
    environment:
      MYSQL_ROOT_PASSWORD: ${DB_ROOT_PASSWORD:-root}
      MYSQL_PASSWORD: ${DB_PASSWORD:-secret}
`
	got := setComposeKeys(yaml, dbUserPassKeys, "n3w-Pass!")
	if strings.Count(got, `: "n3w-Pass!"`) != 2 || !strings.Contains(got, "MYSQL_ROOT_PASSWORD: ${DB_ROOT_PASSWORD:-root}") {
		t.Fatalf("user rewrite:\n%s", got)
	}
	// A regenerated stack keeps the changed user password and the default root one.
	if again := carryDBPasswords(got, yaml); again != got {
		t.Fatalf("carry:\n%s", again)
	}
	if !dbPassRE.MatchString("n3w-Pass!") || dbPassRE.MatchString("short") || dbPassRE.MatchString("has$dollar1") || dbPassRE.MatchString("quote'd1234") {
		t.Fatal("dbPassRE")
	}
}
