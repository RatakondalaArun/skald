// Command skald-bench measures multi-process write contention on the Skald
// SQLite store. The parent process spawns child processes (re-executing
// itself) that each run small write transactions against one shared database.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"skald/internal/store"
)

const taskPool = 20 // tasks shared by all workers, so rows are contended

type childResult struct {
	LatenciesNS []int64 `json:"latencies_ns"`
	BusyErrors  int     `json:"busy_errors"`
	OtherErrors int     `json:"other_errors"`
	FirstError  string  `json:"first_error,omitempty"`
	Reads       int     `json:"reads"`
	ReadErrors  int     `json:"read_errors"`
}

func main() {
	procs := flag.Int("procs", 4, "child processes")
	txs := flag.Int("txs", 1000, "write transactions per process")
	child := flag.Int("child", -1, "internal: run as child with this worker id")
	flag.Parse()

	if *child >= 0 {
		runChild(*child, *txs)
		return
	}
	if err := runParent(*procs, *txs); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func runParent(procs, txs int) error {
	home, err := os.MkdirTemp("", "skald-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	os.Setenv("SKALD_HOME", home)

	ctx := context.Background()
	db, err := store.Open(ctx, home+"/skald.db")
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects(id,slug,name,created_at,updated_at) VALUES (1,'bench','bench',?,?)`, now, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO features(id,project_id,title,created_at,updated_at) VALUES (1,1,'f',?,?)`, now, now); err != nil {
			return err
		}
		for i := 1; i <= taskPool; i++ {
			if _, err := tx.Exec(`INSERT INTO tasks(id,project_id,feature_id,title,created_at,updated_at) VALUES (?,1,1,'t',?,?)`, i, now, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	db.Close()

	self, err := os.Executable()
	if err != nil {
		return err
	}
	results := make([]childResult, procs)
	errs := make([]error, procs)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(self, "-child", fmt.Sprint(i), "-txs", fmt.Sprint(txs))
			cmd.Env = os.Environ()
			cmd.Stderr = os.Stderr
			out, err := cmd.Output()
			if err != nil {
				errs[i] = fmt.Errorf("child %d: %w", i, err)
				return
			}
			errs[i] = json.Unmarshal(out, &results[i])
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, e := range errs {
		if e != nil {
			return e
		}
	}

	var all []int64
	var busy, other, reads, readErrs int
	var first string
	for _, r := range results {
		all = append(all, r.LatenciesNS...)
		busy += r.BusyErrors
		other += r.OtherErrors
		reads += r.Reads
		readErrs += r.ReadErrors
		if first == "" {
			first = r.FirstError
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) float64 {
		if len(all) == 0 {
			return 0
		}
		idx := int(float64(len(all)-1) * p)
		return float64(all[idx]) / 1e6
	}

	// Verify the event log: every successful tx wrote exactly one event.
	db, err = store.Open(ctx, home+"/skald.db")
	if err != nil {
		return err
	}
	var events, maxID int
	if err := db.Reader().QueryRow(`SELECT count(*), coalesce(max(id),0) FROM events`).Scan(&events, &maxID); err != nil {
		return err
	}
	db.Close()

	fmt.Printf("procs=%d txs/proc=%d ok=%d elapsed=%.2fs throughput=%.0f tx/s\n",
		procs, txs, len(all), elapsed.Seconds(), float64(len(all))/elapsed.Seconds())
	fmt.Printf("latency ms: p50=%.2f p95=%.2f p99=%.2f max=%.2f\n", pct(.50), pct(.95), pct(.99), pct(1))
	fmt.Printf("errors: busy=%d other=%d read_errors=%d (reads=%d)\n", busy, other, readErrs, reads)
	if first != "" {
		fmt.Printf("first error: %s\n", first)
	}
	fmt.Printf("events: count=%d max_id=%d expected=%d\n", events, maxID, len(all))
	if events != len(all) || busy+other+readErrs > 0 {
		return fmt.Errorf("run had failures")
	}
	return nil
}

func runChild(id, txs int) {
	ctx := context.Background()
	db, err := store.Open(ctx, os.Getenv("SKALD_HOME")+"/skald.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(1)
	}
	defer db.Close()

	res := childResult{LatenciesNS: make([]int64, 0, txs)}

	stop := make(chan struct{})
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var n int
			err := db.Reader().QueryRow(`SELECT (SELECT count(*) FROM tasks WHERE status = 'in_progress') + (SELECT coalesce(max(id),0) FROM events)`).Scan(&n)
			if err != nil {
				res.ReadErrors++
			} else {
				res.Reads++
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	for i := 0; i < txs; i++ {
		taskID := (id*7+i)%taskPool + 1
		status := []string{"in_progress", "blocked", "todo"}[i%3]
		t0 := time.Now()
		err := db.WriteTx(ctx, func(tx *sql.Tx) error {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Exec(`UPDATE tasks SET status=?, version=version+1, updated_at=? WHERE id=?`, status, now, taskID); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO events(project_id,entity_type,entity_id,type,actor,payload,created_at) VALUES (1,'task',?,'task.status_changed',?,?,?)`,
				taskID, fmt.Sprintf("bench:%d", id), `{"to":"`+status+`"}`, now)
			return err
		})
		lat := time.Since(t0)
		if err != nil {
			if isBusy(err) {
				res.BusyErrors++
			} else {
				res.OtherErrors++
			}
			if res.FirstError == "" {
				res.FirstError = err.Error()
			}
			continue
		}
		res.LatenciesNS = append(res.LatenciesNS, lat.Nanoseconds())
	}
	close(stop)
	rwg.Wait()
	json.NewEncoder(os.Stdout).Encode(res)
}

func isBusy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked") || strings.Contains(s, "(5)")
}
