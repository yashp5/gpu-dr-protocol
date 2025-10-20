package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grene-labs/grene/dbutil"
	"github.com/grene-labs/grene/enve"
	"github.com/grene-labs/grene/http/writeresp"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dns struct {
	Domain  string        `json:"domain"`  // Domain name.
	Status  string        `json:"status"`  // DNS status. 'OK' or error
	Latency time.Duration `json:"latency"` // Latency of the last DNS lookup.
}

var (
	healthCheckRes atomic.Pointer[struct {
		healthCheckResp // Pointer to the latest health check response.
		json            json.RawMessage
	}] // Atomic pointer to the latest health check response.
	start = time.Now() // Program start time.
)

type healthCheckResp struct {
	CheckTime    time.Time     `json:"check_time"`    // Time of the health check.
	Overall      string        `json:"overall"`       // Overall health status. 'OK' or error message.
	ProgramStart time.Time     `json:"program_start"` // Start time of the program.
	Uptime       time.Duration `json:"uptime"`        // Uptime of the program.
	DNS          []dns         `json:"dns"`           // DNS health check results.
	Disk         struct {
		//	Total dfstats `json:"total"` // Total disk statistics.
		//	All    []dfstats `json:"all"`    // All disk statistics.
		Status string `json:"status"` // Disk status. 'OK' or error message.
	} `json:"disk"` // Disk health check results.
	DB struct {
		LatencyMS struct {
			Avg, Min, Max, P90 int64
		} `json:"latency_ms"` // Latency statistics in milliseconds.
		Status string                  `json:"status"` // DB status. 'OK' or error message.`
		Stat   dbutil.PoolStatResponse `json:"pool_stat"`
	} `json:"db"`
	Mem struct {
		Go struct {
			Stats  runtime.MemStats `json:"stats"`  // Memory statistics.
			Status string           `json:"status"` // Memory status. 'OK' or error message.
		}
		Free struct {
			Mem    freeStats `json:"mem"`    // Free memory statistics.
			Swap   freeStats `json:"swap"`   // Free swap statistics.
			Total  freeStats `json:"total"`  // Total memory statistics.
			Status string    `json:"status"` // Free memory status. 'OK' or error message.
		}
	} `json:"mem"` // Memory health check results.
	/*CPU struct {
		UsagePercent float64 `json:"usage_percent"` // CPU usage percentage.
		Status       string  // CPU status. 'OK' or error message.
	}*/
}

type pingStatDB interface {
	Ping(ctx context.Context) error // usually a *pgxpool.Pool
	Stat() *pgxpool.Stat
}

func healthcheckHandler(db pingStatDB) http.HandlerFunc {
	// Run health checks every 10 seconds.
	go func() {
		d := enve.DurationOr("DORA_HEALTHCHECK_INTERVAL", 10*time.Second)
		for range time.Tick(d) {

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cacheEntry := new(
				struct {
					healthCheckResp
					json json.RawMessage
				},
			)
			// marshal it exactly once per check ahead of time, so we can just do a straight memcopy in the handler.
			cacheEntry.healthCheckResp = healthcheck(ctx, db)
			cacheEntry.json, _ = json.Marshal(cacheEntry.healthCheckResp)

			healthCheckRes.Store(cacheEntry)

			cancel()
		}
	}()
	return func(w http.ResponseWriter, r *http.Request) {
		resp := healthCheckRes.Load()
		if resp == nil {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("healthcheck not yet available"), http.StatusTooEarly)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var status int
		if resp.Overall != "OK" {
			status = http.StatusInternalServerError // if any check failed, return 500.
		} else {
			status = http.StatusOK // if all checks passed, return 200.
		}
		writeresp.JSONData(r.Context(), w, resp.json, status)
	}
}

func healthcheck(ctx context.Context, db pingStatDB) healthCheckResp {
	var hc healthCheckResp
	wg := new(sync.WaitGroup)
	hc.ProgramStart = start
	hc.Uptime = time.Since(start)
	hc.CheckTime = time.Now()
	log := slog.Default().WithGroup("healthcheck")
	{ // DNS health check
		hc.DNS = make([]dns, 3) // Preallocate for 3 domains.

		for i, domain := range []string{"google.com", "github.com", "facebook.com"} {
			wg.Add(1)
			go func(i int) {
				log.Debug("health: checking DNS", "domain", domain)
				defer wg.Done()
				start := time.Now()
				_, err := net.LookupHost(domain)
				if err != nil {
					hc.DNS[i].Status = fmt.Sprintf("lookup %s: %s", domain, err)
				} else if time.Since(start) > time.Second {
					hc.DNS[i].Status = fmt.Sprintf("lookup %s took too long: %s", domain, time.Since(start))
				} else {
					hc.DNS[i].Status = "OK"
				}
				hc.DNS[i].Domain = domain
				hc.DNS[i].Latency = time.Since(start)
			}(i)
		}
	}

	{ // DB health check
		wg.Add(1)
		go func() {
			hc.DB.Status = "OK"
			log.Debug("health: checking DB")
			defer wg.Done()
			start := time.Now()
			var latency [10]time.Duration
			var sum time.Duration
			var minLatency, maxLatency, p90Latency time.Duration

			// ping the database 10 times to get a basic sample of latency.
			for i := range latency {
				s := time.Now()
				err := db.Ping(ctx)
				if err != nil {
					hc.DB.Status = fmt.Sprintf("ping %d: %s", i+1, err)
				}
				latency[i] = time.Since(s)
				sum += latency[i]
				if i == 0 || latency[i] < minLatency {
					minLatency = latency[i]
				}
				if latency[i] > maxLatency {
					maxLatency, p90Latency = latency[i], maxLatency
				}
			}

			if err := db.Ping(ctx); err != nil {
				hc.DB.Status = fmt.Sprintf("ping: %s", err)
			} else if time.Since(start) > time.Second {
				hc.DB.Status = fmt.Sprintf("ping took too long: %s", time.Since(start))
			}

			hc.DB.LatencyMS.Avg = (sum / 10).Milliseconds()
			hc.DB.LatencyMS.Min = minLatency.Milliseconds()
			hc.DB.LatencyMS.Max = maxLatency.Milliseconds()
			hc.DB.LatencyMS.P90 = p90Latency.Milliseconds()
			hc.DB.Stat = dbutil.PoolStatToResponse(db.Stat())
		}()
	}

	{ // memory usage (go process)
		wg.Add(1)
		go func() {
			log.Debug("health: checking memory usage (go process)")
			defer wg.Done()
			runtime.ReadMemStats(&hc.Mem.Go.Stats)
		}()
	}
	/*{ // memory usage (total system, via 'free')

		wg.Add(1)
		go func() {
			log.Debug("health: checking memory usage (total system)")
			warnIfUsageOver := enve.IntUnitOr("MEM_USAGE_WARN_THRESHOLD", 8*1024*1024*1024)
			errIfUsageOver := enve.IntUnitOr("MEM_USAGE_ERR_THRESHOLD", 16*1024*1024*1024)
			defer wg.Done()
			var err error
			hc.Mem.Free.Mem, hc.Mem.Free.Swap, hc.Mem.Free.Total, err = free(ctx)
			if err != nil {
				hc.Mem.Free.Status = err.Error()
			} else if hc.Mem.Free.Total.Used > uint64(errIfUsageOver) {
				slog.Warn("memory usage is extremely high", "used", hc.Mem.Free.Total.Used, "warnIfUsageOver", warnIfUsageOver, "errIfUsageOver", errIfUsageOver)
				hc.Mem.Free.Status = fmt.Sprintf("ERROR: used %dKiB > %dKiB", hc.Mem.Free.Total.Used/1024, warnIfUsageOver/1024)
			} else if hc.Mem.Free.Total.Used > uint64(warnIfUsageOver) {
				slog.Warn("memory usage is high", "used", hc.Mem.Free.Total.Used, "warnIfUsageOver", warnIfUsageOver)
				hc.Mem.Free.Status = fmt.Sprintf("WARNING: used %dKiB > %dKiB", hc.Mem.Free.Total.Used/1024, warnIfUsageOver/1024)
			} else {
				hc.Mem.Free.Status = "OK"
			}
		}()
	}*/

	/*{ // disk usage
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Debug("health: checking disk usage")
			var err error
			hc.Disk.Total, err = dfTotalOnly(ctx)
			if err != nil {
				hc.Disk.Status = fmt.Sprintf("df: %s", err)
			}
			warnIfUsageOver := enve.FloatOr("DISK_USAGE_WARN_THRESHOLD", .8) // 80%
			errIfUsageOver := enve.FloatOr("DISK_USAGE_ERR_THRESHOLD", .9)   // 90%
			if hc.Disk.Total.PercentUsed > (errIfUsageOver * 100) {
				hc.Disk.Status = fmt.Sprintf("ERROR: disk usage %.2f%% over %.2f%%", hc.Disk.Total.PercentUsed, errIfUsageOver*100)
				log.Error("disk usage is extremely high", "used", hc.Disk.Total.Used, "warnIfUsageOver", warnIfUsageOver, "errIfUsageOver", err)
			}
			if hc.Disk.Total.PercentUsed > (warnIfUsageOver * 100) {
				hc.Disk.Status = fmt.Sprintf("WARNING: disk usage is high: %.2f%% > %.2f%%", hc.Disk.Total.PercentUsed, warnIfUsageOver*100)
				log.Warn("disk usage is high", "used", hc.Disk.Total.Used, "warn_at", warnIfUsageOver, "err_at", errIfUsageOver, "total", hc.Disk.Total.Size)
			}
		}()
	}*/

	{ // CPU usage
		// TODO
	}

	{ // file descriptors and network connections
		// TODO
	}
	wg.Wait()
	var errs []string
	for _, v := range []struct {
		component string
		status    string
	}{
		{"DNS", hc.DNS[0].Status},
		{"DB", hc.DB.Status},
		{"Memory (Go)", hc.Mem.Go.Status},
		{"Memory (Free)", hc.Mem.Free.Status},
		{"Disk", hc.Disk.Status},
	} {
		if v.status != "OK" && v.status != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", v.component, v.status))
		}
	}
	if len(errs) > 0 {
		hc.Overall = strings.Join(errs, ", ")
	} else {
		hc.Overall = "OK"
	}
	return hc
}

type freeStats struct {
	Total     uint64 `json:"total"`      // Total memory in bytes.
	Used      uint64 `json:"used"`       // Used memory in bytes.
	Free      uint64 `json:"free"`       // Free memory in bytes.
	Shared    uint64 `json:"shared"`     // Shared memory in bytes.
	Buff      uint64 `json:"buff/cache"` // buffer/cache memory in bytes.
	Available uint64 `json:"available"`  // Available memory in bytes.
}

/*var freeonce sync.Once

// call out to 'free -b -t' to get memory and swap stats.
func free(ctx context.Context) (mem, swap, total freeStats, err error) {
	switch runtime.GOOS {
	default: // unknown - assume unix-y
		freeonce.Do(func() { slog.Warn("attempting to run 'free' on non-linux os", "runtime.GOOS", runtime.GOOS) })
	case "windows", "darwin":
		return mem, swap, total, fmt.Errorf("free not yet supported on %s", runtime.GOOS)

	case "linux":
		// nop
	}
	const expectedOutput = `total        used        free      shared  buff/cache   available
Mem:     67377426432  2746585088 63083753472    86360064  1547087872 63889125376
Swap:    17179869184           0 17179869184
Total:   84557295616  2746585088 80263622656
`
	b, err := exec.CommandContext(ctx, "free", "-b", "-t").Output() // -b for bytes, -t for totals
	if err != nil {
		return mem, swap, total, fmt.Errorf("failed to run 'free': %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 4 {
		return mem, swap, total, fmt.Errorf("unexpected output from 'free': %s: expected something like %s", string(b), expectedOutput)
	}
	mem, memErr := parseFreeLine(lines[1])
	swap, swapErr := parseFreeLine(lines[2])
	total, totalErr := parseFreeLine(lines[3])
	return mem, swap, total, errors.Join(memErr, swapErr, totalErr)
}

type dfstats struct {
	Filesystem  string  `json:"filesystem"` // Filesystem name.
	Size        uint64  `json:"size"`       // Size in bytes.
	Used        uint64  `json:"used"`       // Used space in bytes.
	Available   uint64  `json:"available"`  // Available space in bytes.
	PercentUsed float64 `json:"percent"`    // Percent used.
	Mount       string  `json:"mount"`      // Mount point.
}

var dfonce sync.Once

func dfTotalOnly(ctx context.Context) (total dfstats, err error) {
	switch runtime.GOOS {
	default: // unknown - assume unix-y
		dfonce.Do(func() { slog.Warn("attempting to run 'df' on non-linux os", "runtime.GOOS", runtime.GOOS) })
	case "windows", "darwin":
		return total, fmt.Errorf("df not yet supported on %s", runtime.GOOS)

	case "linux":
		// nop
	}
	b, err := exec.CommandContext(ctx, "df", "-P", "--total").Output() // -P for POSIX output, --total for total line
	if err != nil {
		return total, fmt.Errorf("failed to run 'df': %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return total, fmt.Errorf("unexpected output from 'df': %s: expected something like %s", string(b), "Filesystem 1024-blocks Used Available Capacity Mounted on")
	}
	total, err = parseDFLine(lines[1])
	if err != nil {
		return total, fmt.Errorf("parse df line %q: %w", lines[1], err)
	}
	if total.Filesystem != "total" {
		return total, fmt.Errorf("expected 'total' filesystem, got %q", total.Filesystem)
	}
	return total, nil
}

func parseDFLine(line string) (df dfstats, err error) {
	const (
		FILESYSTEM, BLOCKS, USED, AVAILABLE, CAPACITY_PERCENT, MOUNT = 0, 1, 2, 3, 4, 5
	)
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) < 6 {
		fields = append(fields, "0", "0", "0", "0%", "/") // Default values for missing fields.
	}
	blocks, err := strconv.ParseUint(fields[BLOCKS], 10, 64)
	if err != nil {
		return df, fmt.Errorf("parse blocks: %q: %w", fields[BLOCKS], err)
	}
	used, err := strconv.ParseUint(fields[USED], 10, 64)
	if err != nil {
		return df, fmt.Errorf("parse used: %q: %w", fields[USED], err)
	}
	available, err := strconv.ParseUint(fields[AVAILABLE], 10, 64)
	if err != nil {
		return df, fmt.Errorf("parse available: %q: %w", fields[AVAILABLE], err)
	}
	percent, err := strconv.ParseFloat(strings.TrimSuffix(fields[CAPACITY_PERCENT], "%"), 64)
	if err != nil {
		return df, fmt.Errorf("parse percent: %q: %w", fields[CAPACITY_PERCENT], err)
	}
	mount := fields[MOUNT]
	filesystem := fields[FILESYSTEM]
	return dfstats{
		Filesystem:  filesystem,
		Size:        blocks * 1024,    // Convert from 1024-blocks
		Used:        used * 1024,      // Convert from 1024-block
		Available:   available * 1024, // Convert from 1024-blocks
		PercentUsed: percent,
		Mount:       mount,
	}, nil
}

func parseFreeLine(line string) (fs freeStats, err error) {
	// Parse a line from the output of 'free -b -t'.
	// Example line: "Mem:     67377426432  2746585088 63083753472    86360064  1547087872 63889125376"

	titles := []string{"total", "used", "free", "shared", "buff/cache", "available"}

	fields := strings.Fields(line)
	var totalErr, usedErr, freeErr, sharedErr, buffErr, availableErr error
	const (
		TOTAL, USED, FREE, SHARED, BUFF_CACHE, AVAILABLE = 1, 2, 3, 4, 5, 6
	)
	// Ensure we have at least 7 fields (total, used, free, shared, buff/cache, available) by padding if needed.
	for i := len(fields); i < 7; i++ {
		fields = append(fields, "")
	}
	parse := func(i int) (uint64, error) {
		s := strings.TrimSpace(fields[i])
		if s == "" {
			return 0, nil
		}
		m, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s: %q: %w", titles[i-1], fields[i], err)
		}
		return m, nil
	}

	fs.Total, totalErr = parse(TOTAL)
	fs.Used, usedErr = parse(USED)
	fs.Free, freeErr = parse(FREE)
	fs.Shared, sharedErr = parse(SHARED)
	fs.Buff, buffErr = parse(BUFF_CACHE)
	fs.Available, availableErr = parse(AVAILABLE)
	return fs, errors.Join(totalErr, usedErr, freeErr, sharedErr, buffErr, availableErr)
}
*/
