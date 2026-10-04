package gbase

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"reflect"
	"testing"
)

func TestSQLiteDifferential(t *testing.T) {
	sqlite, e := exec.LookPath("sqlite3")
	if e != nil {
		t.Skip("sqlite3 CLI unavailable")
	}
	setup := []string{"CREATE TABLE teams (id INTEGER PRIMARY KEY, name TEXT)", "CREATE TABLE people (id INTEGER PRIMARY KEY, team INTEGER, score INTEGER)", "INSERT INTO teams VALUES (1,'a'),(2,'b'),(3,'c')", "INSERT INTO people VALUES (1,1,10),(2,1,20),(3,2,NULL)", "CREATE INDEX by_team ON people(team)"}
	db := openTest(t)
	script := ""
	for _, s := range setup {
		execTest(t, db, s)
		script += s + ";"
	}
	queries := []string{"SELECT id,score FROM people WHERE team=1 ORDER BY id", "SELECT t.name,COUNT(p.id) AS n,SUM(p.score) AS total FROM teams t LEFT JOIN people p ON p.team=t.id GROUP BY t.name ORDER BY t.name", "SELECT DISTINCT team FROM people ORDER BY team DESC LIMIT 1 OFFSET 1", "SELECT team,AVG(score) AS avg FROM people GROUP BY team HAVING COUNT(*)>1", "SELECT NULL=1 AS a,NULL IS NULL AS b,0 AND NULL AS c,1 OR NULL AS d", "SELECT COUNT(*) AS n,SUM(score) AS s FROM people WHERE id<0", "SELECT name FROM teams WHERE name LIKE 'A%' OR name LIKE '_' ORDER BY id", "SELECT id FROM people WHERE score IN (10,NULL) OR team NOT IN (1) ORDER BY id", "SELECT id FROM people WHERE score BETWEEN 5 AND 15 OR id NOT BETWEEN 1 AND 2 ORDER BY id", "SELECT 2 IN (1,NULL) AS a,1 IN (1,NULL) AS b,NULL BETWEEN 1 AND 2 AS c,'a%' LIKE 'a!%' ESCAPE '!' AS d", "SELECT team,SUM(CASE WHEN score>10 THEN 1 ELSE 0 END) AS hi,CASE team WHEN 1 THEN 'one' END AS t FROM people GROUP BY team ORDER BY team"}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			out, e := exec.Command(sqlite, "-json", ":memory:", script+q+";").Output()
			if e != nil {
				t.Fatal(e)
			}
			dec := json.NewDecoder(bytes.NewReader(out))
			dec.UseNumber()
			var sqliteRows []map[string]any
			if e = dec.Decode(&sqliteRows); e != nil {
				t.Fatal(e)
			}
			r, e := db.Query(bg, q)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			columns := r.Columns()
			var got []map[string]any
			for r.Next() {
				vals := r.Values()
				obj := map[string]any{}
				for i, c := range columns {
					obj[c] = vals[i]
				}
				got = append(got, obj)
			}
			if e = r.Err(); e != nil {
				t.Fatal(e)
			}
			for _, row := range sqliteRows {
				for k, v := range row {
					if n, ok := v.(json.Number); ok {
						if i, e := n.Int64(); e == nil {
							row[k] = i
						} else {
							f, _ := n.Float64()
							row[k] = f
						}
					}
				}
			}
			if !reflect.DeepEqual(got, sqliteRows) {
				t.Fatalf("gbase %#v sqlite %#v", got, sqliteRows)
			}
		})
	}
}
