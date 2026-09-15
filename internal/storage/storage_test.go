package storage

import (
	"testing"
)

func TestSplitSQLStatements(t *testing.T) {
	script := `-- comment line
SET NAMES utf8mb4;
CREATE TABLE a (id bigint,
  name varchar(50)
);
-- another comment
CREATE TABLE b (id bigint);`
	stmts := splitSQLStatements(script)
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d: %q", len(stmts), stmts)
	}
	if stmts[0][:len("CREATE TABLE a")] != "CREATE TABLE a" {
		t.Errorf("stmt0 mismatch: %q", stmts[0])
	}
}

func TestMD5Hex(t *testing.T) {
	if got := md5hex("hello"); got != "5d41402abc4b2a76b9719d911017c592" {
		t.Errorf("md5hex wrong: %s", got)
	}
}
