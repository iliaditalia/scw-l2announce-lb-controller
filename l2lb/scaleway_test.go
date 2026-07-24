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
	"testing"

	"github.com/scaleway/scaleway-sdk-go/scw"
)

func TestServerInfoFromProviderID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		wantZone   scw.Zone
		wantID     string
		wantErr    bool
	}{
		{
			name:       "kapsule instance",
			providerID: "scaleway://instance/fr-par-2/e885e8a0-a3e2-4e61-9368-5a31c9d40a96",
			wantZone:   scw.ZoneFrPar2,
			wantID:     "e885e8a0-a3e2-4e61-9368-5a31c9d40a96",
		},
		{
			name:       "baremetal is rejected",
			providerID: "scaleway://baremetal/fr-par-2/some-uuid",
			wantErr:    true,
		},
		{
			name:       "legacy short form is rejected",
			providerID: "scaleway://e885e8a0-a3e2-4e61-9368-5a31c9d40a96",
			wantErr:    true,
		},
		{
			name:       "bad zone",
			providerID: "scaleway://instance/mars-1/some-uuid",
			wantErr:    true,
		},
		{
			name:       "empty",
			providerID: "",
			wantErr:    true,
		},
		{
			name:       "missing uuid",
			providerID: "scaleway://instance/fr-par-2/",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone, id, err := serverInfoFromProviderID(tt.providerID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got zone=%s id=%s", zone, id)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if zone != tt.wantZone || id != tt.wantID {
				t.Errorf("got (%s, %s), want (%s, %s)", zone, id, tt.wantZone, tt.wantID)
			}
		})
	}
}
