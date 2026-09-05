package sync

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

type testProvider struct {
	devices     []DeviceInfo
	deviceErr   error
	drinks      map[string][]DrinkInfo
	drinkErr    map[string]error
	drinksCalls int
}

func (p *testProvider) Code() string { return "test" }
func (p *testProvider) Name() string { return "Test provider" }
func (p *testProvider) Devices(context.Context, model.Manufacturer) ([]DeviceInfo, error) {
	return p.devices, p.deviceErr
}
func (p *testProvider) Drinks(_ context.Context, _ model.Manufacturer, device DeviceInfo) ([]DrinkInfo, error) {
	p.drinksCalls++
	if err := p.drinkErr[device.SerialUnique]; err != nil {
		return nil, err
	}
	return p.drinks[device.SerialUnique], nil
}

func TestSyncManufacturerUpsertsAndRelatesUsingPersistedIDs(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewMemory()
	manufacturer, err := repo.CreateManufacturer(ctx, model.Manufacturer{ID: "maker", Name: "Maker"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &testProvider{
		devices: []DeviceInfo{
			{SerialUnique: "serial-1", DeviceName: "Machine 1"},
			{}, // invalid snapshot must not stop the next device
			{SerialUnique: "serial-2", Name: "Machine 2"},
		},
		drinks: map[string][]DrinkInfo{
			"serial-1": {{OriginID: "coffee", Name: "Coffee", Price: 300}},
			"serial-2": {{OriginID: "tea", Name: "Tea", Price: 250}},
		},
		drinkErr: map[string]error{},
	}

	report, err := NewService(provider, service.NewService(repo)).SyncManufacturer(ctx, manufacturer)
	if err != nil {
		t.Fatal(err)
	}
	if report.Devices != 2 || report.Drinks != 2 || report.Relations != 2 || len(report.Errors) != 1 {
		t.Fatalf("report = %+v", report)
	}
	devices, _ := repo.ListDevices(ctx)
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	for _, device := range devices {
		relations, err := repo.ListDeviceDrinks(ctx, device.ID)
		if err != nil || len(relations) != 1 || relations[0].DeviceID != device.ID || relations[0].DrinkID == "" {
			t.Fatalf("device=%+v relations=%+v err=%v", device, relations, err)
		}
	}
}

func TestSyncManufacturerContinuesAfterDrinkAndRelationFailures(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewMemory()
	manufacturer, _ := repo.CreateManufacturer(ctx, model.Manufacturer{ID: "maker", Name: "Maker"})
	provider := &testProvider{
		devices: []DeviceInfo{{SerialUnique: "serial-1", Name: "Machine"}},
		drinks: map[string][]DrinkInfo{
			"serial-1": {
				{OriginID: "bad", Name: "Bad", Price: -1},
				{OriginID: "good", Name: "Good", Price: 100},
			},
		},
		drinkErr: map[string]error{},
	}
	report, err := NewService(provider, service.NewService(repo)).SyncManufacturer(ctx, manufacturer)
	if err != nil {
		t.Fatal(err)
	}
	if report.Devices != 1 || report.Drinks != 1 || report.Relations != 1 || len(report.Errors) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestSyncManufacturerReturnsDeviceFetchError(t *testing.T) {
	want := errors.New("provider unavailable")
	provider := &testProvider{deviceErr: want}
	report, err := NewService(provider, service.NewService(repository.NewMemory())).SyncManufacturer(context.Background(), model.Manufacturer{ID: "maker"})
	if !errors.Is(err, want) || report.Devices != 0 || report.Errors != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestSyncManufacturerContinuesAfterDrinkFetchError(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewMemory()
	manufacturer, _ := repo.CreateManufacturer(ctx, model.Manufacturer{ID: "maker", Name: "Maker"})
	provider := &testProvider{
		devices: []DeviceInfo{
			{SerialUnique: "failed", Name: "Failed"},
			{SerialUnique: "ok", Name: "Okay"},
		},
		drinks:   map[string][]DrinkInfo{"ok": {{OriginID: "drink", Name: "Drink"}}},
		drinkErr: map[string]error{"failed": errors.New("drink fetch failed")},
	}
	report, err := NewService(provider, service.NewService(repo)).SyncManufacturer(ctx, manufacturer)
	if err != nil || report.Devices != 2 || report.Drinks != 1 || report.Relations != 1 || len(report.Errors) != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestDeviceInfoDomainDeviceNormalizesAndDefaults(t *testing.T) {
	v, err := (DeviceInfo{
		SerialUnique: " serial-1 ",
		DeviceName:   " Front machine ",
	}).DomainDevice(model.Manufacturer{ID: "m1", Code: " btb "})
	if err != nil {
		t.Fatal(err)
	}
	if v.SerialUnique != "serial-1" || v.Name != "Front machine" || v.ManufacturerCode != "btb" || v.Status != model.StatusActive {
		t.Fatalf("normalized device = %#v", v)
	}
}

func TestDeviceInfoDomainDeviceRequiresStableIdentity(t *testing.T) {
	_, err := (DeviceInfo{Name: "Machine"}).DomainDevice(model.Manufacturer{ID: "m1"})
	if err != model.ErrInvalid {
		t.Fatalf("error = %v, want %v", err, model.ErrInvalid)
	}
}

func TestDrinkInfoDomainDrinkUsesNameFallback(t *testing.T) {
	v, err := (DrinkInfo{
		OriginID:   " origin-1 ",
		ProductNum: " product-1 ",
		Price:      350,
	}).DomainDrink()
	if err != nil {
		t.Fatal(err)
	}
	if v.OriginID != "origin-1" || v.Name != "product-1" || v.Status != model.StatusActive {
		t.Fatalf("normalized drink = %#v", v)
	}
}

func TestDrinkInfoDomainDrinkRejectsInvalidSnapshot(t *testing.T) {
	for _, v := range []DrinkInfo{
		{OriginID: "origin-1", Name: "Latte", Price: -1},
		{OriginID: "", Name: "Latte"},
		{OriginID: "origin-1"},
		{OriginID: "origin-1", Name: "Latte", Status: "unknown"},
	} {
		if _, err := v.DomainDrink(); err != model.ErrInvalid {
			t.Fatalf("snapshot %#v error = %v, want %v", v, err, model.ErrInvalid)
		}
	}
}

func TestSchedulerRunsJobsImmediatelyAndOnIntervals(t *testing.T) {
	var first, second atomic.Int32
	firstRun := make(chan struct{}, 1)
	secondRun := make(chan struct{}, 1)
	scheduler := NewScheduler(SchedulerConfig{Jobs: []Job{
		{Interval: 20 * time.Millisecond, Run: func(context.Context) error {
			first.Add(1)
			select {
			case firstRun <- struct{}{}:
			default:
			}
			return nil
		}},
		{Interval: 30 * time.Millisecond, Run: func(context.Context) error {
			second.Add(1)
			select {
			case secondRun <- struct{}{}:
			default:
			}
			return nil
		}},
	}})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	select {
	case <-firstRun:
	case <-time.After(time.Second):
		t.Fatal("first job did not run immediately")
	}
	select {
	case <-secondRun:
	case <-time.After(time.Second):
		t.Fatal("second job did not run immediately")
	}
	deadline := time.Now().Add(time.Second)
	for (first.Load() < 2 || second.Load() < 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if first.Load() < 2 || second.Load() < 2 {
		t.Fatalf("jobs did not tick: first=%d second=%d", first.Load(), second.Load())
	}
}

func TestSchedulerDoesNotOverlapRunsForOneJob(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	scheduler := NewScheduler(SchedulerConfig{Jobs: []Job{{Interval: time.Millisecond, Run: func(context.Context) error {
		current := active.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		case <-release:
		}
		<-release
		active.Add(-1)
		return nil
	}}}})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job did not start")
	}
	time.Sleep(15 * time.Millisecond)
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent runs = %d, want 1", got)
	}
	close(release)
	scheduler.Stop()
}

func TestSchedulerStopWaitsAndErrorsAreIsolated(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var healthy atomic.Int32
	scheduler := NewScheduler(SchedulerConfig{Jobs: []Job{
		{Interval: time.Hour, Run: func(context.Context) error { started <- struct{}{}; <-release; return errors.New("expected failure") }},
		{Interval: time.Millisecond, Run: func(context.Context) error { healthy.Add(1); return nil }},
	}})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking job did not start")
	}
	deadline := time.Now().Add(time.Second)
	for healthy.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if healthy.Load() == 0 {
		t.Fatal("healthy job did not run")
	}
	stopped := make(chan struct{})
	go func() { scheduler.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned before in-flight job completed")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestSchedulerContinuesAfterJobPanic(t *testing.T) {
	started := make(chan struct{}, 1)
	var healthy atomic.Int32
	scheduler := NewScheduler(SchedulerConfig{Jobs: []Job{
		{Interval: time.Hour, Run: func(context.Context) error {
			started <- struct{}{}
			panic("provider panic")
		}},
		{Interval: time.Millisecond, Run: func(context.Context) error {
			healthy.Add(1)
			return nil
		}},
	}})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("panicking job did not start")
	}
	deadline := time.Now().Add(time.Second)
	for healthy.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if healthy.Load() == 0 {
		t.Fatal("healthy job did not run after panic")
	}
}

func TestSchedulerStopContextReturnsDeadlineForStuckJob(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	scheduler := NewScheduler(SchedulerConfig{Jobs: []Job{{
		Interval: time.Hour,
		Run: func(context.Context) error {
			close(started)
			<-release
			return nil
		},
	}}})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := scheduler.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext() error = %v, want deadline exceeded", err)
	}
	close(release)
	if err := scheduler.StopContext(context.Background()); err != nil {
		t.Fatalf("StopContext() after release error = %v", err)
	}
}

func TestSchedulerRejectsInvalidJobs(t *testing.T) {
	for name, config := range map[string]SchedulerConfig{
		"invalid interval": {Jobs: []Job{{Run: func(context.Context) error { return nil }}}},
		"missing run":      {Jobs: []Job{{Interval: time.Second}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := NewScheduler(config).Start(context.Background()); err == nil {
				t.Fatal("Start succeeded")
			}
		})
	}
}
