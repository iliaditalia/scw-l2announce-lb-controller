/*
Copyright 2026 Iliad

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package l2lb

import (
	"context"
	"testing"
)

func countActions(f *fixture, verb string) int {
	n := 0
	for _, a := range f.dyn.Actions() {
		if a.GetVerb() == verb && a.GetResource() == poolGVR {
			n++
		}
	}
	return n
}

func TestEnsurePoolIdempotent(t *testing.T) {
	f := newFixture(t)
	svc := testService()
	ctx := context.Background()

	if err := f.c.ensurePool(ctx, svc, "172.30.192.10/32"); err != nil {
		t.Fatal(err)
	}
	if got := countActions(f, "create"); got != 1 {
		t.Fatalf("expected 1 create, got %d", got)
	}

	// Unchanged spec: no update.
	if err := f.c.ensurePool(ctx, svc, "172.30.192.10/32"); err != nil {
		t.Fatal(err)
	}
	if got := countActions(f, "update"); got != 0 {
		t.Fatalf("expected 0 updates for unchanged spec, got %d", got)
	}

	// Changed CIDR: exactly one update.
	if err := f.c.ensurePool(ctx, svc, "172.30.192.11/32"); err != nil {
		t.Fatal(err)
	}
	if got := countActions(f, "update"); got != 1 {
		t.Fatalf("expected 1 update after CIDR change, got %d", got)
	}
	spec := f.getPool(t)
	blocks := spec["blocks"].([]any)
	if cidr := blocks[0].(map[string]any)["cidr"]; cidr != "172.30.192.11/32" {
		t.Errorf("pool cidr = %v, want 172.30.192.11/32", cidr)
	}
}

func TestDeletePoolIgnoresNotFound(t *testing.T) {
	f := newFixture(t)
	if err := f.c.deletePool(context.Background(), testService()); err != nil {
		t.Fatal(err)
	}
}
