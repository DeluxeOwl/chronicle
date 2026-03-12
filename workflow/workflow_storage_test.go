package workflow

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// mockClock allows controlling time in tests
type mockClock struct {
	now time.Time
}

func (m *mockClock) Now() time.Time {
	return m.now
}

func (m *mockClock) Advance(d time.Duration) {
	m.now = m.now.Add(d)
}

func setupTestDB(t *testing.T) (*sql.DB, *mockClock) {
	t.Helper()

	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	require.NoError(t, err)

	db.SetMaxOpenConns(1)

	err = MigrateSQLite(db)
	require.NoError(t, err)

	clock := &mockClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}

	t.Cleanup(func() {
		db.Close()
	})

	return db, clock
}

func TestSQLiteStore_Create(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test-workflow",
		Status:       StatusPending,
		CurrentStep:  0,
		Params:       []byte(`{"key": "value"}`),
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}

	err := store.Create(ctx, info)
	require.NoError(t, err)

	// Verify by reading back
	retrieved, err := store.Get(ctx, "wf-123")
	require.NoError(t, err)
	require.Equal(t, info.InstanceID, retrieved.InstanceID)
	require.Equal(t, info.WorkflowName, retrieved.WorkflowName)
	require.Equal(t, info.Status, retrieved.Status)
	require.Equal(t, info.CurrentStep, retrieved.CurrentStep)
	require.Equal(t, info.Params, retrieved.Params)
}

func TestSQLiteStore_Create_Duplicate(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test-workflow",
		Status:       StatusPending,
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}

	err := store.Create(ctx, info)
	require.NoError(t, err)

	// Try to create duplicate
	err = store.Create(ctx, info)
	require.Error(t, err)
}

func TestSQLiteStore_Get_NotFound(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	_, err := store.Get(ctx, "non-existent")
	require.Error(t, err)
	require.ErrorContains(t, err, "not found")
}

func TestSQLiteStore_Update(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create initial record
	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test-workflow",
		Status:       StatusPending,
		CurrentStep:  0,
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}
	err := store.Create(ctx, info)
	require.NoError(t, err)

	// Advance time for update
	clock.Advance(time.Hour)

	// Update status and step
	newStatus := StatusRunning
	newStep := 3
	updates := Updates{
		Status:      &newStatus,
		CurrentStep: &newStep,
	}

	err = store.Update(ctx, "wf-123", updates)
	require.NoError(t, err)

	// Verify
	retrieved, err := store.Get(ctx, "wf-123")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, retrieved.Status)
	require.Equal(t, 3, retrieved.CurrentStep)
	require.True(t, retrieved.UpdatedAt.Equal(clock.Now()), "updated_at should be set to clock time")
}

func TestSQLiteStore_Update_NotFound(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	newStatus := StatusRunning
	updates := Updates{Status: &newStatus}

	err := store.Update(ctx, "non-existent", updates)
	require.Error(t, err)
	require.ErrorContains(t, err, "not found")
}

func TestSQLiteStore_Update_WithSleepUntil(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test-workflow",
		Status:       StatusRunning,
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}
	err := store.Create(ctx, info)
	require.NoError(t, err)

	sleepTime := clock.Now().Add(24 * time.Hour)
	updates := Updates{
		Status:     ptr(StatusSleeping),
		SleepUntil: &sleepTime,
	}

	err = store.Update(ctx, "wf-123", updates)
	require.NoError(t, err)

	retrieved, err := store.Get(ctx, "wf-123")
	require.NoError(t, err)
	require.Equal(t, StatusSleeping, retrieved.Status)
	require.NotNil(t, retrieved.SleepUntil)
	require.True(t, retrieved.SleepUntil.Equal(sleepTime))
}

func TestSQLiteStore_List(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create multiple workflows
	now := clock.Now()
	workflows := []WorkflowInfo{
		{InstanceID: "wf-1", WorkflowName: "type-a", Status: StatusPending, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-2", WorkflowName: "type-a", Status: StatusRunning, CreatedAt: now.Add(time.Minute), UpdatedAt: now},
		{InstanceID: "wf-3", WorkflowName: "type-b", Status: StatusCompleted, CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now},
	}

	for _, wf := range workflows {
		err := store.Create(ctx, wf)
		require.NoError(t, err)
	}

	// Test list all
	results, err := store.List(ctx, ListOptions{})
	require.NoError(t, err)
	require.Len(t, results, 3)

	// Test filter by name
	results, err = store.List(ctx, ListOptions{WorkflowName: "type-a"})
	require.NoError(t, err)
	require.Len(t, results, 2)

	// Test filter by status
	results, err = store.List(ctx, ListOptions{Status: StatusPending})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, InstanceID("wf-1"), results[0].InstanceID)

	// Test pagination
	results, err = store.List(ctx, ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, results, 2)

	results, err = store.List(ctx, ListOptions{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestSQLiteStore_List_WithStatuses(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	now := clock.Now()
	workflows := []WorkflowInfo{
		{InstanceID: "wf-1", WorkflowName: "test", Status: StatusPending, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-2", WorkflowName: "test", Status: StatusRunning, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-3", WorkflowName: "test", Status: StatusCompleted, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-4", WorkflowName: "test", Status: StatusFailed, CreatedAt: now, UpdatedAt: now},
	}

	for _, wf := range workflows {
		err := store.Create(ctx, wf)
		require.NoError(t, err)
	}

	// Filter multiple statuses
	results, err := store.List(ctx, ListOptions{Statuses: []Status{StatusPending, StatusRunning}})
	require.NoError(t, err)
	require.Len(t, results, 2)
}

func TestSQLiteStore_List_WithTimeRange(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	baseTime := clock.Now()

	workflows := []WorkflowInfo{
		{InstanceID: "wf-1", WorkflowName: "test", Status: StatusPending, CreatedAt: baseTime, UpdatedAt: baseTime},
		{InstanceID: "wf-2", WorkflowName: "test", Status: StatusPending, CreatedAt: baseTime.Add(time.Hour), UpdatedAt: baseTime},
		{InstanceID: "wf-3", WorkflowName: "test", Status: StatusPending, CreatedAt: baseTime.Add(2 * time.Hour), UpdatedAt: baseTime},
	}

	for _, wf := range workflows {
		err := store.Create(ctx, wf)
		require.NoError(t, err)
	}

	// Filter by time range
	since := baseTime.Add(30 * time.Minute)
	until := baseTime.Add(90 * time.Minute)

	results, err := store.List(ctx, ListOptions{Since: &since, Until: &until})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, InstanceID("wf-2"), results[0].InstanceID)
}

func TestSQLiteStore_Count(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create workflows
	now := clock.Now()
	workflows := []WorkflowInfo{
		{InstanceID: "wf-1", WorkflowName: "type-a", Status: StatusPending, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-2", WorkflowName: "type-a", Status: StatusRunning, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-3", WorkflowName: "type-b", Status: StatusCompleted, CreatedAt: now, UpdatedAt: now},
	}

	for _, wf := range workflows {
		err := store.Create(ctx, wf)
		require.NoError(t, err)
	}

	// Count all
	count, err := store.Count(ctx, ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 3, count)

	// Count by name
	count, err = store.Count(ctx, ListOptions{WorkflowName: "type-a"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// Count by status
	count, err = store.Count(ctx, ListOptions{Status: StatusPending})
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestSQLiteStore_ScheduleWake(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create workflow
	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test",
		Status:       StatusRunning,
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}
	err := store.Create(ctx, info)
	require.NoError(t, err)

	// Schedule wake
	wakeTime := clock.Now().Add(24 * time.Hour)
	err = store.ScheduleWake(ctx, "wf-123", wakeTime)
	require.NoError(t, err)

	// Verify status changed to sleeping
	retrieved, err := store.Get(ctx, "wf-123")
	require.NoError(t, err)
	require.Equal(t, StatusSleeping, retrieved.Status)
	require.NotNil(t, retrieved.SleepUntil)
	require.True(t, retrieved.SleepUntil.Equal(wakeTime))
}

func TestSQLiteStore_GetDueWakes(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create workflows
	now := clock.Now()
	workflows := []WorkflowInfo{
		{InstanceID: "wf-1", WorkflowName: "test", Status: StatusRunning, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-2", WorkflowName: "test", Status: StatusRunning, CreatedAt: now, UpdatedAt: now},
		{InstanceID: "wf-3", WorkflowName: "test", Status: StatusRunning, CreatedAt: now, UpdatedAt: now},
	}

	for _, wf := range workflows {
		err := store.Create(ctx, wf)
		require.NoError(t, err)
	}

	// Schedule wakes at different times
	_ = store.ScheduleWake(ctx, "wf-1", clock.Now().Add(1*time.Hour))
	_ = store.ScheduleWake(ctx, "wf-2", clock.Now().Add(2*time.Hour))
	_ = store.ScheduleWake(ctx, "wf-3", clock.Now().Add(3*time.Hour))

	// Get wakes due in 2.5 hours
	due, err := store.GetDueWakes(ctx, clock.Now().Add(150*time.Minute))
	require.NoError(t, err)
	require.Len(t, due, 2)
	require.Contains(t, due, InstanceID("wf-1"))
	require.Contains(t, due, InstanceID("wf-2"))
}

func TestSQLiteStore_CompleteWake(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create and schedule wake
	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test",
		Status:       StatusRunning,
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}
	err := store.Create(ctx, info)
	require.NoError(t, err)

	wakeTime := clock.Now().Add(24 * time.Hour)
	err = store.ScheduleWake(ctx, "wf-123", wakeTime)
	require.NoError(t, err)

	// Complete wake
	err = store.CompleteWake(ctx, "wf-123")
	require.NoError(t, err)

	// Verify status is back to running
	retrieved, err := store.Get(ctx, "wf-123")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, retrieved.Status)
	require.Nil(t, retrieved.SleepUntil)

	// Verify no more due wakes
	due, err := store.GetDueWakes(ctx, clock.Now().Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, due, 0)
}

func TestSQLiteStore_ScheduleWake_UpdateExisting(t *testing.T) {
	db, clock := setupTestDB(t)
	store := NewSQLiteStoreWithClock(db, clock)
	ctx := context.Background()

	// Create workflow
	info := WorkflowInfo{
		InstanceID:   "wf-123",
		WorkflowName: "test",
		Status:       StatusRunning,
		CreatedAt:    clock.Now(),
		UpdatedAt:    clock.Now(),
	}
	err := store.Create(ctx, info)
	require.NoError(t, err)

	// First schedule
	wakeTime1 := clock.Now().Add(24 * time.Hour)
	err = store.ScheduleWake(ctx, "wf-123", wakeTime1)
	require.NoError(t, err)

	// Update schedule
	wakeTime2 := clock.Now().Add(12 * time.Hour)
	err = store.ScheduleWake(ctx, "wf-123", wakeTime2)
	require.NoError(t, err)

	// Verify updated time
	retrieved, err := store.Get(ctx, "wf-123")
	require.NoError(t, err)
	require.True(t, retrieved.SleepUntil.Equal(wakeTime2))
}

// ptr helper function
func ptr[T any](v T) *T {
	return &v
}