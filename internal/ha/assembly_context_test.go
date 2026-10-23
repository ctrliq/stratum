package ha

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestNodeAssemblyContextStopsAfterInflightStep(t *testing.T) {
	steps := []struct{ field, name string }{
		{"HostDirs", "host directories"},
		{"Prereqs", "prerequisites"},
		{"ResolveNet", "resolve net"},
		{"TrustPodCIDRs", "trust pod cidrs"},
		{"ResolveNodeIP", "resolve node ip"},
		{"BeforeCerts", "before certificates"},
		{"IssueCerts", "issue certificates"},
		{"WriteKubeconfigs", "write kubeconfigs"},
		{"WriteProfileConfigs", "profile configs"},
		{"BeforeRuntimeConfigs", "before runtime configs"},
		{"WriteRuntimeConfigs", "runtime configs"},
	}
	var order []string
	for _, step := range steps {
		order = append(order, step.name)
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &assemblyRecord{}
			a, req := recordingAssembly(r)
			req.PrereqOptions.Profile = profiletest.Profile()
			// Finish the selected atomic effect, then deliver cancellation
			// before assembly can start a later mutation.
			field := reflect.ValueOf(&a).Elem().FieldByName(step.field)
			original := reflect.ValueOf(field.Interface())
			field.Set(reflect.MakeFunc(field.Type(), func(args []reflect.Value) []reflect.Value {
				results := original.Call(args)
				cancel()
				return results
			}))
			if _, err := AssembleNodeContext(ctx, a, req); !errors.Is(err, context.Canceled) {
				t.Fatalf("assembly error = %v, want cancellation", err)
			}
			if !slices.Equal(r.order, order[:i+1]) {
				t.Fatalf("steps after cancellation: got %v, want %v", r.order, order[:i+1])
			}
		})
	}
}

func TestNodeAssemblyContextPreservesInflightError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boom := errors.New("certificate storage failed")
	r := &assemblyRecord{}
	a, req := recordingAssembly(r)
	a.IssueCerts = func(string, string, *pki.APISANs) error {
		cancel()
		return boom
	}
	if _, err := AssembleNodeContext(ctx, a, req); err != boom {
		t.Fatalf("in-flight failure became %v, want %v", err, boom)
	}
	if slices.Contains(r.order, "write kubeconfigs") {
		t.Fatal("kubeconfigs were written after the certificate failure")
	}
}

func TestNodeAssemblyContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &assemblyRecord{}
	a, req := recordingAssembly(r)
	if _, err := AssembleNodeContext(ctx, a, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("assembly error = %v, want cancellation", err)
	}
	if len(r.order) != 0 {
		t.Fatalf("already cancelled assembly ran %v", r.order)
	}
}
