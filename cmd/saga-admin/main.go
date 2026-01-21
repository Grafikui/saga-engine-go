// saga-admin is a CLI tool for managing saga workflows.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	saga "github.com/grafikui/saga-engine-go"
	_ "github.com/lib/pq"
)

var (
	databaseURL string
	tableName   string
)

func main() {
	flag.StringVar(&databaseURL, "db", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL")
	flag.StringVar(&tableName, "table", "transactions", "Table name for saga workflows")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "list":
		runList(cmdArgs)
	case "show":
		runShow(cmdArgs)
	case "retry":
		runRetry(cmdArgs)
	case "stats":
		runStats(cmdArgs)
	case "dead-letter":
		runDeadLetter(cmdArgs)
	case "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`saga-admin - Saga workflow management CLI

Usage:
  saga-admin [flags] <command> [args]

Flags:
  -db string      PostgreSQL connection URL (or set DATABASE_URL env var)
  -table string   Table name for saga workflows (default "transactions")

Commands:
  list            List workflows (optionally filter by status)
  show <id>       Show details of a specific workflow
  retry <id>      Retry a dead_letter workflow
  stats           Show workflow statistics
  dead-letter     List all dead_letter workflows
  help            Show this help message

Examples:
  saga-admin -db "postgres://localhost/mydb" list
  saga-admin -db "postgres://localhost/mydb" list --status dead_letter
  saga-admin -db "postgres://localhost/mydb" show tx-123
  saga-admin -db "postgres://localhost/mydb" retry tx-123
  saga-admin -db "postgres://localhost/mydb" stats`)
}

func getStorage() (*saga.PostgresStorage, func()) {
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "Error: DATABASE_URL or -db flag required")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}

	storage, err := saga.NewPostgresStorage(db, tableName)
	if err != nil {
		db.Close()
		fmt.Fprintf(os.Stderr, "Error creating storage: %v\n", err)
		os.Exit(1)
	}

	return storage, func() { db.Close() }
}

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	status := fs.String("status", "", "Filter by status (pending, completed, failed, dead_letter)")
	limit := fs.Int("limit", 20, "Maximum number of results")
	offset := fs.Int("offset", 0, "Offset for pagination")
	_ = fs.Parse(args)

	storage, cleanup := getStorage()
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	filter := saga.WorkflowFilter{
		Limit:  *limit,
		Offset: *offset,
	}

	if *status != "" {
		filter.Status = []saga.WorkflowStatus{saga.WorkflowStatus(*status)}
	}

	result, err := storage.Query(ctx, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying workflows: %v\n", err)
		os.Exit(1)
	}

	if len(result.Workflows) == 0 {
		fmt.Println("No workflows found.")
		return
	}

	fmt.Printf("Showing %d of %d workflows:\n\n", len(result.Workflows), result.Total)
	fmt.Printf("%-40s %-12s %-6s %-20s\n", "ID", "STATUS", "RETRY", "UPDATED")
	fmt.Println(strings.Repeat("-", 80))

	for _, wf := range result.Workflows {
		fmt.Printf("%-40s %-12s %-6d %-20s\n",
			truncate(wf.ID, 40),
			wf.Status,
			wf.RetryCount,
			wf.UpdatedAt.Format("2006-01-02 15:04:05"),
		)
	}
}

func runShow(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: workflow ID required")
		os.Exit(1)
	}

	id := args[0]

	storage, cleanup := getStorage()
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	wf, err := storage.GetWorkflow(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching workflow: %v\n", err)
		os.Exit(1)
	}

	if wf == nil {
		fmt.Fprintf(os.Stderr, "Workflow not found: %s\n", id)
		os.Exit(1)
	}

	fmt.Printf("Workflow: %s\n", wf.ID)
	fmt.Printf("Status:   %s\n", wf.Status)
	fmt.Printf("Retries:  %d\n", wf.RetryCount)
	fmt.Printf("Created:  %s\n", wf.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Updated:  %s\n", wf.UpdatedAt.Format(time.RFC3339))

	if len(wf.Input) > 0 && string(wf.Input) != "{}" {
		fmt.Printf("\nInput:\n")
		prettyPrint(wf.Input)
	}

	if len(wf.StepStack) > 0 {
		fmt.Printf("\nSteps (%d):\n", len(wf.StepStack))
		for i, step := range wf.StepStack {
			fmt.Printf("  %d. %s [%s]\n", i+1, step.Name, step.Status)
			if step.Result != nil {
				resultJSON, _ := json.Marshal(step.Result)
				fmt.Printf("     Result: %s\n", truncate(string(resultJSON), 60))
			}
		}
	}

	if wf.Error != nil {
		fmt.Printf("\nError:\n")
		fmt.Printf("  Step:      %s\n", wf.Error.StepName)
		fmt.Printf("  Error:     %s\n", wf.Error.Error)
		if wf.Error.CompensationError != "" {
			fmt.Printf("  Comp Err:  %s\n", wf.Error.CompensationError)
		}
		fmt.Printf("  Timestamp: %s\n", wf.Error.Timestamp.Format(time.RFC3339))
	}
}

func runRetry(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: workflow ID required")
		os.Exit(1)
	}

	id := args[0]

	storage, cleanup := getStorage()
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First check the workflow exists and is in dead_letter
	wf, err := storage.GetWorkflow(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching workflow: %v\n", err)
		os.Exit(1)
	}

	if wf == nil {
		fmt.Fprintf(os.Stderr, "Workflow not found: %s\n", id)
		os.Exit(1)
	}

	if wf.Status != saga.StatusDeadLetter {
		fmt.Fprintf(os.Stderr, "Workflow is not in dead_letter state (current: %s)\n", wf.Status)
		os.Exit(1)
	}

	// Check retry cap
	if wf.RetryCount >= saga.MaxRetryCount {
		fmt.Fprintf(os.Stderr, "Workflow has exceeded retry cap (%d/%d)\n", wf.RetryCount, saga.MaxRetryCount)
		os.Exit(1)
	}

	// Perform atomic retry
	newCount, err := storage.AtomicRetry(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrying workflow: %v\n", err)
		os.Exit(1)
	}

	if newCount == -1 {
		fmt.Fprintf(os.Stderr, "Failed to retry workflow (may have been modified concurrently)\n")
		os.Exit(1)
	}

	fmt.Printf("Workflow %s transitioned to pending (retry %d/%d)\n", id, newCount, saga.MaxRetryCount)
	fmt.Println("The workflow will be picked up on next execution.")
}

func runStats(args []string) {
	storage, cleanup := getStorage()
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	statuses := []saga.WorkflowStatus{
		saga.StatusPending,
		saga.StatusCompleted,
		saga.StatusFailed,
		saga.StatusDeadLetter,
	}

	fmt.Println("Workflow Statistics:")
	fmt.Println(strings.Repeat("-", 30))

	total := 0
	for _, status := range statuses {
		count, err := storage.CountByStatus(ctx, status)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error counting %s: %v\n", status, err)
			continue
		}
		total += count
		fmt.Printf("%-15s %d\n", status+":", count)
	}

	fmt.Println(strings.Repeat("-", 30))
	fmt.Printf("%-15s %d\n", "Total:", total)
}

func runDeadLetter(args []string) {
	fs := flag.NewFlagSet("dead-letter", flag.ExitOnError)
	limit := fs.Int("limit", 50, "Maximum number of results")
	_ = fs.Parse(args)

	storage, cleanup := getStorage()
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := storage.Query(ctx, saga.WorkflowFilter{
		Status: []saga.WorkflowStatus{saga.StatusDeadLetter},
		Limit:  *limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying dead_letter workflows: %v\n", err)
		os.Exit(1)
	}

	if len(result.Workflows) == 0 {
		fmt.Println("No dead_letter workflows found.")
		return
	}

	fmt.Printf("Dead Letter Workflows (%d total):\n\n", result.Total)
	fmt.Printf("%-36s %-6s %-20s %s\n", "ID", "RETRY", "FAILED STEP", "ERROR")
	fmt.Println(strings.Repeat("-", 100))

	for _, wf := range result.Workflows {
		stepName := "unknown"
		errMsg := ""
		if wf.Error != nil {
			stepName = wf.Error.StepName
			errMsg = wf.Error.Error
		}
		fmt.Printf("%-36s %-6d %-20s %s\n",
			truncate(wf.ID, 36),
			wf.RetryCount,
			truncate(stepName, 20),
			truncate(errMsg, 40),
		)
	}

	fmt.Printf("\nUse 'saga-admin retry <id>' to retry a workflow.\n")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func prettyPrint(data []byte) {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		fmt.Printf("  %s\n", string(data))
		return
	}
	pretty, _ := json.MarshalIndent(v, "  ", "  ")
	fmt.Printf("  %s\n", string(pretty))
}
