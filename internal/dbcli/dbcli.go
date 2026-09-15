// Package dbcli implements the `goacos db` subcommands used by deploy scripts
// to discover, verify and initialize a matching MySQL or PostgreSQL server.
package dbcli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/halfking/goacos/internal/storage"
)

// Candidate is one discovered database server.
type Candidate struct {
	Type         string `json:"type"` // mysql | postgres
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Source       string `json:"source"`
	User         string `json:"user,omitempty"`
	Password     string `json:"password,omitempty"`
	RootPassword string `json:"rootPassword,omitempty"`
	DBHint       string `json:"databaseHint,omitempty"`
	ConnectOK    bool   `json:"connectOk"`
	CanCreateDB  bool   `json:"canCreateDb"`
	Verified     bool   `json:"verified"`
}

// Run dispatches `goacos db <discover|verify|init>`.
func Run(args []string) {
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "discover":
		discoverCmd(args[1:])
	case "verify":
		verifyCmd(args[1:])
	case "init":
		initCmd(args[1:])
	default:
		usage()
	}
}

func usage() {
	fmt.Println(`goacos db — MySQL/PostgreSQL discovery & initialization for deploy scripts

Usage:
  goacos db discover [--db-type mysql|postgres] [--cidr 10.0.0.0/24] [--json] [--best]
      Find database servers via Docker containers, localhost and (optionally)
      a subnet scan (MySQL :3306, PostgreSQL :5432); probe common credentials;
      print candidates. --db-type restricts discovery to one engine.
  goacos db verify --db-type mysql|postgres --host H --port P --user U --password W [--db NAME]
      Check connectivity (and CREATE DATABASE privilege when --db given).
  goacos db init --db-type mysql|postgres --host H --port P --user U --password W --db NAME
      Create the database if missing and apply the embedded schema + seed.`)
}

func parseKV(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		if eq := strings.Index(a, "="); eq > 0 {
			out[a[2:eq]] = a[eq+1:]
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[a[2:]] = args[i+1]
			i++
		} else {
			out[a[2:]] = "true"
		}
	}
	return out
}

// dialectOf resolves a --db-type flag value (default mysql).
func dialectOf(kv map[string]string) storage.Dialect {
	return storage.DialectFor(kv["db-type"], "")
}

func defaultPortFor(d storage.Dialect) int {
	if d == storage.DialectPostgres {
		return 5432
	}
	return 3306
}

func discoverCmd(args []string) {
	kv := parseKV(args)
	// three-state engine filter: only an explicit --db-type mysql|postgres
	// restricts discovery; absent/`auto` discovers BOTH engines.
	wantStr := strings.ToLower(strings.TrimSpace(kv["db-type"]))
	wantSet := wantStr != "" && wantStr != "auto"
	var want storage.Dialect
	if wantSet {
		want = storage.DialectFor(wantStr, "")
	}
	cands := dockerCandidates()
	cands = append(cands, Candidate{Type: "mysql", Host: "127.0.0.1", Port: 3306, Source: "localhost"})
	cands = append(cands, Candidate{Type: "postgres", Host: "127.0.0.1", Port: 5432, Source: "localhost"})
	if v, ok := kv["host"]; ok && v != "" {
		flagD := storage.DialectMySQL
		if wantSet {
			flagD = want
		}
		cands = append(cands, Candidate{Type: string(flagD), Host: v, Port: portOf(kv, defaultPortFor(flagD)), Source: "flag"})
	}
	if cidr, ok := kv["cidr"]; ok && cidr != "false" {
		cands = append(cands, scanCIDR(cidr)...)
	}
	// TCP reachability prefilter (+ explicit --db-type filter)
	alive := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if wantSet && storage.Dialect(c.Type) != want {
			continue
		}
		if tcpOK(c.Host, c.Port, 600*time.Millisecond) {
			alive = append(alive, c)
		}
	}
	// credential probing
	verified := make([]Candidate, 0, len(alive))
	for _, c := range alive {
		d := storage.Dialect(c.Type)
		for _, cred := range credList(c) {
			ok, canCreate := probe(d, c.Host, c.Port, cred.user, cred.pass)
			if ok {
				c.User, c.Password = cred.user, cred.pass
				c.ConnectOK, c.CanCreateDB, c.Verified = true, canCreate, true
				verified = append(verified, c)
				break
			}
		}
		if !c.Verified {
			c.ConnectOK = true
			verified = append(verified, c)
		}
	}
	// best = verified + can create db, docker source first
	var best *Candidate
	for i := range verified {
		c := &verified[i]
		if !c.Verified || !c.CanCreateDB {
			continue
		}
		if best == nil || rank(c) > rank(best) {
			best = c
		}
	}
	if kv["best"] == "true" {
		if best == nil {
			fmt.Fprintln(osWriter(), "NOMATCH")
			osExit(3)
		}
		enc := json.NewEncoder(osWriter())
		enc.SetIndent("", "  ")
		_ = enc.Encode(best)
		return
	}
	enc := json.NewEncoder(osWriter())
	enc.SetIndent("", "  ")
	_ = enc.Encode(verified)
}

func rank(c *Candidate) int {
	r := 0
	if c.Source == "docker" {
		r += 2
	}
	if c.RootPassword != "" {
		r++
	}
	return r
}

type cred struct{ user, pass string }

func credList(c Candidate) []cred {
	var out []cred
	add := func(u, p string) {
		for _, e := range out {
			if e.user == u && e.pass == p {
				return
			}
		}
		out = append(out, cred{u, p})
	}
	isPG := storage.Dialect(c.Type) == storage.DialectPostgres
	super := "root"
	if isPG {
		super = "postgres"
		if c.User != "" {
			super = c.User
		}
	}
	if c.RootPassword != "" {
		add(super, c.RootPassword)
	}
	if c.User != "" && c.Password != "" {
		add(c.User, c.Password)
	}
	if isPG {
		add("postgres", "postgres")
		add("postgres", "password")
		add("postgres", "123456")
		add("postgres", "")
	} else {
		add("root", "root")
		add("root", "123456")
		add("root", "password")
		add("mysql", "mysql")
		add("root", "")
	}
	return out
}

func dockerCandidates() []Candidate {
	var out []Candidate
	docker, err := exec.LookPath("docker")
	if err != nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ps, err := exec.CommandContext(ctx, docker, "ps", "--format", "{{.ID}}\t{{.Image}}").Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(ps), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		img := strings.ToLower(parts[1])
		var ctype storage.Dialect
		switch {
		case strings.Contains(img, "mysql") || strings.Contains(img, "mariadb"):
			ctype = storage.DialectMySQL
		case strings.Contains(img, "postgres") || strings.Contains(img, "pgvector"):
			ctype = storage.DialectPostgres
		default:
			continue
		}
		insp, err := exec.CommandContext(ctx, docker, "inspect", parts[0]).Output()
		if err != nil {
			continue
		}
		var items []struct {
			Name   string `json:"Name"`
			Config struct {
				Env []string `json:"Env"`
			} `json:"Config"`
			NetworkSettings struct {
				Ports map[string][]struct {
					HostIP   string `json:"HostIp"`
					HostPort string `json:"HostPort"`
				} `json:"Ports"`
				Networks map[string]struct {
					IPAddress string `json:"IPAddress"`
				} `json:"Networks"`
			} `json:"NetworkSettings"`
		}
		if err := json.Unmarshal(insp, &items); err != nil || len(items) == 0 {
			continue
		}
		it := items[0]
		c := Candidate{Type: string(ctype), Source: "docker"}
		for _, e := range it.Config.Env {
			switch {
			case strings.HasPrefix(e, "MYSQL_ROOT_PASSWORD="):
				c.RootPassword = strings.TrimPrefix(e, "MYSQL_ROOT_PASSWORD=")
			case strings.HasPrefix(e, "MYSQL_PASSWORD="):
				c.Password = strings.TrimPrefix(e, "MYSQL_PASSWORD=")
			case strings.HasPrefix(e, "MYSQL_USER="):
				c.User = strings.TrimPrefix(e, "MYSQL_USER=")
			case strings.HasPrefix(e, "MYSQL_DATABASE="):
				c.DBHint = strings.TrimPrefix(e, "MYSQL_DATABASE=")
			case strings.HasPrefix(e, "POSTGRES_PASSWORD="):
				c.RootPassword = strings.TrimPrefix(e, "POSTGRES_PASSWORD=")
			case strings.HasPrefix(e, "POSTGRES_USER="):
				c.User = strings.TrimPrefix(e, "POSTGRES_USER=")
			case strings.HasPrefix(e, "POSTGRES_DB="):
				c.DBHint = strings.TrimPrefix(e, "POSTGRES_DB=")
			}
		}
		// prefer published host ports (works on macOS Docker Desktop)
		found := false
		for _, bindings := range it.NetworkSettings.Ports {
			if found {
				break
			}
			for _, b := range bindings {
				if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
					host := b.HostIP
					if host == "" || host == "0.0.0.0" || host == "::" {
						host = "127.0.0.1"
					}
					c.Host, c.Port = host, p
					out = append(out, c)
					found = true
					break // one reachable mapping per container is enough
				}
			}
		}
		if found {
			continue
		}
		// fall back to container IPs (works on Linux hosts)
		for _, nw := range it.NetworkSettings.Networks {
			if nw.IPAddress != "" {
				c.Host, c.Port = nw.IPAddress, defaultPortFor(ctype)
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func scanCIDR(cidr string) []Candidate {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	var mu sync.Mutex
	var out []Candidate
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	base := ip.Mask(ipnet.Mask).To4()
	ports := []struct {
		port  int
		ctype storage.Dialect
	}{
		{3306, storage.DialectMySQL},
		{5432, storage.DialectPostgres},
	}
	for i := 1; i < 255; i++ {
		host := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], base[3]+byte(i))
		for _, p := range ports {
			wg.Add(1)
			sem <- struct{}{}
			go func(h string, port int, ctype storage.Dialect) {
				defer wg.Done()
				defer func() { <-sem }()
				if tcpOK(h, port, 500*time.Millisecond) {
					mu.Lock()
					out = append(out, Candidate{Type: string(ctype), Host: h, Port: port, Source: "scan"})
					mu.Unlock()
				}
			}(host, p.port, p.ctype)
		}
	}
	wg.Wait()
	return out
}

func tcpOK(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func probe(d storage.Dialect, host string, port int, user, pass string) (connectOK, canCreate bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	db, err := sql.Open(d.DriverName(), storage.DSNFor(d, host, port, user, pass, d.ServerDB()))
	if err != nil {
		return false, false
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return false, false
	}
	connectOK = true
	probeDB := fmt.Sprintf("goacos_probe_%d", time.Now().UnixNano()%100000)
	if d == storage.DialectPostgres {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %q`, probeDB)); err == nil {
			canCreate = true
			_, _ = db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE %q`, probeDB))
		}
	} else {
		if _, err := db.ExecContext(ctx, "CREATE DATABASE `"+probeDB+"`"); err == nil {
			canCreate = true
			_, _ = db.ExecContext(ctx, "DROP DATABASE `"+probeDB+"`")
		}
	}
	return connectOK, canCreate
}

func verifyCmd(args []string) {
	kv := parseKV(args)
	host := kv["host"]
	if host == "" {
		fmt.Fprintln(osWriter(), "--host required")
		osExit(2)
	}
	d := dialectOf(kv)
	user, pass := kv["user"], kv["password"]
	ok, canCreate := probe(d, host, portOf(kv, defaultPortFor(d)), user, pass)
	if !ok {
		fmt.Fprintln(osWriter(), "VERIFY FAIL: cannot connect")
		osExit(1)
	}
	if kv["db"] != "" && !canCreate {
		fmt.Fprintln(osWriter(), "VERIFY FAIL: no CREATE DATABASE privilege")
		osExit(1)
	}
	fmt.Fprintln(osWriter(), "VERIFY OK (canCreateDb="+strconv.FormatBool(canCreate)+")")
}

func initCmd(args []string) {
	kv := parseKV(args)
	host, name := kv["host"], kv["db"]
	if host == "" || name == "" {
		fmt.Fprintln(osWriter(), "--host and --db required")
		osExit(2)
	}
	d := dialectOf(kv)
	cfg := testConfig(d, host, portOf(kv, defaultPortFor(d)), kv["user"], kv["password"], name)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := storage.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintln(osWriter(), "INIT FAIL:", err)
		osExit(1)
	}
	st.Close()
	fmt.Fprintf(osWriter(), "INIT OK: %s database %q ready (schema+seed applied)\n", d, name)
}

func portOf(kv map[string]string, def int) int {
	if v := kv["port"]; v != "" && v != "true" {
		if p, err := strconv.Atoi(v); err == nil {
			return p
		}
	}
	return def
}
