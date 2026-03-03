# Workflow System Enhancement Plan

## Overview

This document outlines the plan to evolve Chronicle's workflow system into a production-ready durable execution engine, similar to Temporal or Cloudflare Workflows. The workflow system currently provides basic durable execution with step replay, but needs several critical features for real-world usage.

## Current State Analysis

### What Works Now

The existing workflow system (`workflow.go`, `workflow_test.go`) provides:

1. **Durable Execution**: Workflows are event-sourced aggregates, state is persisted after each step
2. **Step Replay**: `Step()` and `Step2()` cache results, allowing workflows to resume after crashes
3. **Type-Safe API**: Generic `Workflow[Params, Output]` with compile-time type safety
4. **SQLite Backend**: `NewSqliteRunner()` provides persistent storage via event log
5. **Basic Lifecycle**: Start → Run → GetResult workflow lifecycle

### Current API Surface

```go
// Creating workflows
runner, _ := workflow.NewSqliteRunner(db)
sendReports := workflow.New(runner, "send-reports", func(wctx workflow.Context, params *Params) (*Output, error) {
    files, _ := workflow.Step(wctx, func(ctx context.Context) ([]string, error) {
        return generateFiles()
    })
    _ = workflow.Step2(wctx, func(ctx context.Context) error {
        return sendEmail(files)
    })
    return &Output{Files: files}, nil
})

// Executing
instanceID, _ := sendReports.Start(ctx, params)
output, _ := sendReports.Run(ctx, instanceID)
result, _ := sendReports.GetResult(ctx, instanceID)
```

### Current Data Model

Workflow instances are stored as aggregates with events:
- `workflowStarted` - workflow begins
- `stepCompleted` - step finished with result
- `stepFailed` - step failed
- `workflowCompleted` - workflow finished successfully
- `workflowFailed` - workflow failed permanently

Each workflow instance has:
- `InstanceID` - unique identifier
- `workflowName` - registered workflow type
- `status` - pending/running/completed/failed
- `params` - serialized input parameters
- `output` - serialized output
- `stepResults` - map of step_index → serialized result
- `currentStep` - next step to execute

## Missing Features & Requirements

### 1. Sleep/Delay Functionality ⭐ CRITICAL

**Problem**: Workflows need to pause for long periods (minutes, hours, days). Currently, the only way to "wait" is to complete the workflow and start a new one, which breaks the logical flow.

**Requirements**:
- `workflow.Sleep(wctx, duration)` - pause workflow execution for a specified duration
- Durable: sleep must survive process restarts
- Efficient: sleeping workflows shouldn't consume resources
- Early wake: ability to signal/cancel sleep

**API Design**:

```go
// Basic sleep
err := workflow.Sleep(wctx, 3 * 24 * time.Hour) // Sleep for 3 days

// Sleep with early wake via signal (future enhancement)
err := workflow.Sleep(wctx, duration, workflow.SleepWithSignal("wake-signal"))
```

**Implementation Strategy**:

1. **New Event Type**: Add `workflowSleepStarted` event:
   ```go
   type workflowSleepStarted struct {
       StepIndex     int       `json:"stepIndex"`
       WakeUpTime    time.Time `json:"wakeUpTime"`
       Duration      string    `json:"duration"` // ISO8601 duration
   }
   ```

2. **Sleep Tracking**: Extend `WorkflowInstance` with:
   ```go
   sleepUntil *time.Time  // If set, workflow is sleeping
   ```

3. **Step Indexing**: Modify step execution to check for sleep:
   - When workflow resumes, check if current step is a sleep
   - If `time.Now() < sleepUntil`, workflow returns early with "sleeping" status
   - Runner needs a scheduler component to wake sleeping workflows

4. **Scheduler Component**: New `Scheduler` interface + implementations:
   ```go
   type Scheduler interface {
       ScheduleWake(ctx context.Context, instanceID InstanceID, at time.Time) error
       PollWakes(ctx context.Context) ([]InstanceID, error) // Called periodically or via trigger
   }
   
   // SQLite implementation uses a table:
   // CREATE TABLE workflow_wake_schedule (
   //     instance_id TEXT PRIMARY KEY,
   //     wake_at TIMESTAMP NOT NULL,
   //     created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
   // );
   // CREATE INDEX idx_wake_at ON workflow_wake_schedule(wake_at);
   ```

5. **Runner Integration**:
   ```go
   type Runner struct {
       // ... existing fields
       scheduler Scheduler
       clock     Clock // For testing
   }
   ```

6. **Sleep Function**:
   ```go
   func Sleep(wctx Context, duration time.Duration) error {
       instance, _ := wctx.runner.repo.Get(wctx.ctx, wctx.instanceID)
       stepIndex := instance.currentStep
       
       // Check if already sleeping (replay)
       if instance.stepResults[stepIndex] is sleep marker {
           wakeTime := parseWakeTime(instance.stepResults[stepIndex])
           if time.Now().Before(wakeTime) {
               return ErrWorkflowSleeping // Signal to stop execution
           }
           // Sleep completed, continue
           return nil
       }
       
       // New sleep
       wakeTime := time.Now().Add(duration)
       
       // Record sleep event
       instance.recordThat(&workflowSleepStarted{
           StepIndex:  stepIndex,
           WakeUpTime: wakeTime,
           Duration:   duration.String(),
       })
       
       // Save result (special marker)
       resultJSON, _ := json.Marshal(sleepResult{WakeAt: wakeTime})
       instance.recordThat(&stepCompleted{StepIndex: stepIndex, Result: resultJSON})
       
       _, _, _ = wctx.runner.repo.Save(wctx.ctx, instance)
       
       // Schedule wake
       wctx.runner.scheduler.ScheduleWake(wctx.ctx, wctx.instanceID, wakeTime)
       
       return ErrWorkflowSleeping // Stop execution
   }
   ```

**Challenges**:
- Sleep can be long (days/weeks), need to handle clock changes/timezone issues
- Need efficient polling/querying for wakeable workflows
- Must handle process restarts (scheduler must persist)

### 2. Workflow CRUD & Admin API ⭐ CRITICAL

**Problem**: No way to query workflows, see what's running, or manage them. Users need visibility into their workflow system.

**Requirements**:
- List all workflow instances
- Filter by status (pending, running, sleeping, completed, failed)
- Filter by workflow type/name
- Get detailed status of a specific workflow
- Cancel running workflows
- Retry failed workflows

**API Design**:

```go
// Admin/Query interface
type Admin interface {
    // List workflows with filtering
    List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error)
    
    // Get specific workflow details
    Get(ctx context.Context, instanceID InstanceID) (*WorkflowInfo, error)
    
    // Cancel a running/pending workflow
    Cancel(ctx context.Context, instanceID InstanceID, reason string) error
    
    // Retry a failed workflow
    Retry(ctx context.Context, instanceID InstanceID) error
    
    // Get workflow history (events)
    GetHistory(ctx context.Context, instanceID InstanceID) ([]WorkflowEvent, error)
}

type ListOptions struct {
    WorkflowName string      // Filter by workflow type
    Status       Status      // Filter by status
    Limit        int         // Pagination
    Offset       int         // Pagination
    Since        *time.Time  // Created after
    Until        *time.Time  // Created before
}

type WorkflowInfo struct {
    InstanceID   InstanceID
    WorkflowName string
    Status       Status
    CreatedAt    time.Time
    UpdatedAt    time.Time
    CurrentStep  int
    TotalSteps   int        // If known
    Error        string     // If failed
    Params       json.RawMessage
    Output       json.RawMessage
}
```

**Implementation Strategy**:

1. **Storage Abstraction Interface**:
   
   Since workflows are aggregates stored in event logs, we need a query layer:
   
   ```go
   // WorkflowStore is the interface for workflow metadata storage
   // Can be backed by SQLite, Postgres, or any other store
   type Store interface {
       // Create initial workflow record
       Create(ctx context.Context, info WorkflowInfo) error
       
       // Update status and metadata
       Update(ctx context.Context, instanceID InstanceID, updates Update) error
       
       // Get by ID
       Get(ctx context.Context, instanceID InstanceID) (*WorkflowInfo, error)
       
       // List with filtering
       List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error)
       
       // Count for pagination
       Count(ctx context.Context, opts ListOptions) (int, error)
       
       // Record event in history
       RecordEvent(ctx context.Context, instanceID InstanceID, event WorkflowEventRecord) error
       
       // Get event history
       GetHistory(ctx context.Context, instanceID InstanceID) ([]WorkflowEventRecord, error)
   }
   
   type Update struct {
       Status      *Status
       CurrentStep *int
       Error       *string
       Output      *[]byte
       UpdatedAt   time.Time
   }
   ```

2. **SQLite Implementation**:
   
   Add migration for workflow metadata table:
   ```sql
   CREATE TABLE workflow_instances (
       instance_id     TEXT PRIMARY KEY,
       workflow_name   TEXT NOT NULL,
       status          TEXT NOT NULL, -- pending, running, sleeping, completed, failed, cancelled
       current_step    INTEGER DEFAULT 0,
       params          BLOB,
       output          BLOB,
       error           TEXT,
       created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
       updated_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
       sleep_until     TIMESTAMP NULL,
       -- Indexes
       FOREIGN KEY (instance_id) REFERENCES chronicle_events(log_id)
   );
   
   CREATE INDEX idx_workflow_name ON workflow_instances(workflow_name);
   CREATE INDEX idx_workflow_status ON workflow_instances(status);
   CREATE INDEX idx_workflow_sleep ON workflow_instances(sleep_until) WHERE sleep_until IS NOT NULL;
   CREATE INDEX idx_workflow_created ON workflow_instances(created_at);
   
   -- Trigger to update updated_at
   CREATE TRIGGER workflow_instances_updated_at 
   AFTER UPDATE ON workflow_instances
   BEGIN
       UPDATE workflow_instances SET updated_at = CURRENT_TIMESTAMP WHERE instance_id = NEW.instance_id;
   END;
   ```

3. **Integration with Existing Flow**:
   
   Modify `Runner` to accept optional `Store`:
   ```go
   type Runner struct {
       repo      aggregate.Repository[InstanceID, WorkflowEvent, *WorkflowInstance]
       logger    *slog.Logger
       registry  event.Registry[event.Any]
       scheduler Scheduler
       store     Store  // Optional, for admin features
       clock     Clock
   }
   
   func NewRunner(eventLog event.Log, opts ...RunnerOption) (*Runner, error) {
       // ... existing setup
       for _, opt := range opts {
           opt(r)
       }
       return r, nil
   }
   
   type RunnerOption func(*Runner)
   
   func WithStore(store Store) RunnerOption {
       return func(r *Runner) { r.store = store }
   }
   
   func WithScheduler(scheduler Scheduler) RunnerOption {
       return func(r *Runner) { r.scheduler = scheduler }
   }
   ```

4. **Workflow.Start() Integration**:
   ```go
   func (w *Workflow[Params, Output]) Start(ctx context.Context, params *Params) (InstanceID, error) {
       // ... existing event recording
       
       // If store is available, create metadata record
       if w.runner.store != nil {
           w.runner.store.Create(ctx, WorkflowInfo{
               InstanceID:   instanceID,
               WorkflowName: w.name,
               Status:       StatusPending,
               Params:       paramsJSON,
               CreatedAt:    time.Now(),
           })
       }
       
       // ... save and return
   }
   ```

5. **Workflow.Run() Integration**:
   Update status as workflow progresses:
   ```go
   func (w *Workflow[Params, Output]) Run(...) {
       // Update to running
       if w.runner.store != nil {
           w.runner.store.Update(ctx, instanceID, Update{Status: ptr(StatusRunning)})
       }
       
       // ... execute
       
       // Update on completion/failure
       if w.runner.store != nil {
           w.runner.store.Update(ctx, instanceID, Update{
               Status: ptr(StatusCompleted),
               Output: &outputJSON,
           })
       }
   }
   ```

6. **Admin Implementation**:
   ```go
   type admin struct {
       store Store
   }
   
   func (a *admin) List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error) {
       return a.store.List(ctx, opts)
   }
   
   func (a *admin) Cancel(ctx context.Context, instanceID InstanceID, reason string) error {
       // Record cancellation event
       instance, _ := a.runner.repo.Get(ctx, instanceID)
       instance.recordThat(&workflowCancelled{Reason: reason})
       _, _, _ = a.runner.repo.Save(ctx, instance)
       
       // Update metadata
       return a.store.Update(ctx, instanceID, Update{
           Status: ptr(StatusCancelled),
           Error:  &reason,
       })
   }
   ```

**Challenges**:
- Eventual consistency: event log and metadata store need to be synchronized
- The metadata store is a projection of the event log, could get out of sync
- Solution: Make metadata updates idempotent, allow rebuilding from event log

### 3. Workflow Recovery & Auto-Restart ⭐ CRITICAL

**Problem**: When the application restarts, pending/running/sleeping workflows must be automatically resumed. Currently, workflows only run when explicitly called with `Run()`.

**Requirements**:
- On startup, identify all non-completed workflows
- Automatically resume running workflows
- Schedule wake-ups for sleeping workflows
- Handle failures gracefully (exponential backoff for retries)

**API Design**:

```go
type Recovery struct {
    runner    *Runner
    workflows map[string]*Workflow[Any, Any] // registered workflows by name
    options   RecoveryOptions
}

type RecoveryOptions struct {
    Concurrency    int           // Max parallel workflow executions
    RetryBackoff   time.Duration // Base backoff for failed workflows
    MaxRetries     int           // Max retry attempts
    PollInterval   time.Duration // How often to poll for new work
}

func (r *Recovery) Start(ctx context.Context) error
func (r *Recovery) Stop() error

// On application startup:
runner, _ := workflow.NewRunner(eventLog, opts...)
recovery := workflow.NewRecovery(runner, recoveryOpts)

// Register all workflow types
recovery.Register(sendReports)
recovery.Register(processOrders)

// Start recovery (non-blocking)
_ = recovery.Start(ctx)
```

**Implementation Strategy**:

1. **Workflow Registration**:
   ```go
   func (r *Recovery) Register(wf interface{}) {
       // Use reflection or require Workflow to implement interface
       // Store in map by name
   }
   ```

2. **Recovery Logic**:
   ```go
   func (r *Recovery) Start(ctx context.Context) error {
       // 1. Query all pending/running/sleeping workflows
       workflows, _ := r.runner.store.List(ctx, ListOptions{
           Statuses: []Status{StatusPending, StatusRunning, StatusSleeping},
       })
       
       // 2. For each workflow, determine action
       for _, wf := range workflows {
           switch wf.Status {
           case StatusPending, StatusRunning:
               // Resume execution
               go r.resumeWorkflow(ctx, wf)
               
           case StatusSleeping:
               if time.Now().After(wf.SleepUntil) {
                   // Wake time passed, resume
                   go r.resumeWorkflow(ctx, wf)
               } else {
                   // Re-schedule wake
                   r.runner.scheduler.ScheduleWake(ctx, wf.InstanceID, wf.SleepUntil)
               }
           }
       }
       
       // 3. Start polling for new workflows
       go r.pollLoop(ctx)
       
       return nil
   }
   ```

3. **Resume Workflow**:
   ```go
   func (r *Recovery) resumeWorkflow(ctx context.Context, info WorkflowInfo) {
       // Get registered workflow by name
       wf, ok := r.workflows[info.WorkflowName]
       if !ok {
           log.Error("unknown workflow type", "name", info.WorkflowName)
           return
       }
       
       // Execute with retry logic
       backoff := r.options.RetryBackoff
       for attempt := 0; attempt < r.options.MaxRetries; attempt++ {
           _, err := wf.Run(ctx, info.InstanceID)
           if err == nil {
               return // Success
           }
           
           // Failed, wait and retry
           time.Sleep(backoff)
           backoff *= 2 // Exponential backoff
       }
       
       // Max retries exceeded, mark as failed
       r.runner.store.Update(ctx, info.InstanceID, Update{
           Status: ptr(StatusFailed),
           Error:  ptr("max retries exceeded"),
       })
   }
   ```

4. **Polling for New Work**:
   ```go
   func (r *Recovery) pollLoop(ctx context.Context) {
       ticker := time.NewTicker(r.options.PollInterval)
       defer ticker.Stop()
       
       for {
           select {
           case <-ctx.Done():
               return
           case <-ticker.C:
               // Check for newly started workflows that aren't being processed
               r.checkForNewWork(ctx)
           }
       }
   }
   ```

5. **Integration with Sleep**:
   When `Sleep()` is called, the workflow returns early. Recovery needs to:
   - Detect that workflow is sleeping (check status)
   - Not retry sleeping workflows immediately
   - Wait for scheduler to wake them

**Challenges**:
- Concurrency control: prevent multiple instances from running same workflow
- Need distributed locks if running multiple app instances
- SQLite might not be suitable for multi-instance deployments

### 4. Storage Abstraction Interface ⭐ CRITICAL

**Problem**: Currently tied to SQLite for workflow metadata. Users need flexibility to use Postgres, Redis, or custom stores.

**Design**:

```go
// Package workflow/storage

// Store is the primary interface for workflow metadata and scheduling
type Store interface {
    MetadataStore
    SchedulerStore
}

// MetadataStore handles workflow instance metadata
type MetadataStore interface {
    Create(ctx context.Context, info WorkflowInfo) error
    Get(ctx context.Context, instanceID InstanceID) (*WorkflowInfo, error)
    Update(ctx context.Context, instanceID InstanceID, updates Updates) error
    List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error)
    Count(ctx context.Context, opts ListOptions) (int, error)
}

// SchedulerStore handles wake scheduling for sleeping workflows
type SchedulerStore interface {
    ScheduleWake(ctx context.Context, instanceID InstanceID, wakeAt time.Time) error
    GetDueWakes(ctx context.Context, before time.Time) ([]InstanceID, error)
    CompleteWake(ctx context.Context, instanceID InstanceID) error
}

// Clock abstracts time for testing
type Clock interface {
    Now() time.Time
}

// Built-in implementations:
// - storage.NewSqlite(db *sql.DB) Store
// - storage.NewPostgres(db *sql.DB) Store
// - storage.NewMemory() Store (for testing)
```

**SQLite Implementation**:

```go
type sqliteStore struct {
    db    *sql.DB
    clock Clock
}

func (s *sqliteStore) Create(ctx context.Context, info WorkflowInfo) error {
    _, err := s.db.ExecContext(ctx, `
        INSERT INTO workflow_instances 
        (instance_id, workflow_name, status, current_step, params, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
    `, info.InstanceID, info.WorkflowName, info.Status, 0, info.Params, s.clock.Now())
    return err
}

func (s *sqliteStore) List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error) {
    query := `SELECT instance_id, workflow_name, status, ... FROM workflow_instances WHERE 1=1`
    args := []interface{}{}
    
    if opts.WorkflowName != "" {
        query += " AND workflow_name = ?"
        args = append(args, opts.WorkflowName)
    }
    if opts.Status != "" {
        query += " AND status = ?"
        args = append(args, opts.Status)
    }
    // ... more filters
    
    query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
    args = append(args, opts.Limit, opts.Offset)
    
    rows, err := s.db.QueryContext(ctx, query, args...)
    // ... scan rows
}

func (s *sqliteStore) ScheduleWake(ctx context.Context, instanceID InstanceID, wakeAt time.Time) error {
    _, err := s.db.ExecContext(ctx, `
        INSERT INTO workflow_wake_schedule (instance_id, wake_at)
        VALUES (?, ?)
        ON CONFLICT(instance_id) DO UPDATE SET wake_at = excluded.wake_at
    `, instanceID, wakeAt)
    
    // Also update instance status to sleeping
    _, _ = s.db.ExecContext(ctx, `
        UPDATE workflow_instances 
        SET status = 'sleeping', sleep_until = ?
        WHERE instance_id = ?
    `, wakeAt, instanceID)
    
    return err
}

func (s *sqliteStore) GetDueWakes(ctx context.Context, before time.Time) ([]InstanceID, error) {
    rows, err := s.db.QueryContext(ctx, `
        SELECT instance_id FROM workflow_wake_schedule
        WHERE wake_at <= ?
        ORDER BY wake_at ASC
    `, before)
    // ... scan and return
}
```

**Migration Strategy**:

Migrations should be separate from core library:
```go
// In user's code:
db, _ := sql.Open("sqlite3", "workflow.db")

// Run migrations (using goose, golang-migrate, etc.)
workflowstorage.MigrateSQLite(db)

// Create store
store := workflowstorage.NewSqlite(db)

// Create runner with store
runner, _ := workflow.NewRunner(eventLog, 
    workflow.WithStore(store),
    workflow.WithScheduler(store), // Store implements both interfaces
)
```

### 5. Enhanced Context & Observability

**Problem**: Current workflow context is minimal. Production systems need tracing, logging, and metadata.

**Features**:

```go
// Enhanced context
type Context struct {
    context.Context  // Embeds standard context
    
    instanceID InstanceID
    workflowName string
    runner     *Runner
    
    // Metadata
    Attempt    int           // Current retry attempt
    StartTime  time.Time     // When workflow started
}

// Accessors
func (c Context) InstanceID() InstanceID
func (c Context) WorkflowName() string
func (c Context) Attempt() int

// Tracing integration
func (c Context) WithSpan(name string) (Context, Span)

// Logging with context
func (c Context) Logger() *slog.Logger  // Pre-configured with workflow fields
```

### 6. Signal Support (Future)

**Problem**: External systems need to send data/signals to running workflows (e.g., approve order, cancel process).

**API**:

```go
// In workflow function:
func myWorkflow(wctx workflow.Context, params *Params) (*Output, error) {
    // Wait for approval signal
    approval, _ := workflow.AwaitSignal[ApprovalData](wctx, "approval", 24*time.Hour)
    
    if !approval.Approved {
        return nil, fmt.Errorf("not approved")
    }
    
    // Continue...
}

// External code:
runner.Signal(ctx, instanceID, "approval", approvalData)
```

This requires:
- Signal queue per workflow instance
- Async handling of signals during workflow execution
- Integration with sleep (wake on signal)

### 7. Child Workflows (Future)

**Problem**: Workflows need to spawn other workflows and wait for completion.

**API**:

```go
func parentWorkflow(wctx workflow.Context, params *ParentParams) (*ParentOutput, error) {
    // Start child workflow
    childID, _ := workflow.StartChild(wctx, "child-workflow", childParams)
    
    // Wait for completion
    result, _ := workflow.AwaitChild[ChildOutput](wctx, childID)
    
    // Or fire-and-forget
    _ = workflow.StartChild(wctx, "async-child", asyncParams)
    
    return &ParentOutput{}, nil
}
```

## Implementation Phases

### Phase 1: Core Infrastructure (Week 1-2)
1. Define `Store` interface and SQLite implementation
2. Add workflow metadata table migrations
3. Integrate store with Runner (optional dependency)
4. Add `Admin` interface and basic implementation
5. Update `Start()`, `Run()` to write metadata

### Phase 2: Sleep & Scheduling (Week 2-3)
1. Define `Scheduler` interface
2. Implement SQLite-based scheduler
3. Add `workflow.Sleep()` function
4. Add sleep event types and handling
5. Update recovery logic to handle sleeping workflows

### Phase 3: Recovery System (Week 3-4)
1. Implement `Recovery` struct
2. Add workflow registration mechanism
3. Implement startup scanning and resumption
4. Add polling for new workflows
5. Add retry logic with backoff

### Phase 4: Testing & Polish (Week 4-5)
1. Comprehensive tests for sleep functionality
2. Tests for recovery scenarios
3. Tests for store implementations
4. Integration tests
5. Documentation and examples

### Phase 5: Advanced Features (Future)
1. Signal support
2. Child workflows
3. Postgres store implementation
4. Distributed locks for multi-instance
5. Metrics and observability

## Design Decisions

### 1. Optional Store Pattern

The `Store` is optional in `Runner`. If not provided, admin features and recovery are disabled. This maintains backward compatibility and allows gradual adoption.

### 2. Event Log as Source of Truth

The event log remains the authoritative source. The metadata store is a **projection** for querying convenience. It can be rebuilt from the event log if corrupted.

### 3. SQLite First, Pluggable Later

Start with SQLite implementation since it's already used. The interface design should support Postgres, but implementation can come later.

### 4. Sleep vs. External Trigger

Sleep is implemented as a special step that records a wake time. This is simpler than external timers but requires the recovery system to poll for due wakes.

### 5. No Distributed Locks (Initially)

For the first version, assume single-instance deployments. Multi-instance support with distributed locks can be added later via interface extension.

## Testing Strategy

### Unit Tests
- Store interface implementations
- Scheduler logic
- Sleep calculation
- Retry backoff

### Integration Tests
- Full workflow with sleep
- Crash recovery simulation
- Concurrent workflow execution
- Admin API operations

### Property-Based Tests
- Workflow determinism (same events → same result)
- Replay invariants (step executes exactly once)
- Recovery safety (no duplicate executions)

## Example Usage After Implementation

```go
package main

import (
    "context"
    "database/sql"
    "time"
    
    "github.com/DeluxeOwl/chronicle/workflow"
    "github.com/DeluxeOwl/chronicle/workflow/storage"
    _ "github.com/mattn/go-sqlite3"
)

func main() {
    ctx := context.Background()
    
    // Setup database
    db, _ := sql.Open("sqlite3", "workflow.db")
    
    // Run migrations
    storage.MigrateSQLite(db)
    
    // Create event log and store
    eventLog, _ := eventlog.NewSqlite(db)
    store := storage.NewSqlite(db)
    
    // Create runner with all features
    runner, _ := workflow.NewRunner(eventLog,
        workflow.WithStore(store),
        workflow.WithScheduler(store),
    )
    
    // Define workflow
    processOrder := workflow.New(runner, "process-order", 
        func(wctx workflow.Context, params *OrderParams) (*OrderResult, error) {
            // Step 1: Reserve inventory
            reservation, _ := workflow.Step(wctx, func(ctx context.Context) (*Reservation, error) {
                return inventory.Reserve(ctx, params.Items)
            })
            
            // Step 2: Wait for payment (with 24h timeout)
            _ = workflow.Sleep(wctx, 24*time.Hour)
            
            // Step 3: Check payment status
            payment, _ := workflow.Step(wctx, func(ctx context.Context) (*Payment, error) {
                return paymentService.GetStatus(ctx, params.OrderID)
            })
            
            if !payment.Paid {
                // Release reservation
                _ = workflow.Step2(wctx, func(ctx context.Context) error {
                    return inventory.Release(ctx, reservation.ID)
                })
                return nil, fmt.Errorf("payment not received")
            }
            
            // Step 4: Ship
            shipment, _ := workflow.Step(wctx, func(ctx context.Context) (*Shipment, error) {
                return shipping.Create(ctx, params.Items, params.Address)
            })
            
            return &OrderResult{ShipmentID: shipment.ID}, nil
        },
    )
    
    // Admin API
    admin := workflow.NewAdmin(store)
    
    // Recovery system
    recovery := workflow.NewRecovery(runner, workflow.RecoveryOptions{
        Concurrency:  10,
        PollInterval: 5 * time.Second,
    })
    recovery.Register(processOrder)
    recovery.Start(ctx)
    defer recovery.Stop()
    
    // In HTTP handler:
    http.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
        params := &OrderParams{...}
        instanceID, _ := processOrder.Start(r.Context(), params)
        json.NewEncoder(w).Encode(map[string]string{"instance_id": string(instanceID)})
    })
    
    http.HandleFunc("/admin/workflows", func(w http.ResponseWriter, r *http.Request) {
        workflows, _ := admin.List(r.Context(), workflow.ListOptions{
            Status: workflow.StatusRunning,
            Limit:  100,
        })
        json.NewEncoder(w).Encode(workflows)
    })
    
    http.ListenAndServe(":8080", nil)
}
```

## Success Criteria

1. **Sleep Works**: Can sleep for days, survives restarts, wakes on time
2. **Recovery Works**: All pending workflows resume after restart
3. **Admin API**: Can list, query, and manage workflows
4. **Pluggable Storage**: Can implement custom Store interface
5. **Backward Compatible**: Existing code continues to work
6. **Well Tested**: >90% coverage, property tests, integration tests
7. **Documented**: Comprehensive examples and guides

## Open Questions

1. Should we support sagas/compensating transactions explicitly?
2. How to handle versioning of workflow definitions?
3. What's the story for multi-region deployments?
4. Should we provide a web UI for workflow monitoring?
5. How to handle long-running workflows with schema migrations?

## Notes

- Keep the API simple and Go-idiomatic
- Learn from Temporal's mistakes (complexity) and Cloudflare's simplicity
- Event log is the foundation - everything else is derived
- Make it easy to test workflows (deterministic execution)
- Consider future integration with the projection system
