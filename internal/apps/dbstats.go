package apps

import (
	"context"
	"strconv"
	"strings"
	"time"

	"xdev/internal/runtime"
)

// SharedDBStats is the shared server's own view of itself — the counters
// MariaDB keeps that a container's CPU/memory cannot show: how many clients are
// attached, how hard the buffer pool is working, whether queries are being
// refused or running slow.
//
// Every field is a rendered string rather than a number, because the page shows
// each one as-is and an unavailable server has to render as "—" rather than as
// a misleading zero. Available reports whether the server answered at all.
type SharedDBStats struct {
	Available bool

	Version    string
	Uptime     string // humanised, e.g. "3d 4h"
	Threads    string // currently open client connections
	Running    string // of those, actively executing a query
	MaxUsed    string // high-water mark since start
	MaxConns   string // configured ceiling
	Questions  string // statements executed since start
	QPS        string // Questions / Uptime — a start-to-now average, not "now"
	SlowLog    string // queries over long_query_time
	Aborted    string // connections that failed to authenticate or dropped
	PoolSize   string // innodb_buffer_pool_size, humanised
	PoolUsed   string // resident data in the pool, humanised
	PoolPct    string // PoolUsed as a percentage of PoolSize
	TableCount string // user tables across every schema
}

// SharedDBStats queries the shared server's global counters. A server that is
// down or unconfigured yields Available=false and no error — the page renders
// that as "—" rather than as a failure, matching SharedDBInfo's behaviour.
func (s *Service) SharedDBStats(ctx context.Context) SharedDBStats {
	var st SharedDBStats
	engine := s.sel.Current()
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		return st
	}
	if !containerRunning(ctx, engine, sharedDBContainer) {
		return st
	}

	// One round trip for all of it: status counters, the two variables we care
	// about, and the table count, each row already in name<TAB>value shape.
	const q = `SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS
	    WHERE VARIABLE_NAME IN ('UPTIME','THREADS_CONNECTED','THREADS_RUNNING',
	      'MAX_USED_CONNECTIONS','QUESTIONS','SLOW_QUERIES','ABORTED_CONNECTS',
	      'INNODB_BUFFER_POOL_BYTES_DATA')
	  UNION ALL
	  SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_VARIABLES
	    WHERE VARIABLE_NAME IN ('VERSION','MAX_CONNECTIONS','INNODB_BUFFER_POOL_SIZE')
	  UNION ALL
	  SELECT 'TABLE_COUNT', COUNT(*) FROM information_schema.TABLES
	    WHERE TABLE_SCHEMA NOT IN ('information_schema','performance_schema','mysql','sys');`

	out, err := runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb", "-uroot", "-N", "-B", "-e", q)
	if err != nil {
		return st
	}

	v := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) == 2 {
			v[strings.ToUpper(strings.TrimSpace(f[0]))] = strings.TrimSpace(f[1])
		}
	}
	if len(v) == 0 {
		return st
	}

	st.Available = true
	st.Version = v["VERSION"]
	st.Threads = v["THREADS_CONNECTED"]
	st.Running = v["THREADS_RUNNING"]
	st.MaxUsed = v["MAX_USED_CONNECTIONS"]
	st.MaxConns = v["MAX_CONNECTIONS"]
	st.Questions = v["QUESTIONS"]
	st.SlowLog = v["SLOW_QUERIES"]
	st.Aborted = v["ABORTED_CONNECTS"]
	st.TableCount = v["TABLE_COUNT"]

	uptime, _ := strconv.ParseInt(v["UPTIME"], 10, 64)
	st.Uptime = humanizeUptime(uptime)
	if q, e := strconv.ParseFloat(v["QUESTIONS"], 64); e == nil && uptime > 0 {
		st.QPS = strconv.FormatFloat(q/float64(uptime), 'f', 1, 64)
	}

	poolSize, _ := strconv.ParseFloat(v["INNODB_BUFFER_POOL_SIZE"], 64)
	poolUsed, _ := strconv.ParseFloat(v["INNODB_BUFFER_POOL_BYTES_DATA"], 64)
	st.PoolSize = humanizeBytes(poolSize)
	st.PoolUsed = humanizeBytes(poolUsed)
	if poolSize > 0 {
		st.PoolPct = strconv.FormatFloat(poolUsed/poolSize*100, 'f', 0, 64)
	}
	return st
}

// humanizeUptime renders a duration in seconds as the one or two units that
// carry meaning ("3d 4h", "12m"), never as a bare pile of seconds.
func humanizeUptime(secs int64) string {
	if secs <= 0 {
		return ""
	}
	d := time.Duration(secs) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return strconv.Itoa(days) + "d " + strconv.Itoa(hours) + "h"
	case hours > 0:
		return strconv.Itoa(hours) + "h " + strconv.Itoa(mins) + "m"
	case mins > 0:
		return strconv.Itoa(mins) + "m"
	default:
		return strconv.FormatInt(secs, 10) + "s"
	}
}

// humanizeBytes renders a byte count in the largest unit that keeps it under
// 1024, one decimal place. Zero renders empty so the page shows "—".
func humanizeBytes(b float64) string {
	if b <= 0 {
		return ""
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	prec := 1
	if i == 0 {
		prec = 0
	}
	return strconv.FormatFloat(b, 'f', prec, 64) + " " + units[i]
}
