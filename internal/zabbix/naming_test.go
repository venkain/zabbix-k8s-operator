/*
Copyright 2026.

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

package zabbix

import (
	"testing"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

func TestNames(t *testing.T) {
	cases := []struct {
		instance, wantServer, wantFrontend, wantWebService string
	}{
		{"test-zabbix", "test-zabbix-server", "test-zabbix-frontend", "test-zabbix-web-service"},
	}
	for _, tc := range cases {
		if got := ServerName(tc.instance); got != tc.wantServer {
			t.Errorf("ServerName(%q) = %q, want %q", tc.instance, got, tc.wantServer)
		}
		if got := FrontendName(tc.instance); got != tc.wantFrontend {
			t.Errorf("FrontendName(%q) = %q, want %q", tc.instance, got, tc.wantFrontend)
		}
		if got := WebServiceName(tc.instance); got != tc.wantWebService {
			t.Errorf("WebServiceName(%q) = %q, want %q", tc.instance, got, tc.wantWebService)
		}
	}
}

func TestImageFor(t *testing.T) {
	cases := []struct {
		name      string
		component string
		version   string
		override  *zabbixv1alpha1.ImageSpec
		want      string
	}{
		{
			name:      "release line uses -latest tag",
			component: "server-pgsql",
			version:   "8.0",
			want:      "zabbix/zabbix-server-pgsql:ubuntu-8.0-latest",
		},
		{
			name:      "point release pins the exact tag",
			component: "server-pgsql",
			version:   "8.0.2",
			want:      "zabbix/zabbix-server-pgsql:ubuntu-8.0.2",
		},
		{
			name:      "7.0 LTS release line",
			component: "web-nginx-pgsql",
			version:   "7.0",
			want:      "zabbix/zabbix-web-nginx-pgsql:ubuntu-7.0-latest",
		},
		{
			name:      "full override",
			component: "server-pgsql",
			version:   "8.0",
			override: &zabbixv1alpha1.ImageSpec{
				Repository: "registry.example.com/mirror/zabbix-server-pgsql",
				Tag:        "v1-custom",
			},
			want: "registry.example.com/mirror/zabbix-server-pgsql:v1-custom",
		},
		{
			name:      "tag-only override keeps default repo",
			component: "agent2",
			version:   "8.0",
			override:  &zabbixv1alpha1.ImageSpec{Tag: "ubuntu-8.0.1"},
			want:      "zabbix/zabbix-agent2:ubuntu-8.0.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageFor(tc.component, tc.version, tc.override); got != tc.want {
				t.Errorf("ImageFor(%q, %q) = %q, want %q", tc.component, tc.version, got, tc.want)
			}
		})
	}
}

func TestSelectorLabelsAreSubsetOfPodLabels(t *testing.T) {
	common := commonLabels("inst", ComponentServer)
	sel := selectorLabels("inst", ComponentServer)
	for k, v := range sel {
		if common[k] != v {
			t.Errorf("selector label %s=%q not present in common labels (got %q)", k, v, common[k])
		}
	}
}
