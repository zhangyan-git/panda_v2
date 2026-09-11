package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// sumFor 把某个计数器在当前快照里的所有数据点加起来。
func sumFor(t *testing.T, collected metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	var total int64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data = %T, want an int64 sum", name, m.Data)
			}
			for _, point := range sum.DataPoints {
				total += point.Value
			}
		}
	}
	return total
}

// 组件在 observability.Init 之前就构造是常态（casbin.Broadcaster 在 main 里先
// 于 server 建好），所以 Counter 必须在那时候也能建、也能 Add。这里锁住的是
// 「早建不报错、建完就能用」这一半；「provider 就位后委派过去」是 OTel 全局
// meter 自己的保证，不在这里重复实现。
func TestCounterIsUsableBeforeAnyProviderIsSet(t *testing.T) {
	counter := Counter("panda.test.early", "created before a provider exists")
	counter.Add(context.Background(), 1)
}

// 建了 provider 之后，同一个名字的计数器必须真的把数据交给它。
func TestCounterRecordsIntoTheConfiguredProvider(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	counter := Counter("panda.test.recorded", "records into the configured provider")
	counter.Add(context.Background(), 3, metric.WithAttributes(attribute.String("event_type", "admin.operation.logged")))

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	sum := sumFor(t, collected, "panda.test.recorded")
	if sum != 3 {
		t.Fatalf("recorded value = %v, want 3", sum)
	}
}
